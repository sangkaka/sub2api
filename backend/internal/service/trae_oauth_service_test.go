package service

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
)

// 企业版账号（credentials.auth_host）的刷新必须打到 auth_host，且 Data
// 包装响应可解析（console.enterprise.trae.cn 实测形状）。
func TestTraeRefreshEnterpriseAuthHost(t *testing.T) {
	var gotPath string
	exchange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1794488528}`))
		fmt.Fprintf(w, `{"code":0,"message":"","Data":{"Token":"h.%s.s","RefreshToken":"rt-new"}}`, payload)
	}))
	defer exchange.Close()

	svc := NewTraeOAuthService(nil, nil)
	account := &Account{
		Platform: PlatformTrae,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"refresh_token": "rt-old",
			"user_id":       "433084928",
			"auth_host":     " " + exchange.URL + "/ ",
		},
	}
	result, err := svc.RefreshAccountToken(context.Background(), account)
	require.NoError(t, err)
	require.Equal(t, "/cloudide/api/v3/trae/oauth/ExchangeToken", gotPath)
	require.Equal(t, "rt-new", result.RefreshToken)
}

// BuildTraeCredentials 在 TokenExpireAt 缺失时用 JWT exp 兜底 expires_at。
func TestBuildTraeCredentialsJWTExpFallback(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1794488528}`))
	token := "header." + payload + ".sig"
	creds := BuildTraeCredentials(&trae.ExchangeTokenResult{
		Token:        token,
		RefreshToken: "rt",
	})
	require.Equal(t, token, creds["token"])
	require.Contains(t, creds["expires_at"], "2026") // 1794488528 → 2026-11
}

// UpstreamClient 读取账号 auth_host 凭据（去空白、去尾斜杠），空值回落默认。
func TestUpstreamClientAuthHost(t *testing.T) {
	svc := NewTraeOAuthService(nil, nil)
	withHost := svc.UpstreamClient(context.Background(), &Account{
		Credentials: map[string]any{"auth_host": " https://console.enterprise.trae.cn/ "},
	})
	require.Equal(t, "https://console.enterprise.trae.cn", withHost.ExchangeHost)

	withoutHost := svc.UpstreamClient(context.Background(), &Account{})
	require.Empty(t, withoutHost.ExchangeHost)
}
