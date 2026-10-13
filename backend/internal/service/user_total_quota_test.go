//go:build unit

package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/timezone"
)

type totalQuotaTestCache struct {
	*fakeZeroQuotaCache
	entry     *UserPlatformQuotaCacheEntry
	platforms []string
}

func (f *totalQuotaTestCache) GetUserBalance(context.Context, int64) (float64, error) {
	return 100, nil
}

func (f *totalQuotaTestCache) GetUserPlatformQuotaCache(_ context.Context, _ int64, platform string) (*UserPlatformQuotaCacheEntry, bool, error) {
	f.platforms = append(f.platforms, platform)
	if platform == "total" {
		return f.entry, true, nil
	}
	return nil, false, nil
}

func TestTotalQuotaEligibility_AcrossPlatformsAndSubscriptions(t *testing.T) {
	for _, platform := range []string{"anthropic", "openai", "grok", "antigravity", ""} {
		for _, subscribed := range []bool{false, true} {
			t.Run(platform+map[bool]string{true: "/subscription", false: "/balance"}[subscribed], func(t *testing.T) {
				now := time.Now()
				day, week := timezone.StartOfDay(now), timezone.StartOfWeek(now)
				limit := 1.0
				cache := &totalQuotaTestCache{fakeZeroQuotaCache: &fakeZeroQuotaCache{}, entry: &UserPlatformQuotaCacheEntry{
					SchemaVersion:  UserPlatformQuotaCacheSchemaV1,
					WeeklyUsageUSD: 1, WeeklyLimitUSD: &limit,
					DailyWindowStart: &day, WeeklyWindowStart: &week, MonthlyWindowStart: &now,
				}}
				s := &BillingCacheService{cfg: &config.Config{}, cache: cache, userPlatformQuotaRepo: &fakeQuotaRepo{}}
				var group *Group
				var subscription *UserSubscription
				if subscribed {
					group = &Group{ID: 10, SubscriptionType: "subscription", Status: "active"}
					subscription = &UserSubscription{Status: "active"}
				}
				err := s.CheckBillingEligibility(context.Background(), &User{ID: 7}, nil, group, subscription, platform)
				if err == nil || !errors.Is(err, ErrUserTotalWeeklyQuotaExhausted) {
					t.Fatalf("shared weekly quota must block, got %v", err)
				}
			})
		}
	}
}

func TestTotalQuotaPlatform_AllowedWithoutRegisteringProvider(t *testing.T) {
	if !IsAllowedQuotaPlatform("total") {
		t.Fatal("admin quota API must accept total")
	}
	for _, platform := range AllowedQuotaPlatforms {
		if platform == "total" {
			t.Fatal("total must not become a provider or registration default")
		}
	}
}
