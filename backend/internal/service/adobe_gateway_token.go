package service

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/adobe"
)

const (
	// adobeTokenUsableSkew 是旧 token 在刷新失败时仍可继续使用的最小剩余寿命。
	adobeTokenUsableSkew = time.Minute
	// adobeLockWaitTime 是其它实例持有刷新锁、且本地 token 已不可用时的等待时长。
	adobeLockWaitTime = 200 * time.Millisecond
)

// AdobeTokenProvider 为 Adobe OAuth（cookie）账号提供 IMS access_token。
//
// 后台 TokenRefreshService 负责保温，但请求仍可能落到空 token 或临近过期的 token
// （新账号、错过一轮）。请求路径走与后台相同的 OAuthRefreshAPI：进程内锁 + Redis 锁
// + DB 重读 + 二次检查，并发请求只打一次 IMS，刷新结果按 cookie 条件合并落库。
type AdobeTokenProvider struct {
	accountRepo AccountRepository
	refreshAPI  *OAuthRefreshAPI
	refresher   *AdobeTokenRefresher
}

// NewAdobeTokenProvider 构造 provider。refreshAPI 为 nil 时退化为仅进程内锁的独立实例。
func NewAdobeTokenProvider(accountRepo AccountRepository, refreshAPI *OAuthRefreshAPI) *AdobeTokenProvider {
	return newAdobeTokenProvider(accountRepo, refreshAPI, NewAdobeTokenRefresher())
}

func newAdobeTokenProvider(accountRepo AccountRepository, refreshAPI *OAuthRefreshAPI, refresher *AdobeTokenRefresher) *AdobeTokenProvider {
	if refreshAPI == nil {
		refreshAPI = NewOAuthRefreshAPI(accountRepo, nil)
	}
	return &AdobeTokenProvider{
		accountRepo: accountRepo,
		refreshAPI:  refreshAPI,
		refresher:   refresher,
	}
}

// GetAccessToken 返回可用的 IMS access_token（firefly 免费通道）。
//
// 刷新失败时的取舍：
//   - cookie 已失效（AuthError）：账号置 error 并返回错误，即便旧 token 尚未过期；
//   - 账号在锁内重读时已被停用或删掉 cookie：尊重管理员操作，返回错误；
//   - 其它失败（IMS 5xx/超时、DB 重读或落库失败、锁竞争）：旧 token 仍可用就继续用。
//
// 无 cookie 可换时照用旧 token，让上游 401 作为权威结果——与账号测试路径一致。
// 调用方传入的 account 是调度快照，这里不修改其 Credentials。
func (p *AdobeTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	return p.getLaneToken(ctx, account, AdobeLaneFirefly)
}

// GetGenerationToken 为一次出图选择计费通道并返回该通道可用的 token。
//
// 通道选择见 SelectAdobeGenerationLane：积分优先且 express 可用 → express（付费
// 积分），否则 firefly（免费额度）。express 拿不到 token（铸币退避窗口内、刷新
// 失败等）时降级 firefly——付费通道的问题不该让请求失败，返回的 lane 始终是
// 实际拿到 token 的通道。
func (p *AdobeTokenProvider) GetGenerationToken(
	ctx context.Context, account *Account, freeLaneOnly bool,
) (string, AdobeLane, error) {
	if account == nil {
		return "", AdobeLaneFirefly, errors.New("account is nil")
	}
	lane := SelectAdobeGenerationLane(account, freeLaneOnly)
	if !lane.IsExpress() {
		token, err := p.getLaneToken(ctx, account, AdobeLaneFirefly)
		return token, AdobeLaneFirefly, err
	}
	token, err := p.getLaneToken(ctx, account, AdobeLaneExpress)
	if err == nil {
		return token, AdobeLaneExpress, nil
	}
	slog.Warn("adobe_express_token_unavailable_fallback_free",
		"account_id", account.ID, "error", err)
	token, err = p.getLaneToken(ctx, account, AdobeLaneFirefly)
	return token, AdobeLaneFirefly, err
}

// laneCredentialKey 是通道对应的凭据键。
func laneCredentialKey(lane AdobeLane) string {
	if lane.IsExpress() {
		return adobeExpressAccessTokenKey
	}
	return "access_token"
}

