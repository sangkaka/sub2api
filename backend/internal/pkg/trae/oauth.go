package trae

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client 是 Trae 上游 HTTP 客户端。零值可用（默认端点 + 默认 http.Client）；
// 端点可覆盖以适配镜像/自定义网关。
type Client struct {
	// APIHost 覆盖 Cloud-IDE API 域名（默认 DefaultAPIHost）。
	APIHost string
	// CloudIDEHost 覆盖 Cloud-IDE v3 身份端点（ExchangeToken / GetUserInfo）
	// 的域名。企业版账号（ToB 租户）的凭据由 console.enterprise.trae.cn 签发，
	// 其 rt 只能在该域名兑换，且默认域名的 GetUserInfo 会报 20315
	// "User Not Found"；个人版留空（随 APIHost）。/trae/api/v2/*（积分/
	// 签到）对两种签发的 token 均在默认域名可用，不受此覆盖。
	CloudIDEHost string
	// RemoteBase 覆盖 Remote 会话协议基址（默认 DefaultRemoteBase）。
	RemoteBase string
	// WebOrigin 覆盖浏览器 Origin/Referer（默认 DefaultWebOrigin）。
	WebOrigin string
	// ProxyURL 是出站代理地址（http/https/socks5）；空串直连。
	ProxyURL string
	// HTTP 用于注入代理/超时；nil 时按 ProxyURL 构建默认客户端。
	HTTP *http.Client
}

func (c *Client) apiHost() string {
	if v := strings.TrimSpace(c.APIHost); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultAPIHost
}

func (c *Client) cloudIDEHost() string {
	if v := strings.TrimSpace(c.CloudIDEHost); v != "" {
		return strings.TrimRight(v, "/")
	}
	return c.apiHost()
}

func (c *Client) remoteBase() string {
	if v := strings.TrimSpace(c.RemoteBase); v != "" {
		return strings.TrimRight(v, "/")
	}
	return DefaultRemoteBase
}

func (c *Client) webOrigin() string {
	if v := strings.TrimSpace(c.WebOrigin); v != "" {
		return v
	}
	return DefaultWebOrigin
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	if proxyURL := strings.TrimSpace(c.ProxyURL); proxyURL != "" {
		if parsed, err := url.Parse(proxyURL); err == nil {
			transport := http.DefaultTransport.(*http.Transport).Clone()
			transport.Proxy = http.ProxyURL(parsed)
			return &http.Client{Transport: transport, Timeout: 60 * time.Second}
		}
	}
	return &http.Client{Timeout: 60 * time.Second}
}

// ExchangeTokenRequest 是 ExchangeToken 的请求体。UserID 允许为空（老授权
// 流程仅凭 refresh_token 也能兑换，响应会补全 UserInfo）。
type ExchangeTokenRequest struct {
	ClientID     string `json:"ClientID"`
	RefreshToken string `json:"RefreshToken"`
	ClientSecret string `json:"ClientSecret"`
	UserID       string `json:"UserID"`
}

// ExchangeTokenResult 是 ExchangeToken 响应的载荷。标准个人版端点包在
// Result 里；企业控制台（console.enterprise.trae.cn）包在 Data 里且时间戳
// 为数字毫秒（flexTimestamp 兼容两种形态）。
type ExchangeTokenResult struct {
	Token           string        `json:"Token"`
	RefreshToken    string        `json:"RefreshToken"`
	TokenExpireAt   flexTimestamp `json:"TokenExpireAt"`
	RefreshExpireAt flexTimestamp `json:"RefreshExpireAt"`
	UserInfo        TraeUserInfo  `json:"UserInfo"`
}

// flexTimestamp 兼容字符串与数字（毫秒/秒时间戳）两种 JSON 形态，统一为
// NormalizeExpireAt 可解析的字符串。
type flexTimestamp string

func (f *flexTimestamp) UnmarshalJSON(data []byte) error {
	text := strings.TrimSpace(string(data))
	text = strings.Trim(text, `"`)
	if text == "" || text == "null" {
		*f = ""
		return nil
	}
	if n, err := strconv.ParseFloat(text, 64); err == nil {
		*f = flexTimestamp(strconv.FormatInt(int64(n), 10))
		return nil
	}
	*f = flexTimestamp(text)
	return nil
}

