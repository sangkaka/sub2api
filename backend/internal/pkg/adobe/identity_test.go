//go:build unit

package adobe

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// ExpressIdentity 只覆盖计费身份字段（client_id/scope/x-api-key/Origin），
// 浏览器伪装组由 withDefaults 从 DefaultIdentity 补齐——两组字段正交。
func TestExpressIdentityWithDefaults(t *testing.T) {
	identity := ExpressIdentity.withDefaults()

	require.Equal(t, "projectx_webapp", identity.IMSClientID)
	require.Equal(t, "AdobeID,firefly_api,openid", identity.IMSScope)
	require.Equal(t, "projectx_webapp", identity.FireflyAPIKey)
	require.Equal(t, "https://new.express.adobe.com", identity.Origin)
	require.Equal(t, "https://new.express.adobe.com/", identity.Referer)
	// credits/balance 与浏览器伪装沿用 firefly 侧的值。
	require.Equal(t, DefaultIdentity.CreditsAPIKey, identity.CreditsAPIKey)
	require.Equal(t, DefaultIdentity.UserAgent, identity.UserAgent)
	require.Equal(t, DefaultIdentity.TLSProfile, identity.TLSProfile)
	require.Equal(t, DefaultIdentity.SecChUA, identity.SecChUA)
	require.NotEqual(t, DefaultIdentity.IMSClientID, identity.IMSClientID)
}

// express 客户端的提交头必须带付费身份的 x-api-key 与 Origin。
func TestExpressClientSubmitHeaders(t *testing.T) {
	api := &fakeTransport{handler: func(*Request, int) (*Response, error) {
		return &Response{StatusCode: 200, Headers: map[string]string{}, Body: []byte(`{}`)}, nil
	}}
	client := NewClient(ClientConfig{Transport: api, DownloadTransport: api, Identity: ExpressIdentity})
	require.Equal(t, "projectx_webapp", client.Identity().FireflyAPIKey)

	headers, _ := client.submitHeaders("tok", "prompt", "")
	require.Equal(t, "projectx_webapp", headers["x-api-key"])
	require.Equal(t, "https://new.express.adobe.com", headers["origin"])
	require.Equal(t, "https://new.express.adobe.com/", headers["referer"])
}
