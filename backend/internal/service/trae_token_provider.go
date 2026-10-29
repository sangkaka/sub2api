package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
)

// traeTokenRefreshSkew：距过期不足该窗口即触发刷新。上游实测 token 有效期
// 较短，30 分钟窗口与 trae2api 生产一致。
const traeTokenRefreshSkew = 30 * time.Minute
const traeTokenCacheSkew = 5 * time.Minute

type TraeTokenCache = GeminiTokenCache

type traeAccountTokenRefresher interface {
	RefreshAccountToken(ctx context.Context, account *Account) (*trae.ExchangeTokenResult, error)
}

// TraeTokenProvider 是请求热路径的 token 获取器：缓存命中直接返回，临期经
// OAuthRefreshAPI 统一刷新（分布式锁 + 竞态保护）。
type TraeTokenProvider struct {
	accountRepo      AccountRepository
	tokenCache       TraeTokenCache
	traeOAuthService traeAccountTokenRefresher
	refreshAPI       *OAuthRefreshAPI
	executor         OAuthRefreshExecutor
	refreshPolicy    ProviderRefreshPolicy
}

func NewTraeTokenProvider(
	accountRepo AccountRepository,
	tokenCache TraeTokenCache,
	traeOAuthService *TraeOAuthService,
) *TraeTokenProvider {
	return &TraeTokenProvider{
		accountRepo:      accountRepo,
		tokenCache:       tokenCache,
		traeOAuthService: traeOAuthService,
		refreshPolicy:    GeminiProviderRefreshPolicy(),
	}
}

func (p *TraeTokenProvider) SetRefreshAPI(api *OAuthRefreshAPI, executor OAuthRefreshExecutor) {
	p.refreshAPI = api
	p.executor = executor
}

func (p *TraeTokenProvider) SetRefreshPolicy(policy ProviderRefreshPolicy) {
	p.refreshPolicy = policy
}

// GetAccessToken 返回可用的 Cloud-IDE-JWT。
func (p *TraeTokenProvider) GetAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformTrae || account.Type != AccountTypeOAuth {
		return "", errors.New("not a trae oauth account")
	}

	cacheKey := TraeTokenCacheKey(account)
	if p.tokenCache != nil {
		if token, err := p.tokenCache.GetAccessToken(ctx, cacheKey); err == nil && strings.TrimSpace(token) != "" {
			return token, nil
		}
	}

	expiresAt := account.GetCredentialAsTime("expires_at")
	needsRefresh := expiresAt == nil || time.Until(*expiresAt) <= traeTokenRefreshSkew

	if needsRefresh && p.refreshAPI != nil && p.executor != nil {
		result, err := p.refreshAPI.RefreshIfNeeded(ctx, account, p.executor, traeTokenRefreshSkew)
		if err != nil {
			if p.refreshPolicy.OnRefreshError == ProviderRefreshErrorReturn {
				return "", err
			}
		} else if result.LockHeld {
			if p.refreshPolicy.OnLockHeld == ProviderLockHeldWaitForCache && p.tokenCache != nil {
				if token, cacheErr := p.tokenCache.GetAccessToken(ctx, cacheKey); cacheErr == nil && strings.TrimSpace(token) != "" {
					return token, nil
				}
			}
		} else {
			if result.Account != nil {
				account = result.Account
			}
			if len(result.NewCredentials) > 0 {
				account.Credentials = shallowCopyMap(result.NewCredentials)
			}
			expiresAt = account.GetCredentialAsTime("expires_at")
		}
	}

	accessToken := account.GetCredential("token")
	if strings.TrimSpace(accessToken) == "" {
		return "", errors.New("token not found in trae credentials")
	}

	if p.tokenCache != nil {
		latestAccount, isStale := CheckTokenVersion(ctx, account, p.accountRepo)
		if isStale && latestAccount != nil {
			accessToken = latestAccount.GetCredential("token")
			if strings.TrimSpace(accessToken) == "" {
				return "", errors.New("token not found after version check")
			}
		} else {
			ttl := 30 * time.Minute
			if expiresAt != nil {
				until := time.Until(*expiresAt)
				switch {
				case until > traeTokenCacheSkew:
					ttl = until - traeTokenCacheSkew
				case until > 0:
					ttl = until
				default:
					ttl = time.Minute
				}
			}
			_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
		}
	}
	return accessToken, nil
}

