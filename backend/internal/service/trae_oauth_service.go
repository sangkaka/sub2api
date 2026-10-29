package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
)

// TraeOAuthService 承载 TRAE 平台的 OAuth 语义：ExchangeToken 兑换/刷新、
// 凭据组装、Remote common_params 身份字段组装。账号形态 type=oauth，
// credentials = {token, refresh_token, user_id, client_id, expires_at,
// provider_specific{...}}。
type TraeOAuthService struct {
	accountRepo AccountRepository
	proxyRepo   ProxyRepository
	// client 可注入替换（测试）；nil 时用默认端点。
	client *trae.Client
}

func NewTraeOAuthService(accountRepo AccountRepository, proxyRepo ProxyRepository) *TraeOAuthService {
	return &TraeOAuthService{accountRepo: accountRepo, proxyRepo: proxyRepo}
}

// SetClient 覆盖上游客户端（测试/自定义端点）。
func (s *TraeOAuthService) SetClient(client *trae.Client) { s.client = client }

func (s *TraeOAuthService) upstreamClient() *trae.Client {
	if s.client != nil {
		return s.client
	}
	return &trae.Client{}
}

// UpstreamClient 返回带账号代理的上游客户端：保留注入客户端的端点覆盖
// （测试用），按 account.ProxyID 解析出站代理（与 grok/kiro 同语义，账号
// 未配代理则直连）。
func (s *TraeOAuthService) UpstreamClient(ctx context.Context, account *Account) *trae.Client {
	base := s.upstreamClient()
	out := &trae.Client{
		APIHost:     base.APIHost,
		RemoteBase:  base.RemoteBase,
		WebOrigin:   base.WebOrigin,
		CloudIDEHost: firstNonEmptyString(traeAccountAuthHost(account), base.CloudIDEHost),
		HTTP:        base.HTTP,
	}
	if out.HTTP == nil {
		out.ProxyURL = s.resolveAccountProxyURL(ctx, account)
	}
	return out
}

// traeAccountAuthHost 读取账号凭据 auth_host：签发/刷新该账号凭据的
// Cloud-IDE 控制台域名。企业版（ToB 租户）账号为
// console.enterprise.trae.cn——其 rt 在默认 api.trae.cn 兑换会被拒
// （"refresh token is invalid"），GetUserInfo 也报 20315 "User Not Found"；
// 个人版留空走默认。作用于 Cloud-IDE v3 身份端点（ExchangeToken /
// GetUserInfo）；/trae/api/v2/*（积分/签到）对两种 token 均在默认域名可用。
func traeAccountAuthHost(account *Account) string {
	if account == nil {
		return ""
	}
	return strings.TrimRight(strings.TrimSpace(account.GetCredential("auth_host")), "/")
}

func (s *TraeOAuthService) resolveAccountProxyURL(ctx context.Context, account *Account) string {
	if s.proxyRepo == nil || account == nil || account.ProxyID == nil {
		return ""
	}
	proxy, err := s.proxyRepo.GetByID(ctx, *account.ProxyID)
	if err != nil || proxy == nil {
		return ""
	}
	return proxy.URL()
}

// ExchangeCredentials 仅凭 refresh_token（+可选 user_id）兑换完整凭据。
// 「粘贴 refresh_token 建号」与账号探活共用此入口：兑换失败即建号失败，
// 不落半截凭据。
func (s *TraeOAuthService) ExchangeCredentials(ctx context.Context, refreshToken, userID string) (map[string]any, error) {
	result, err := s.upstreamClient().ExchangeToken(ctx, refreshToken, userID)
	if err != nil {
		return nil, err
	}
	return BuildTraeCredentials(result), nil
}

// RefreshAccountToken 刷新账号 token 并做轮换校验：交换期间凭据可能被并发
// 刷新替换，落库前必须确认请求时所用的 refresh_token 仍是账号当前值。
func (s *TraeOAuthService) RefreshAccountToken(ctx context.Context, account *Account) (*trae.ExchangeTokenResult, error) {
	if account == nil || account.Platform != PlatformTrae || account.Type != AccountTypeOAuth {
		return nil, fmt.Errorf("not a trae oauth account")
	}
	refreshToken := account.GetCredential("refresh_token")
	if refreshToken == "" {
		return nil, fmt.Errorf("trae account %d has no refresh_token", account.ID)
	}
	userID := account.GetCredential("user_id")
	result, err := s.UpstreamClient(ctx, account).ExchangeToken(ctx, refreshToken, userID)
	if err != nil {
		return nil, err
	}
	// 轮换竞态校验：以仓库中的最新凭据为准（而非调用方内存里的旧副本）。
	if s.accountRepo != nil {
		if latest, rerr := s.accountRepo.GetByID(ctx, account.ID); rerr == nil && latest != nil {
			if verr := trae.ValidateRefreshRotation(refreshToken, latest.GetCredential("refresh_token")); verr != nil {
				return nil, verr
			}
		}
	}
	return result, nil
}

