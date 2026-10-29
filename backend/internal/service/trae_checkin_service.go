package service

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"go.uber.org/zap"
)

// TraeCheckinService 是 TRAE 每日签到自动化（PR-3.4）。按账号 credentials 的
// auto_checkin=true 启用；9074（设备维度风控）不立即重试——退避并在下一次
// 尝试前「换代」设备 id。状态（下次尝试时间/代数/退避计数）持久化在
// credentials，重启不丢。
type TraeCheckinService struct {
	accountRepo       AccountRepository
	traeOAuthService  *TraeOAuthService
	traeTokenProvider *TraeTokenProvider
	interval          time.Duration
	stopCh            chan struct{}
}

const (
	traeCheckinDefaultInterval = 10 * time.Minute
	traeCheckinDayDelay        = 24 * time.Hour
	traeCheckinJitter          = 30 * time.Minute
	traeCheckinBaseBackoff     = 15 * time.Minute
	traeCheckinMaxBackoff      = 6 * time.Hour
	traeCheckinMaxRetries      = 6
)

func NewTraeCheckinService(accountRepo AccountRepository, oauthService *TraeOAuthService, tokenProvider *TraeTokenProvider) *TraeCheckinService {
	return &TraeCheckinService{
		accountRepo:       accountRepo,
		traeOAuthService:  oauthService,
		traeTokenProvider: tokenProvider,
		interval:          traeCheckinDefaultInterval,
		stopCh:            make(chan struct{}),
	}
}

// Start 启动后台循环（幂等：重复调用只起一个循环）。
func (s *TraeCheckinService) Start() {
	if s == nil {
		return
	}
	go func() {
		ticker := time.NewTicker(s.interval)
		defer ticker.Stop()
		for {
			s.sweep(context.Background())
			select {
			case <-s.stopCh:
				return
			case <-ticker.C:
			}
		}
	}()
}

// Stop 结束后台循环。
func (s *TraeCheckinService) Stop() {
	if s == nil {
		return
	}
	select {
	case <-s.stopCh:
	default:
		close(s.stopCh)
	}
}

// sweep 扫描全部 trae OAuth 账号，对到期且启用 auto_checkin 的执行签到。
func (s *TraeCheckinService) sweep(ctx context.Context) {
	if s.accountRepo == nil || s.traeTokenProvider == nil || s.traeOAuthService == nil {
		return
	}
	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformTrae)
	if err != nil {
		return
	}
	for i := range accounts {
		account := &accounts[i]
		if account.Type != AccountTypeOAuth || !isTraeCredentialTrue(account, "auto_checkin") {
			continue
		}
		if !traeCheckinDue(account) {
			continue
		}
		s.checkinAccount(ctx, account)
	}
}

