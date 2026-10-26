package service

import (
	"context"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// Adobe 积分模式（付费优先、耗尽自动回退免费）的配置键、凭据键与通道选择。
//
// 是否花积分由 IMS 铸币时的 client_id/scope 决定（见 pkg/adobe 的 ExpressIdentity
// 注释）：同一份 cookie 可同时铸出 firefly（免费生成权益）与 express（消耗付费
// generative credits）两种 token。extra.adobe_use_credits=true 的账号出图先走
// express 通道，烧完为止——credits/balance 归零，或上游以 x-access-error:
// *_exhausted 明示耗尽——之后同一账号自动切回 firefly 免费通道；余额回补后
// （余额轮询发现 available>0）自动恢复 express。
//
// 耗尽状态持久化在 credentials.express_exhausted_until：请求路径撞到 *_exhausted
// 时写入短窗口（余额轮询会纠正），余额轮询负责停用与恢复，跨请求/跨重启都生效。

// adobeUseCreditsExtraKey 是账号级「积分优先」开关（extra JSON，bool）。
const adobeUseCreditsExtraKey = "adobe_use_credits"

// express 通道的凭据键，与 firefly 侧的 access_token / expires_at 同构。
const (
	adobeExpressAccessTokenKey    = "express_access_token"
	adobeExpressExpiresAtKey      = "express_expires_at"
	adobeExpressExhaustedUntilKey = "express_exhausted_until"
)

// AdobeLane 标识一次 Adobe 出图走的计费通道。
type AdobeLane string

const (
	// AdobeLaneFirefly 是免费额度通道（默认，行为与历史版本一致）。
	AdobeLaneFirefly AdobeLane = "firefly"
	// AdobeLaneExpress 是付费积分通道。
	AdobeLaneExpress AdobeLane = "express"
)

// IsExpress 报告通道是否为付费积分通道。
func (lane AdobeLane) IsExpress() bool { return lane == AdobeLaneExpress }

// String 实现 fmt.Stringer，供日志与 ops 归因使用。
func (lane AdobeLane) String() string { return string(lane) }

// AdobeUseCredits 报告账号是否开启「积分优先」。仅 Adobe OAuth（cookie）账号有意义；
// 中转号与其它平台恒为 false。
func AdobeUseCredits(account *Account) bool {
	if account == nil || account.Platform != PlatformAdobe || account.Type != AccountTypeOAuth {
		return false
	}
	enabled, _ := account.Extra[adobeUseCreditsExtraKey].(bool)
	return enabled
}

// AdobeExpressExhaustedUntil 返回 express 通道的耗尽截止时刻；零值表示未在耗尽窗口。
func AdobeExpressExhaustedUntil(account *Account) time.Time {
	if account == nil {
		return time.Time{}
	}
	if t := account.GetCredentialAsTime(adobeExpressExhaustedUntilKey); t != nil {
		return *t
	}
	return time.Time{}
}

// AdobeExpressLaneAvailable 报告 express 通道当前是否可调度：
// 开启了积分优先，且不在耗尽窗口内（窗口过期视为待重试，余额轮询会再纠正）。
func AdobeExpressLaneAvailable(account *Account) bool {
	if !AdobeUseCredits(account) {
		return false
	}
	until := AdobeExpressExhaustedUntil(account)
	return until.IsZero() || time.Now().After(until)
}

// SelectAdobeGenerationLane 为一次出图选择计费通道：积分优先且 express 可用时走
// express，否则 firefly。freeLaneOnly 强制免费——请求内该账号的 express 已撞过
// 耗尽，同账号重试必须立刻换通道，不能等凭据快照刷新。
func SelectAdobeGenerationLane(account *Account, freeLaneOnly bool) AdobeLane {
	if !freeLaneOnly && AdobeExpressLaneAvailable(account) {
		return AdobeLaneExpress
	}
	return AdobeLaneFirefly
}

// AdobeExpressCredentialsRepository 是 express 通道状态写入的持久化边界。
// 由 repository.AccountRepository 实现（类型断言获取，缺失时静默降级——
// 状态写失败最多多一次失败尝试，不该让本次请求失败）。
type AdobeExpressCredentialsRepository interface {
	SetAdobeExpressExhaustedUntil(ctx context.Context, id int64, until *time.Time) error
	InvalidateAdobeExpressTokenIfUnchanged(ctx context.Context, id int64, expectedAccessToken string) (bool, error)
}

// adobeExpressExhaustionWriter 是耗尽窗口写入所需的最小接口：只关心这一条
// 写入的调用方（余额轮询）不必连带实现 token 失效。
type adobeExpressExhaustionWriter interface {
	SetAdobeExpressExhaustedUntil(ctx context.Context, id int64, until *time.Time) error
}

// setAdobeExpressExhausted 写入/清除 express 耗尽窗口，失败只记日志（见接口注释）。
func setAdobeExpressExhausted(ctx context.Context, repo AccountRepository, accountID int64, until *time.Time) {
	writer, ok := repo.(adobeExpressExhaustionWriter)
	if !ok || repo == nil || accountID <= 0 {
		return
	}
	if err := writer.SetAdobeExpressExhaustedUntil(ctx, accountID, until); err != nil {
		logger.L().With(zap.String("component", "service.adobe")).Warn(
			"adobe.express_exhausted_persist_failed",
			zap.Int64("account_id", accountID),
			zap.Error(err),
		)
	}
}
