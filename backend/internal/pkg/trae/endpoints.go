// Package trae 实现 Trae CN 上游私有协议客户端与 OpenAI Chat Completions
// 双向翻译：Cloud-IDE-JWT OAuth（ExchangeToken 刷新）、Remote chat_sessions
// 两步会话（POST /chat_sessions + GET /chat_sessions/{id}/events）、模型目录
// 与 entitlement 拉取。协议事实来自 Trae2api-cn 逆向（生产验证），端点与
// 头组集中在本包，上游变动的影响面收敛于此。
package trae

import "time"

// 上游端点与固定常量。APIHost 是 Cloud-IDE（OAuth/用户信息/entitlement）域名，
// RemoteBase 是 Remote 会话协议基址；均可经 ClientOptions 覆盖（自建网关/域名
// 变动时无需改代码）。
const (
	// DefaultAPIHost Cloud-IDE API 域名（OAuth ExchangeToken / GetUserInfo /
	// ide_user_ent_usage / checkin_credits）。
	DefaultAPIHost = "https://api.trae.cn"
	// DefaultRemoteBase Remote 会话协议基址（chat_sessions / events / models）。
	DefaultRemoteBase = "https://trae-api-cn.mchost.guru/api/remote/v1"
	// DefaultWebOrigin 浏览器 Origin/Referer，上游按它识别 Solo Web 客户端。
	DefaultWebOrigin = "https://solo.trae.cn"

	// PublicClientID 是 Trae IDE 的公开 OAuth client id；ClientSecret 为占位
	// 值 "-"（原生客户端不校验）。
	PublicClientID    = "ono9krqynydwx5"
	placeholderSecret = "-"

	exchangeTokenPath = "/cloudide/api/v3/trae/oauth/ExchangeToken"
	getUserInfoPath   = "/cloudide/api/v3/trae/GetUserInfo"
	entUsagePath      = "/trae/api/v2/pay/ide_user_ent_usage"

	modelsPath    = "/models"
	sessionsPath  = "/chat_sessions"
	eventsPathFmt = "/chat_sessions/%s/events"
	stopPathFmt   = "/chat_sessions/%s/stop"

	// agentTypeAgent / agentTypeWork 是 Remote 会话的执行器档位；agent_id 与
	// agent_type 同值钉住，缺省会被上游静默换成其默认模型。
	agentTypeAgent = "solo_agent_remote"
	agentTypeWork  = "solo_work_remote"

	// RemoteSessionModeAgent 是 Remote 会话的 mode 字段（code 会话）。
	RemoteSessionModeAgent = "code"

	// UserAgentChrome 与 Solo Web 一致的桌面 Chrome UA。
	UserAgentChrome = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"

	// FirstEventTimeout 建立 events SSE 后等待首个模型事件的超时；超时视为
	// 会话建立但无输出（上游偶发静默），首模型事件前允许换号重试。
	FirstEventTimeout = 120 * time.Second
)
