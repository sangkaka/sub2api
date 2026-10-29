package service

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const traeUsageCacheTTL = 60 * time.Second

type traeUsageCacheEntry struct {
	usage     *UsageInfo
	fetchedAt time.Time
}

var traeUsageCache sync.Map // accountID -> traeUsageCacheEntry

// SetTraeDeps 注入 TRAE token provider 与 OAuth service（用量探测经账号代
// 理客户端调用 entitlement 端点）。
func (s *AccountUsageService) SetTraeDeps(tokenProvider *TraeTokenProvider, oauthService *TraeOAuthService) *AccountUsageService {
	s.traeTokenProvider = tokenProvider
	s.traeOAuthService = oauthService
	return s
}

// getTraeUsage 拉取 TRAE 积分余额（ide_user_ent_usage 的 product_type=2
// 积分包汇总）与并发槽位，映射为通用余额展示；60s 进程内缓存。
func (s *AccountUsageService) getTraeUsage(ctx context.Context, account *Account, force bool) (*UsageInfo, error) {
	now := time.Now()
	if account == nil || account.Platform != PlatformTrae {
		return &UsageInfo{Source: "active", UpdatedAt: &now}, nil
	}
	if !force {
		if cached, ok := traeUsageCache.Load(account.ID); ok {
			entry := cached.(traeUsageCacheEntry)
			if time.Since(entry.fetchedAt) < traeUsageCacheTTL && entry.usage.Error == "" {
				usage := *entry.usage
				return &usage, nil
			}
		}
	}
	usage := &UsageInfo{Source: "active", UpdatedAt: &now}
	if s == nil || s.traeTokenProvider == nil || s.traeOAuthService == nil {
		usage.Error = "trAE usage deps not configured"
		return usage, nil
	}
	token, err := s.traeTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		usage.Error = fmt.Sprintf("get token: %s", err.Error())
		return usage, nil
	}
	client := s.traeOAuthService.UpstreamClient(ctx, account)
	ent, err := client.FetchEntitlementUsage(ctx, token)
	if err != nil {
		usage.Error = fmt.Sprintf("fetch entitlement: %s", err.Error())
		return usage, nil
	}
	remaining := ent.CreditsTotal - ent.CreditsUsed
	if remaining < 0 {
		remaining = 0
	}
	usage.TraeCredits = &TraeCreditProgress{
		CurrentUsage:   ent.CreditsUsed,
		UsageLimit:     ent.CreditsTotal,
		PercentageUsed: percentageUsed(ent.CreditsUsed, ent.CreditsTotal),
	}
	usage.TraeParallelLimit = ent.ParallelLimit
	traeUsageCache.Store(account.ID, traeUsageCacheEntry{usage: usage, fetchedAt: now})
	return usage, nil
}

func percentageUsed(used, total float64) float64 {
	if total <= 0 {
		return 0
	}
	pct := used / total * 100
	if pct < 0 {
		return 0
	}
	if pct > 100 {
		return 100
	}
	return pct
}
