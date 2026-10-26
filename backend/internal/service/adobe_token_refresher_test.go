//go:build unit

package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adobe"
	"github.com/stretchr/testify/require"
)

func adobeTestToken(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(raw) + ".sig"
}

func newAdobeTestRefresher(t *testing.T, handler func(*adobe.Request, int) (*adobe.Response, error)) *AdobeTokenRefresher {
	t.Helper()
	transport := &adobeFakeTransport{handler: handler}
	client := adobe.NewClient(adobe.ClientConfig{Transport: transport, DownloadTransport: transport})
	r := NewAdobeTokenRefresher()
	r.clients.newClient = func(string) *adobe.Client { return client }
	return r
}

func TestAdobeTokenRefresherCanRefresh(t *testing.T) {
	withCookie := &Account{
		Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: map[string]any{"cookie": "aux_sid=1"},
	}
	r := NewAdobeTokenRefresher()
	require.True(t, r.CanRefresh(withCookie))

	// 没有 cookie 就换不了 token——Adobe 没有 refresh_token 这条退路。
	require.False(t, r.CanRefresh(&Account{
		Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: map[string]any{"access_token": "tok"},
	}))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformKiro, Type: AccountTypeOAuth,
		Credentials: map[string]any{"cookie": "c"}}))
	require.False(t, r.CanRefresh(&Account{Platform: PlatformAdobe, Type: AccountTypeAPIKey,
		Credentials: map[string]any{"cookie": "c"}}))
	require.False(t, r.CanRefresh(nil))
}

func TestAdobeTokenRefresherNeedsRefresh(t *testing.T) {
	r := NewAdobeTokenRefresher()
	account := func(token string) *Account {
		return &Account{
			Platform: PlatformAdobe, Type: AccountTypeOAuth,
			Credentials: map[string]any{"cookie": "aux_sid=1", "access_token": token},
		}
	}

	// token 缺失必须刷新，否则新建的账号永远不会被填上 token。
	require.True(t, r.NeedsRefresh(account(""), time.Minute))
	require.True(t, r.NeedsRefresh(
		account(adobeTestToken(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})), time.Minute))
	require.False(t, r.NeedsRefresh(
		account(adobeTestToken(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})), time.Minute))

	// 传入窗口比 Adobe 的默认窗口还小时应取默认值：IMS token 寿命短，
	// 窗口过小会让刷新赶不上过期。
	soon := account(adobeTestToken(t, map[string]any{"exp": time.Now().Add(10 * time.Minute).Unix()}))
	require.True(t, r.NeedsRefresh(soon, time.Second))
}

// Refresh 必须保留 credentials 里的既有字段：cookie 和 model_mapping 都在里面，
// 直接返回新 map 会把账号的配置抹掉。
func TestAdobeTokenRefresherPreservesExistingCredentials(t *testing.T) {
	newToken := adobeTestToken(t, map[string]any{
		"user_id": "u1", "exp": time.Now().Add(24 * time.Hour).Unix(),
	})
	r := newAdobeTestRefresher(t, func(*adobe.Request, int) (*adobe.Response, error) {
		body, _ := json.Marshal(map[string]any{"access_token": newToken, "expires_in": 86400})
		return &adobe.Response{StatusCode: 200, Headers: map[string]string{}, Body: body}, nil
	})

	account := &Account{
		ID: 3, Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: map[string]any{
			"cookie":        "aux_sid=abc",
			"access_token":  "stale",
			"model_mapping": map[string]any{"gpt-image-2": "firefly-gpt-image-2"},
		},
	}
	updated, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)

	require.Equal(t, newToken, updated["access_token"])
	require.Equal(t, "aux_sid=abc", updated["cookie"])
	require.NotNil(t, updated["model_mapping"])
	require.NotEmpty(t, updated["expires_at"])
}

