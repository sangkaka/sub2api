package service

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adobe"
)

// adobeClientIdleTTL 是缓存条目闲置多久后被回收（账号删除、长期不用）。
const adobeClientIdleTTL = 30 * time.Minute

// adobeClientSweepInterval 限制惰性清理的频率，避免每次取客户端都遍历整张表。
const adobeClientSweepInterval = time.Minute

// adobeClientCache 按账号（×计费通道）缓存 *adobe.Client。
//
// Client 内部持有带 TLS 指纹的连接池，每请求新建会白白丢掉连接复用，也会让
// 指纹握手的开销叠加到每一次出图上。tlsclient 的 CookieJar 是客户端级的：
// IMS Set-Cookie 会写进 jar，多账号共用一个 Client 会串 ims_sid。
//
// 每个账号每条通道只保留一个条目：firefly（免费）与 express（付费积分）的
// Origin / x-api-key 不同，是两个独立身份，不能共用 Client。代理或 cookie 变化时
// 整个 Client 重建，旧 jar 里的会话 cookie 不会跟着新 cookie 一起发出去；闲置超过
// adobeClientIdleTTL 的条目被惰性回收。
type adobeClientCache struct {
	mu        sync.Mutex
	clients   map[adobeClientKey]*adobeClientEntry
	lastSweep time.Time
	// newClient 为空时用真实构造函数；单测替换它来注入假传输，避免打真网络。
	// 测试替身不区分通道：fake 只关心传输注入，身份参数对断言不可见。
	newClient func(proxyURL string) *adobe.Client
	// newClientWithLane 同 newClient 但携带计费通道；非空时优先——需要区分
	// firefly / express 身份的测试用这个钩子。
	newClientWithLane func(proxyURL string, lane AdobeLane) *adobe.Client
}

// adobeClientKey 是缓存键：账号 × 计费通道。
type adobeClientKey struct {
	accountID int64
	lane      AdobeLane
}

type adobeClientEntry struct {
	client      *adobe.Client
	fingerprint string
	lastUsed    time.Time
}

// adobeClientFingerprint 标识一个 Client 可复用的前提：同一代理、同一份 cookie、
// 同一计费通道。cookie 只存摘要，缓存里不留明文凭据。
func adobeClientFingerprint(account *Account, proxyURL string, lane AdobeLane) string {
	sum := sha256.Sum256([]byte(account.GetCredential("cookie")))
	return proxyURL + "\x00" + hex.EncodeToString(sum[:]) + "\x00" + lane.String()
}

func (c *adobeClientCache) build(proxyURL string, lane AdobeLane) *adobe.Client {
	if c.newClientWithLane != nil {
		return c.newClientWithLane(proxyURL, lane)
	}
	if c.newClient != nil {
		return c.newClient(proxyURL)
	}
	cfg := adobe.ClientConfig{ProxyURL: proxyURL}
	if lane.IsExpress() {
		// 零值字段（UA/TLS/CreditsAPIKey 等）由 NewClient 用 DefaultIdentity 补齐。
		cfg.Identity = adobe.ExpressIdentity
	}
	return adobe.NewClient(cfg)
}

// clientForAccount 返回该账号对应的 Firefly（免费通道）客户端。
func (c *adobeClientCache) clientForAccount(account *Account) *adobe.Client {
	return c.clientForAccountLane(account, AdobeLaneFirefly)
}

// clientForAccountLane 返回该账号指定计费通道的客户端（跟随账号的代理配置）。
// 账号为 nil 或尚未落库（ID<=0）时不缓存，每次新建，避免按代理地址在账号之间共用 jar。
func (c *adobeClientCache) clientForAccountLane(account *Account, lane AdobeLane) *adobe.Client {
	proxyURL := accountProxyURL(account)
	if account == nil || account.ID <= 0 {
		return c.build(proxyURL, lane)
	}

	key := adobeClientKey{accountID: account.ID, lane: lane}
	fingerprint := adobeClientFingerprint(account, proxyURL, lane)
	now := time.Now()

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.clients == nil {
		c.clients = make(map[adobeClientKey]*adobeClientEntry)
	}
	c.sweepLocked(now)
	if entry, ok := c.clients[key]; ok && entry.fingerprint == fingerprint {
		entry.lastUsed = now
		return entry.client
	}
	client := c.build(proxyURL, lane)
	c.clients[key] = &adobeClientEntry{client: client, fingerprint: fingerprint, lastUsed: now}
	return client
}

// sweepLocked 回收闲置条目；调用方持有 c.mu。
func (c *adobeClientCache) sweepLocked(now time.Time) {
	if now.Sub(c.lastSweep) < adobeClientSweepInterval {
		return
	}
	c.lastSweep = now
	for key, entry := range c.clients {
		if now.Sub(entry.lastUsed) > adobeClientIdleTTL {
			delete(c.clients, key)
		}
	}
}