// getLaneToken 是 GetAccessToken 的通道参数化版本：express 通道复用同一套
// 锁 + 重读 + 二次检查的刷新编排（Refresh 内部会同时铸两种 token）。
func (p *AdobeTokenProvider) getLaneToken(ctx context.Context, account *Account, lane AdobeLane) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	credentialKey := laneCredentialKey(lane)
	token := strings.TrimSpace(account.GetCredential(credentialKey))
	if p == nil || p.refresher == nil || p.refreshAPI == nil {
		if token == "" {
			return "", errors.New(strings.TrimPrefix(credentialKey, "express_") + " not found in credentials")
		}
		return token, nil
	}

	if token != "" && !adobe.IsTokenExpired(token, adobeRefreshWindow) {
		return token, nil
	}

	if !p.refresher.CanRefresh(account) {
		if token == "" {
			return "", errors.New(strings.TrimPrefix(credentialKey, "express_") + " not found in credentials")
		}
		return token, nil
	}

	usable := adobeTokenUsable(token)
	result, err := p.refreshAPI.RefreshIfNeeded(withOAuthRefreshRequestPath(ctx), account, p.refresher, adobeRefreshWindow)
	if err != nil {
		var authErr *adobe.AuthError
		switch {
		case errors.As(err, &authErr):
			markAdobeCookieRefreshPermanentFailure(ctx, p.accountRepo, account, err)
			return "", err
		case ctx.Err() != nil:
			return "", ctx.Err()
		case errors.Is(err, errOAuthRefreshAccountStateChanged):
			return "", err
		case usable:
			slog.Warn("adobe_token_refresh_failed_use_existing",
				"account_id", account.ID, "lane", lane.String(), "error", err)
			return token, nil
		default:
			return "", err
		}
	}

	if result.LockHeld {
		if usable {
			return token, nil
		}
		return p.waitForConcurrentRefresh(ctx, account, credentialKey)
	}

	if result.Account != nil {
		if refreshed := strings.TrimSpace(result.Account.GetCredential(credentialKey)); adobeTokenUsable(refreshed) {
			return refreshed, nil
		}
	}
	if usable {
		return token, nil
	}
	return "", errors.New("adobe refresh returned no usable access_token for lane " + lane.String())
}

// waitForConcurrentRefresh 在其它实例持有刷新锁时短暂等待，再从 DB 读一次结果。
func (p *AdobeTokenProvider) waitForConcurrentRefresh(ctx context.Context, account *Account, credentialKey string) (string, error) {
	timer := time.NewTimer(adobeLockWaitTime)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-timer.C:
	}
	if p.accountRepo != nil {
		latest, err := p.accountRepo.GetByID(ctx, account.ID)
		if err == nil && latest != nil {
			if refreshed := strings.TrimSpace(latest.GetCredential(credentialKey)); adobeTokenUsable(refreshed) {
				return refreshed, nil
			}
		}
	}
	return "", errors.New("adobe token refresh in progress")
}

// adobeTokenUsable 判断 token 是否仍能直接发往上游。
// 解不出 exp 的 token 视为可用，交给上游 401 裁决（与 IsTokenExpired 的约定一致）。
func adobeTokenUsable(token string) bool {
	return token != "" && !adobe.IsTokenExpired(token, adobeTokenUsableSkew)
}

// markAdobeCookieRefreshPermanentFailure 把 IMS cookie 换 token 的终态失败落到账号 error。
// SetError 失败只记日志：调用方仍必须看到原来的刷新错误，不能误以为 token 可用。
func markAdobeCookieRefreshPermanentFailure(ctx context.Context, repo AccountRepository, account *Account, err error) {
	if repo == nil || account == nil || account.ID <= 0 || !isNonRetryableRefreshError(err) {
		return
	}
	// client_id / scope 类错误对所有账号都一样，不是这份 cookie 的问题：不隔离账号。
	if isSharedProviderRefreshError(err) {
		return
	}
	errorMsg := "Token refresh failed (non-retryable): " + err.Error()
	applied, setErr := setAdobeCookieRefreshError(ctx, repo, account, errorMsg)
	if setErr != nil {
		slog.Warn("failed to persist adobe cookie refresh error status",
			"account_id", account.ID, "error", setErr)
		return
	}
	if !applied {
		slog.Info("adobe_cookie_refresh_error_skipped_cookie_changed", "account_id", account.ID)
	}
}

func (s *GatewayService) getAdobeOAuthToken(ctx context.Context, account *Account) (string, error) {
	return s.adobeTokenProvider.GetAccessToken(ctx, account)
}

// GetAdobeGenerationToken 为出图选择计费通道并取 token，语义见
// AdobeTokenProvider.GetGenerationToken。handler 层的出图路径一律走这里。
func (s *GatewayService) GetAdobeGenerationToken(
	ctx context.Context, account *Account, freeLaneOnly bool,
) (string, AdobeLane, error) {
	return s.adobeTokenProvider.GetGenerationToken(ctx, account, freeLaneOnly)
}
