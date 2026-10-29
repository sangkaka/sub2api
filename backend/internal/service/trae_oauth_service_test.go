package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	require.Equal(t, "https://console.enterprise.trae.cn", withHost.CloudIDEHost)

	withoutHost := svc.UpstreamClient(context.Background(), &Account{})
	require.Empty(t, withoutHost.CloudIDEHost)
}

// 网关与探活的 biz_session_id 必须不同命名空间且各自稳定：探活半途 stop
// 的会话不得污染网关会话（迁移首批 1005 事故的回归测试）。
func TestTraeCommonParamsSessionNamespaces(t *testing.T) {
	account := &Account{Credentials: map[string]any{"user_id": "433084928"}}
	gw1 := BuildTraeCommonParams(account, "kimi-k3.1-pro")
	gw2 := BuildTraeCommonParams(account, "kimi-k3.1-pro")
	probe := BuildTraeCommonParamsForProbe(account, "kimi-k3.1-pro")

	parse := func(s string) map[string]any {
		out := map[string]any{}
		require.NoError(t, json.Unmarshal([]byte(s), &out))
		return out
	}
	require.Equal(t, parse(gw1)["biz_session_id"], parse(gw2)["biz_session_id"], "同账号同模型网关会话需稳定")
	require.NotEqual(t, parse(gw1)["biz_session_id"], parse(probe)["biz_session_id"], "探活会话必须与网关隔离")
	require.NotEmpty(t, parse(gw1)["biz_session_id"])
}