// checkinAccount 执行一次签到并按结果推进状态（下次时间/代数/退避）。
func (s *TraeCheckinService) checkinAccount(ctx context.Context, account *Account) {
	identity := account.GetCredential("user_id")
	if identity == "" {
		return
	}
	token, err := s.traeTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		s.scheduleRetry(ctx, account, 0, "get token: "+err.Error())
		return
	}
	client := s.traeOAuthService.UpstreamClient(ctx, account)
	generation := traeCheckinGeneration(account)

	status, err := client.CheckinStatus(ctx, token, identity, generation)
	if err != nil {
		s.scheduleRetry(ctx, account, 0, "status: "+err.Error())
		return
	}
	if !status.Enabled || status.CheckedIn {
		s.scheduleNext(ctx, account, generation, time.Now().Add(traeCheckinDayDelay+jitter()), "already checked in")
		return
	}
	result, err := client.ClaimCheckin(ctx, token, identity, generation)
	if err != nil {
		s.scheduleRetry(ctx, account, generation, "claim: "+err.Error())
		return
	}
	switch {
	case result.RateLimited:
		// 9074：设备维度风控。退避 + 换代（下一次用新设备 id 尝试）。
		retries := traeCheckinRetries(account) + 1
		if retries > traeCheckinMaxRetries {
			// 退避穷尽：当天放弃，明天从新一代继续。
			s.scheduleNext(ctx, account, generation+1, time.Now().Add(traeCheckinDayDelay), "9074 retries exhausted")
			return
		}
		backoff := traeCheckinBaseBackoff << retries
		if backoff > traeCheckinMaxBackoff || backoff <= 0 {
			backoff = traeCheckinMaxBackoff
		}
		s.persistCheckinState(ctx, account, map[string]any{
			"checkin_generation": generation + 1,
			"checkin_retries":    retries,
			"checkin_next_at":    time.Now().Add(backoff).Format(time.RFC3339),
			"checkin_note":       "9074 rate limited",
		})
	case result.AlreadyDone:
		s.scheduleNext(ctx, account, generation, time.Now().Add(traeCheckinDayDelay+jitter()), "already done")
	case result.Claimed:
		s.scheduleNext(ctx, account, generation, time.Now().Add(traeCheckinDayDelay+jitter()), "claimed")
		logger.L().Info("trae checkin claimed",
			zap.Int64("account_id", account.ID),
			zap.Float64("credits", result.Credits))
	default:
		s.scheduleRetry(ctx, account, generation, result.RawMessage)
	}
}

func (s *TraeCheckinService) scheduleNext(ctx context.Context, account *Account, generation int, next time.Time, note string) {
	s.persistCheckinState(ctx, account, map[string]any{
		"checkin_generation": generation,
		"checkin_retries":    0,
		"checkin_next_at":    next.Format(time.RFC3339),
		"checkin_note":       note,
	})
}

func (s *TraeCheckinService) scheduleRetry(ctx context.Context, account *Account, generation int, reason string) {
	retries := traeCheckinRetries(account) + 1
	if retries > traeCheckinMaxRetries {
		s.scheduleNext(ctx, account, generation, time.Now().Add(traeCheckinDayDelay), reason)
		return
	}
	backoff := traeCheckinBaseBackoff << retries
	if backoff > traeCheckinMaxBackoff || backoff <= 0 {
		backoff = traeCheckinMaxBackoff
	}
	s.persistCheckinState(ctx, account, map[string]any{
		"checkin_generation": generation,
		"checkin_retries":    retries,
		"checkin_next_at":    time.Now().Add(backoff).Format(time.RFC3339),
		"checkin_note":       reason,
	})
}

func (s *TraeCheckinService) persistCheckinState(ctx context.Context, account *Account, state map[string]any) {
	updated := MergeCredentials(account.Credentials, state)
	account.Credentials = shallowCopyMap(updated)
	if s.accountRepo != nil {
		if err := s.accountRepo.Update(ctx, account); err != nil {
			logger.L().Warn("trae checkin state persist failed",
				zap.Int64("account_id", account.ID), zap.Error(err))
		}
	}
}

func traeCheckinDue(account *Account) bool {
	nextAt := strings.TrimSpace(account.GetCredential("checkin_next_at"))
	if nextAt == "" {
		return true // 从未签到：立即安排
	}
	next, err := time.Parse(time.RFC3339, nextAt)
	if err != nil {
		return true
	}
	return time.Now().After(next)
}

func traeCheckinGeneration(account *Account) int {
	value := strings.TrimSpace(account.GetCredential("checkin_generation"))
	generation, err := strconv.Atoi(value)
	if err != nil || generation < 0 {
		return 0
	}
	return generation
}

func traeCheckinRetries(account *Account) int {
	value := strings.TrimSpace(account.GetCredential("checkin_retries"))
	retries, err := strconv.Atoi(value)
	if err != nil || retries < 0 {
		return 0
	}
	return retries
}

func jitter() time.Duration {
	return time.Duration(traeRandInt64(int64(traeCheckinJitter)))
}

func traeRandInt64(max int64) int64 {
	if max <= 0 {
		return 0
	}
	return time.Now().UnixNano() % max
}