// TraeTokenCacheKey 账号级缓存 key（与其他 provider 一致以 account.ID 派生）。
func TraeTokenCacheKey(account *Account) string {
	if account == nil {
		return "trae:account:0"
	}
	return "trae:account:" + strconv.FormatInt(account.ID, 10)
}

// ForceRefreshAccessToken 强制刷新（401/业务码 1001 自愈路径）。
func (p *TraeTokenProvider) ForceRefreshAccessToken(ctx context.Context, account *Account) (string, error) {
	if account == nil {
		return "", errors.New("account is nil")
	}
	if account.Platform != PlatformTrae || account.Type != AccountTypeOAuth {
		return "", errors.New("not a trae oauth account")
	}
	if p.traeOAuthService == nil {
		return "", errors.New("trae oauth service is nil")
	}

	cacheKey := TraeTokenCacheKey(account)
	if p.tokenCache != nil {
		if locked, lockErr := p.tokenCache.AcquireRefreshLock(ctx, cacheKey, 30*time.Second); lockErr == nil && locked {
			defer func() { _ = p.tokenCache.ReleaseRefreshLock(ctx, cacheKey) }()
		}
	}

	if p.accountRepo != nil {
		if latest, err := p.accountRepo.GetByID(ctx, account.ID); err == nil && latest != nil {
			account = latest
		}
	}

	result, err := p.traeOAuthService.RefreshAccountToken(ctx, account)
	if err != nil {
		if isNonRetryableRefreshError(err) && p.accountRepo != nil {
			_ = p.accountRepo.SetError(ctx, account.ID, "Token refresh failed (non-retryable): "+err.Error())
		}
		return "", err
	}

	newCredentials := MergeCredentials(account.Credentials, BuildTraeCredentials(result))
	newCredentials["_token_version"] = time.Now().UnixMilli()
	if err := persistAccountCredentials(ctx, p.accountRepo, account, newCredentials); err != nil {
		return "", err
	}
	accessToken, _ := newCredentials["token"].(string)
	accessToken = strings.TrimSpace(accessToken)
	if accessToken == "" {
		return "", errors.New("token not found after trae refresh")
	}
	if p.tokenCache != nil {
		ttl := 30 * time.Minute
		if expiresAt := account.GetCredentialAsTime("expires_at"); expiresAt != nil {
			if until := time.Until(*expiresAt); until > traeTokenCacheSkew {
				ttl = until - traeTokenCacheSkew
			}
		}
		_ = p.tokenCache.SetAccessToken(ctx, cacheKey, accessToken, ttl)
	}
	return accessToken, nil
}

// TraeTokenRefresher 实现 TokenRefresher + CacheKey（后台扫描与统一刷新
// API 的执行器）。
type TraeTokenRefresher struct {
	traeOAuthService *TraeOAuthService
}

func NewTraeTokenRefresher(traeOAuthService *TraeOAuthService) *TraeTokenRefresher {
	return &TraeTokenRefresher{traeOAuthService: traeOAuthService}
}

func (r *TraeTokenRefresher) CacheKey(account *Account) string {
	return TraeTokenCacheKey(account)
}

func (r *TraeTokenRefresher) CanRefresh(account *Account) bool {
	return account != nil && account.Platform == PlatformTrae && account.Type == AccountTypeOAuth
}

func (r *TraeTokenRefresher) NeedsRefresh(account *Account, _ time.Duration) bool {
	if !r.CanRefresh(account) {
		return false
	}
	if strings.TrimSpace(account.GetCredential("refresh_token")) == "" {
		return false
	}
	expiresAt := account.GetCredentialAsTime("expires_at")
	if expiresAt == nil {
		return true
	}
	return time.Until(*expiresAt) <= traeTokenRefreshSkew
}

func (r *TraeTokenRefresher) Refresh(ctx context.Context, account *Account) (map[string]any, error) {
	if r == nil || r.traeOAuthService == nil {
		return nil, errors.New("trae oauth service is not configured")
	}
	result, err := r.traeOAuthService.RefreshAccountToken(ctx, account)
	if err != nil {
		return nil, err
	}
	newCredentials := r.traeOAuthService.BuildAccountCredentials(result)
	return MergeCredentials(account.Credentials, newCredentials), nil
}
