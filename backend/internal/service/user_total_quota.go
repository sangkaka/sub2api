package service

import (
	"context"
	"errors"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/domain"
	infraerrors "github.com/Wei-Shaw/sub2api/internal/pkg/errors"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
)

const TotalQuotaPlatform = domain.TotalQuotaPlatform

var (
	ErrUserTotalDailyQuotaExhausted  = infraerrors.TooManyRequests("USER_TOTAL_DAILY_QUOTA_EXHAUSTED", "Daily usage quota exhausted.")
	ErrUserTotalWeeklyQuotaExhausted = infraerrors.TooManyRequests("USER_TOTAL_WEEKLY_QUOTA_EXHAUSTED", "Weekly usage quota exhausted.")
)

// 总额度复用配额缓存与日/周窗口；未配置时不限额。
func (s *BillingCacheService) checkUserTotalQuotaEligibility(ctx context.Context, userID int64) error {
	err := s.checkUserPlatformQuotaEligibility(ctx, userID, TotalQuotaPlatform)
	switch {
	case errors.Is(err, ErrUserPlatformDailyQuotaExhausted):
		return withWindowResetsMetadata(ErrUserTotalDailyQuotaExhausted, nextDailyReset(time.Now()))
	case errors.Is(err, ErrUserPlatformWeeklyQuotaExhausted):
		return withWindowResetsMetadata(ErrUserTotalWeeklyQuotaExhausted, nextWeeklyReset(time.Now()))
	case errors.Is(err, ErrUserPlatformMonthlyQuotaExhausted):
		return nil // 公司总额度只使用日、周窗口。
	default:
		return err
	}
}

// 在计费去重成功后累加 actual_cost；订阅和 composite 不豁免。
func recordUserTotalQuotaUsage(ctx context.Context, deps *billingDeps, userID int64, cost float64, syncDB bool) {
	if deps == nil || deps.billingCacheService == nil || deps.userPlatformQuotaRepo == nil || cost <= 0 || userID == 0 {
		return
	}
	if !deps.billingCacheService.HasUserPlatformQuotaLimit(ctx, userID, TotalQuotaPlatform) {
		return
	}
	deps.billingCacheService.IncrementUserPlatformQuotaUsage(userID, TotalQuotaPlatform, cost)
	if deps.cfg != nil && deps.cfg.Database.UserPlatformQuotaFlusherEnabled {
		return
	}
	write := func(dbCtx context.Context) {
		if err := deps.userPlatformQuotaRepo.IncrementUsageWithReset(dbCtx, userID, TotalQuotaPlatform, cost, time.Now().UTC()); err != nil {
			userPlatformQuotaDBIncrErrorTotal.Add(1)
			logger.LegacyPrintf("service.gateway", "ALERT: incr user total quota DB failed user=%d cost=%f: %v", userID, cost, err)
		}
	}
	if syncDB {
		write(ctx)
		return
	}
	dbCtx, cancel := detachedBillingContext(ctx)
	go func() {
		defer cancel()
		defer func() {
			if r := recover(); r != nil {
				logger.LegacyPrintf("service.gateway", "ALERT: panic in user total quota incr user=%d: %v", userID, r)
			}
		}()
		write(dbCtx)
	}()
}