// BuildTraeCredentials 把 ExchangeToken 结果组装为账号 credentials。
// provider_specific 携带 Remote common_params 所需身份字段，缺失时运行期
// 用默认值兜底。
func BuildTraeCredentials(result *trae.ExchangeTokenResult) map[string]any {
	if result == nil {
		return map[string]any{}
	}
	creds := map[string]any{
		"token":         result.Token,
		"refresh_token": result.RefreshToken,
		"client_id":     trae.PublicClientID,
	}
	if result.UserInfo.UserID != "" {
		creds["user_id"] = result.UserInfo.UserID
	}
	if expiresAt, ok := trae.NormalizeExpireAt(string(result.TokenExpireAt)); ok {
		creds["expires_at"] = expiresAt.Format(time.RFC3339)
	} else if exp, ok := trae.JWTExp(result.Token); ok {
		// 企业控制台的兑换响应可能不带 TokenExpireAt，回退读 JWT 的 exp。
		creds["expires_at"] = exp.Format(time.RFC3339)
	}
	if refreshExpiresAt, ok := trae.NormalizeExpireAt(string(result.RefreshExpireAt)); ok {
		creds["refresh_expires_at"] = refreshExpiresAt.Format(time.RFC3339)
	}
	providerSpecific := map[string]any{}
	info := result.UserInfo
	if info.WebID != "" {
		providerSpecific["webId"] = info.WebID
	}
	if info.BizUserID != "" {
		providerSpecific["bizUserId"] = info.BizUserID
	}
	if info.UserUniqueID != "" {
		providerSpecific["userUniqueId"] = info.UserUniqueID
	}
	if info.Tenant != "" {
		providerSpecific["tenant"] = info.Tenant
	}
	if info.Region != "" {
		providerSpecific["region"] = info.Region
	}
	if info.AIRegion != "" {
		providerSpecific["aiRegion"] = info.AIRegion
	}
	creds["provider_specific"] = providerSpecific
	return creds
}

// BuildAccountCredentials 把刷新结果组装为可合并的 credentials（保留原凭据
// 其余字段由 MergeCredentials 处理）。
func (s *TraeOAuthService) BuildAccountCredentials(result *trae.ExchangeTokenResult) map[string]any {
	return BuildTraeCredentials(result)
}

// TraeProviderSpecific 读取账号的 provider_specific 身份字段（缺失容忍）。
func TraeProviderSpecific(account *Account) map[string]any {
	if account == nil {
		return map[string]any{}
	}
	switch v := account.Credentials["provider_specific"].(type) {
	case map[string]any:
		return v
	case string:
		parsed := map[string]any{}
		if err := json.Unmarshal([]byte(v), &parsed); err == nil {
			return parsed
		}
	}
	return map[string]any{}
}

// BuildTraeCommonParams 组装 Remote 会话的 common_params JSON 字符串：
// 身份字段（provider_specific 优先、CN 默认值兜底）+ 稳定派生的设备标识 +
// biz_session_id（账号+模型派生，上游按它复用会话上下文）。
func BuildTraeCommonParams(account *Account, model string) string {
	psd := TraeProviderSpecific(account)
	accountID := account.GetCredential("user_id")
	deviceID := TraeDeviceID(accountID)
	seed := accountID + "\x1f" + deviceID + "\x1f" + model
	bizSession := hex.EncodeToString(sha256Sum(seed))[:32]
	params := map[string]any{
		"language":        "zh-cn",
		"app_language":    psdString(psd, "appLanguage", "zh-CN"),
		"quality":         "stable",
		"app_version":     psdString(psd, "appVersion", "1.0.0.1229"),
		"web_id":          psdString(psd, "webId", ""),
		"user_identity":   psdString(psd, "userIdentity", "Free"),
		"is_freshman":     "0",
		"biz_user_id":     psdString(psd, "bizUserId", ""),
		"user_unique_id":  psdString(psd, "userUniqueId", ""),
		"scope":           psdString(psd, "scope", "marscode-cn"),
		"tenant":          psdString(psd, "tenant", "marscode"),
		"region":          psdString(psd, "region", "cn"),
		"aiRegion":        psdString(psd, "aiRegion", psdString(psd, "region", "cn")),
		"is_privacy_mode": 0,
		"privacy_mode":    "off",
		"solo_chat_mode":  "code",
		"biz_session_id":  bizSession,
		"device_id":       deviceID,
		"machine_id":      hex.EncodeToString(sha256Sum("traework-linux-probe\x1f" + deviceID)),
		"local_device_id": "aha-" + hex.EncodeToString(sha256Sum("local-" + accountID + "-" + deviceID))[:32],
	}
	payload, err := json.Marshal(params)
	if err != nil {
		return "{}"
	}
	return string(payload)
}

// TraeDeviceID 派生每账号稳定的 16 位数字设备标识（签到链路同款派生；
// sha256("trae"+accountID) 取前 32 hex 转 10^16 取模）。
func TraeDeviceID(accountID string) string {
	digest := hex.EncodeToString(sha256Sum("trae" + accountID))
	// 256-bit 摘要超出 uint64，用大数取模（与 trae2api 派生语义一致）。
	value, ok := new(big.Int).SetString(digest, 16)
	if !ok {
		return "0000000000000000"
	}
	mod := new(big.Int).Mod(value, big.NewInt(1e16))
	return fmt.Sprintf("%016s", mod.String())
}

func psdString(psd map[string]any, key, fallback string) string {
	if v, ok := psd[key].(string); ok && v != "" {
		return v
	}
	return fallback
}

func sha256Sum(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}