// TraeUserInfo 是上游用户身份信息（ExchangeToken 响应 / GetUserInfo 共享形状）。
// 字段进入 credentials 的 provider_specific，Remote common_params 依赖它们。
type TraeUserInfo struct {
	UserID       string `json:"UserID"`
	Region       string `json:"Region"`
	AIRegion     string `json:"AIRegion"`
	WebID        string `json:"WebId"`
	BizUserID    string `json:"BizUserId"`
	UserUniqueID string `json:"UserUniqueId"`
	Tenant       string `json:"Tenant"`
	ScreenName   string `json:"ScreenName"`
}

type exchangeTokenResponse struct {
	Result *ExchangeTokenResult `json:"Result"`
	// Data 是企业控制台的载荷包装（与 Result 二选一）。
	Data *ExchangeTokenResult `json:"Data"`
	// Code/Message 是企业控制台的业务码：0 成功，非 0 失败。
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ExchangeToken 用 refresh_token 兑换新的 Cloud-IDE-JWT。refresh_token 每次
// 刷新轮换：调用方落库前必须校验请求时所用的 refresh_token 仍为账号当前值
// （见 ValidateRefreshRotation），防止并发刷新把旧响应写回覆盖新凭据。
func (c *Client) ExchangeToken(ctx context.Context, refreshToken, userID string) (*ExchangeTokenResult, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil, fmt.Errorf("trae: refresh token is empty")
	}
	clientID := PublicClientID
	body, err := json.Marshal(ExchangeTokenRequest{
		ClientID:     clientID,
		RefreshToken: refreshToken,
		ClientSecret: placeholderSecret,
		UserID:       strings.TrimSpace(userID),
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cloudIDEHost()+exchangeTokenPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: exchange token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("trae: exchange token read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			Kind:    ErrorKindAuth,
			Code:    strconv.Itoa(resp.StatusCode),
			Message: fmt.Sprintf("ExchangeToken [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var parsed exchangeTokenResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("trae: exchange token parse response: %w", err)
	}
	payload := parsed.Result
	if payload == nil {
		payload = parsed.Data
	}
	if payload == nil || payload.Token == "" {
		if parsed.Code != 0 || parsed.Message != "" {
			return nil, &UpstreamError{
				Kind:    ErrorKindAuth,
				Code:    strconv.Itoa(parsed.Code),
				Message: fmt.Sprintf("ExchangeToken code %d: %s", parsed.Code, firstNonEmpty(parsed.Message, truncate(string(raw), 300))),
			}
		}
		return nil, &UpstreamError{
			Kind:    ErrorKindAuth,
			Message: fmt.Sprintf("ExchangeToken returned no token: %s", truncate(string(raw), 300)),
		}
	}
	return payload, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// GetUserInfo 拉取上游用户信息。注意该端点鉴权头是 x-cloudide-token，
// Authorization: Cloud-IDE-JWT 反而被拒（401）。
func (c *Client) GetUserInfo(ctx context.Context, token string) (*TraeUserInfo, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("trae: token is empty")
	}
	body, _ := json.Marshal(map[string]any{"ReqSource": "IDE"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cloudIDEHost()+getUserInfoPath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-cloudide-token", token)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: get user info: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("trae: get user info read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			Kind:    ErrorKindAuth,
			Code:    strconv.Itoa(resp.StatusCode),
			Message: fmt.Sprintf("GetUserInfo [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var parsed struct {
		Result *TraeUserInfo `json:"Result"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("trae: get user info parse response: %w", err)
	}
	if parsed.Result == nil || parsed.Result.UserID == "" {
		return nil, fmt.Errorf("trae: get user info: empty Result")
	}
	return parsed.Result, nil
}

// EntitlementUsage 是 ide_user_ent_usage 的解析结果：积分包与账号并发上限。
type EntitlementUsage struct {
	// ParallelLimit 是 entitlement quota.solo_agent_parallel_limit（账号并发
	// 槽位，免费号实测 2；未知时调用方应保守取 2）。
	ParallelLimit int
	// CreditsTotal / CreditsUsed 是 product_type=2 积分包的汇总。
	CreditsTotal float64
	CreditsUsed  float64
}

type entUsageResponse struct {
	Data struct {
		Quota struct {
			ParallelLimit int `json:"solo_agent_parallel_limit"`
		} `json:"quota"`
		UserEntitlementPackList []struct {
			ProductType int `json:"product_type"`
			CreditsInfo struct {
				CreditsLimit float64 `json:"credits_limit"`
				CreditsUsed  float64 `json:"credits_used"`
			} `json:"credits_info"`
		} `json:"user_entitlement_pack_list"`
	} `json:"data"`
}

// FetchEntitlementUsage 拉取账号 entitlement（并发上限与积分包）。
func (c *Client) FetchEntitlementUsage(ctx context.Context, token string) (*EntitlementUsage, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("trae: token is empty")
	}
	body, _ := json.Marshal(map[string]any{"require_usage": true, "req_source": 1})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiHost()+entUsagePath, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(req, token, false)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: fetch entitlement usage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("trae: fetch entitlement usage read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			Kind:    classifyHTTPError(resp.StatusCode),
			Code:    strconv.Itoa(resp.StatusCode),
			Message: fmt.Sprintf("ide_user_ent_usage [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var parsed entUsageResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("trae: fetch entitlement usage parse response: %w", err)
	}
	out := &EntitlementUsage{ParallelLimit: parsed.Data.Quota.ParallelLimit}
	for _, pack := range parsed.Data.UserEntitlementPackList {
		if pack.ProductType == 2 {
			out.CreditsTotal += pack.CreditsInfo.CreditsLimit
			out.CreditsUsed += pack.CreditsInfo.CreditsUsed
		}
	}
	return out, nil
}

// JWTExp 从 Cloud-IDE-JWT 的 exp 声明解析过期时间。企业控制台的兑换响应可能
// 不带 TokenExpireAt，回退用此值驱动刷新调度。
func JWTExp(token string) (time.Time, bool) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	claims := struct {
		Exp int64 `json:"exp"`
	}{}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}

// NormalizeExpireAt 把上游时间戳归一化为 time.Time。上游混用秒级/毫秒级
// Unix 时间戳与 ISO 字符串（含带时区与 naive 两种），需统一解析。
func NormalizeExpireAt(raw string) (time.Time, bool) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return time.Time{}, false
	}
	// 纯数字：按数量级区分秒/毫秒。
	if n := len(value); n >= 10 && n <= 14 && isAllDigits(value) {
		num, err := strconv.ParseInt(value, 10, 64)
		if err != nil {
			return time.Time{}, false
		}
		if n >= 13 { // 毫秒
			return time.UnixMilli(num), true
		}
		return time.Unix(num, 0), true
	}
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"}
	for i, layout := range layouts {
		var (
			t   time.Time
			err error
		)
		if i >= 2 {
			t, err = time.ParseInLocation(layout, value, time.UTC)
		} else {
			t, err = time.Parse(layout, value)
		}
		if err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

func isAllDigits(value string) bool {
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return len(value) > 0
}

func truncate(value string, n int) string {
	if len(value) <= n {
		return value
	}
	return value[:n] + "..."
}

// ValidateRefreshRotation 在落库前校验刷新竞态：请求发出时所用的
// refresh_token 必须仍是账号当前值；不等说明刷新期间凭据已被并发替换，
// 本次响应已过期，必须丢弃（防止把新轮换的 rt 覆盖回旧值）。
func ValidateRefreshRotation(requestRefreshToken, currentRefreshToken string) error {
	requested := strings.TrimSpace(requestRefreshToken)
	current := strings.TrimSpace(currentRefreshToken)
	if current != "" && requested != current {
		return fmt.Errorf("trae: refresh token rotated during exchange (stale response discarded)")
	}
	return nil
}
