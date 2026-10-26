//go:build unit

package service

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func adobeCreditModeAccount(extra map[string]any, credentials map[string]any) *Account {
	account := &Account{
		Platform: PlatformAdobe, Type: AccountTypeOAuth,
		Credentials: credentials,
	}
	if extra != nil {
		account.Extra = extra
	}
	return account
}

func TestAdobeUseCredits(t *testing.T) {
	require.True(t, AdobeUseCredits(adobeCreditModeAccount(
		map[string]any{"adobe_use_credits": true}, nil)))
	// 缺省/显式 false / 非布尔：都算关闭。
	require.False(t, AdobeUseCredits(adobeCreditModeAccount(nil, nil)))
	require.False(t, AdobeUseCredits(adobeCreditModeAccount(
		map[string]any{"adobe_use_credits": false}, nil)))
	require.False(t, AdobeUseCredits(adobeCreditModeAccount(
		map[string]any{"adobe_use_credits": "true"}, nil)))
	// 中转号（apikey）与其它平台即便带键也不生效。
	relay := &Account{Platform: PlatformAdobe, Type: AccountTypeAPIKey,
		Extra: map[string]any{"adobe_use_credits": true}}
	require.False(t, AdobeUseCredits(relay))
	other := &Account{Platform: PlatformGrok, Type: AccountTypeOAuth,
		Extra: map[string]any{"adobe_use_credits": true}}
	require.False(t, AdobeUseCredits(other))
	require.False(t, AdobeUseCredits(nil))
}

func TestAdobeExpressLaneAvailable(t *testing.T) {
	// 未开启积分优先：express 永不可用。
	require.False(t, AdobeExpressLaneAvailable(adobeCreditModeAccount(nil, nil)))

	enabled := func(credentials map[string]any) *Account {
		return adobeCreditModeAccount(map[string]any{"adobe_use_credits": true}, credentials)
	}

	// 开启且无耗尽标记：可用。
	require.True(t, AdobeExpressLaneAvailable(enabled(nil)))
	// 耗尽窗口未到：不可用。
	future := time.Now().Add(30 * time.Minute).UTC().Format(time.RFC3339)
	require.False(t, AdobeExpressLaneAvailable(enabled(map[string]any{
		"express_exhausted_until": future,
	})))
	// 窗口已过：视为待重试，可用（余额轮询或下次失败会再纠正）。
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	require.True(t, AdobeExpressLaneAvailable(enabled(map[string]any{
		"express_exhausted_until": past,
	})))
	// 脏值不当作耗尽。
	require.True(t, AdobeExpressLaneAvailable(enabled(map[string]any{
		"express_exhausted_until": "not-a-time",
	})))
}

func TestSelectAdobeGenerationLane(t *testing.T) {
	enabled := adobeCreditModeAccount(map[string]any{"adobe_use_credits": true}, nil)
	require.Equal(t, AdobeLaneExpress, SelectAdobeGenerationLane(enabled, false))
	// 请求内已强制免费（express 撞过耗尽）：即便凭据快照还没刷新也走 firefly。
	require.Equal(t, AdobeLaneFirefly, SelectAdobeGenerationLane(enabled, true))
	// 未开启积分优先：恒免费。
	require.Equal(t, AdobeLaneFirefly, SelectAdobeGenerationLane(adobeCreditModeAccount(nil, nil), false))

	require.True(t, AdobeLaneExpress.IsExpress())
	require.False(t, AdobeLaneFirefly.IsExpress())
	require.Equal(t, "express", AdobeLaneExpress.String())
}