// cookie 失效只能由用户重新导出，必须给出明确信号而不是当成可重试的临时故障。
func TestAdobeTokenRefresherSurfacesDeadCookie(t *testing.T) {
	r := newAdobeTestRefresher(t, func(*adobe.Request, int) (*adobe.Response, error) {
		return &adobe.Response{StatusCode: 401, Headers: map[string]string{}, Body: []byte("{}")}, nil
	})
	_, err := r.Refresh(context.Background(), &Account{
		ID: 4, Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: map[string]any{"cookie": "dead"},
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "re-export")

	var authErr *adobe.AuthError
	require.True(t, errors.As(err, &authErr), "应保留 AuthError 以便上层区分")
}

func TestAdobeTokenRefresherRejectsNonAdobeAccount(t *testing.T) {
	r := NewAdobeTokenRefresher()
	_, err := r.Refresh(context.Background(), &Account{Platform: PlatformKiro, Type: AccountTypeOAuth})
	require.ErrorContains(t, err, "not a refreshable adobe account")
}

func TestAdobeTokenRefresherCacheKey(t *testing.T) {
	r := NewAdobeTokenRefresher()
	require.Equal(t, "adobe:token:42", r.CacheKey(&Account{ID: 42}))
	require.Empty(t, r.CacheKey(nil))
}

// adobeIMSFormValue 从 IMS 刷新请求体里取表单字段（client_id/scope 等）。
func adobeIMSFormValue(t *testing.T, req *adobe.Request, key string) string {
	t.Helper()
	values, err := url.ParseQuery(string(req.Body))
	require.NoError(t, err)
	return values.Get(key)
}

// newAdobeDualLaneTestRefresher 与 newAdobeTestRefresher 同构，但按通道返回
// 对应身份的客户端——firefly 用 clio-playground-web，express 用 projectx_webapp。
func newAdobeDualLaneTestRefresher(t *testing.T, handler func(*adobe.Request, int) (*adobe.Response, error)) *AdobeTokenRefresher {
	t.Helper()
	transport := &adobeFakeTransport{handler: handler}
	r := NewAdobeTokenRefresher()
	r.clients.newClientWithLane = func(_ string, lane AdobeLane) *adobe.Client {
		cfg := adobe.ClientConfig{Transport: transport, DownloadTransport: transport}
		if lane.IsExpress() {
			cfg.Identity = adobe.ExpressIdentity
		}
		return adobe.NewClient(cfg)
	}
	return r
}

// 积分优先账号一次 Refresh 同时铸两种 token：同一份 cookie、两个 client_id。
func TestAdobeTokenRefresherDualMintsExpressToken(t *testing.T) {
	fireflyToken := adobeTestToken(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	expressToken := adobeTestToken(t, map[string]any{"exp": time.Now().Add(12 * time.Hour).Unix()})
	r := newAdobeDualLaneTestRefresher(t, func(req *adobe.Request, _ int) (*adobe.Response, error) {
		token := fireflyToken
		if adobeIMSFormValue(t, req, "client_id") == "projectx_webapp" {
			require.Equal(t, "AdobeID,firefly_api,openid", adobeIMSFormValue(t, req, "scope"),
				"express 铸币必须用 Express scope")
			token = expressToken
		}
		body, _ := json.Marshal(map[string]any{"access_token": token, "expires_in": 86400})
		return &adobe.Response{StatusCode: 200, Headers: map[string]string{}, Body: body}, nil
	})
	account := &Account{
		ID: 1, Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Extra:       map[string]any{"adobe_use_credits": true},
		Credentials: map[string]any{"cookie": "aux_sid=1", "model_mapping": map[string]any{"a": "b"}},
	}

	credentials, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, fireflyToken, credentials["access_token"])
	require.Equal(t, expressToken, credentials[adobeExpressAccessTokenKey])
	require.Contains(t, credentials, adobeExpressExpiresAtKey)
	// 既有字段与积分开关之外的内容原样保留。
	require.Equal(t, "aux_sid=1", credentials["cookie"])
	require.Equal(t, map[string]any{"a": "b"}, credentials["model_mapping"])
	require.NotContains(t, credentials, adobeExpressExhaustedUntilKey)
}

// express 铸币失败不拖垮 firefly：只写一个短退避标记。
func TestAdobeTokenRefresherExpressMintFailureDegrades(t *testing.T) {
	fireflyToken := adobeTestToken(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	r := newAdobeDualLaneTestRefresher(t, func(req *adobe.Request, _ int) (*adobe.Response, error) {
		if adobeIMSFormValue(t, req, "client_id") == "projectx_webapp" {
			return &adobe.Response{StatusCode: 400,
				Headers: map[string]string{}, Body: []byte(`{"error":"invalid_scope"}`)}, nil
		}
		body, _ := json.Marshal(map[string]any{"access_token": fireflyToken, "expires_in": 86400})
		return &adobe.Response{StatusCode: 200, Headers: map[string]string{}, Body: body}, nil
	})
	account := &Account{
		ID: 1, Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Extra:       map[string]any{"adobe_use_credits": true},
		Credentials: map[string]any{"cookie": "aux_sid=1"},
	}

	credentials, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, fireflyToken, credentials["access_token"])
	require.NotContains(t, credentials, adobeExpressAccessTokenKey)

	until, ok := credentials[adobeExpressExhaustedUntilKey].(string)
	require.True(t, ok, "铸币失败必须写退避标记")
	parsed, err := time.Parse(time.RFC3339, until)
	require.NoError(t, err)
	require.WithinDuration(t, time.Now().Add(adobeExpressMintRetryDelay), parsed, time.Minute)
}

// 非积分优先账号不铸 express：失败路径也不会被触发。
func TestAdobeTokenRefresherSkipsExpressWithoutUseCredits(t *testing.T) {
	newToken := adobeTestToken(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	r := newAdobeTestRefresher(t, func(req *adobe.Request, _ int) (*adobe.Response, error) {
		require.Equal(t, "clio-playground-web", adobeIMSFormValue(t, req, "client_id"))
		body, _ := json.Marshal(map[string]any{"access_token": newToken, "expires_in": 86400})
		return &adobe.Response{StatusCode: 200, Headers: map[string]string{}, Body: body}, nil
	})
	account := &Account{
		ID: 1, Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: map[string]any{"cookie": "aux_sid=1"},
	}

	credentials, err := r.Refresh(context.Background(), account)
	require.NoError(t, err)
	require.NotContains(t, credentials, adobeExpressAccessTokenKey)
}

// 积分优先账号把 express 侧纳入 NeedsRefresh：缺失/临期都要刷，退避窗口内不刷。
func TestAdobeTokenRefresherNeedsRefreshConsidersExpress(t *testing.T) {
	r := NewAdobeTokenRefresher()
	freshFirefly := adobeTestToken(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	freshExpress := adobeTestToken(t, map[string]any{"exp": time.Now().Add(24 * time.Hour).Unix()})
	expiredExpress := adobeTestToken(t, map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
	base := func(credentials map[string]any) *Account {
		return &Account{
			ID: 1, Platform: PlatformAdobe, Type: AccountTypeOAuth,
			Extra: map[string]any{"adobe_use_credits": true},
			Credentials: map[string]any{
				"cookie":       "aux_sid=1",
				"access_token": freshFirefly,
				"expires_at":   time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
			},
		}
	}
	withExpress := func(token string, extra ...map[string]any) *Account {
		account := base(nil)
		account.Credentials[adobeExpressAccessTokenKey] = token
		for _, e := range extra {
			for k, v := range e {
				account.Credentials[k] = v
			}
		}
		return account
	}

	// firefly 新鲜但 express 缺失 / 过期：需要刷新。
	require.True(t, r.NeedsRefresh(withExpress(""), time.Minute))
	require.True(t, r.NeedsRefresh(withExpress(expiredExpress), time.Minute))
	// 两者都新鲜：不刷。
	require.False(t, r.NeedsRefresh(withExpress(freshExpress), time.Minute))
	// 退避/耗尽窗口内：即便 express 缺失也不刷（窗口的语义就是别再为 express 花刷新）。
	require.False(t, r.NeedsRefresh(withExpress("", map[string]any{
		adobeExpressExhaustedUntilKey: time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
	}), time.Minute))
	// 未开启积分优先：express 状态不影响判断。
	plain := &Account{
		ID: 1, Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: map[string]any{"cookie": "aux_sid=1", "access_token": freshFirefly},
	}
	require.False(t, r.NeedsRefresh(plain, time.Minute))
}
