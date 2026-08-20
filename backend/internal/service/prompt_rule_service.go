package service

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
)

type PromptRuleListFilters struct {
	Search string
}

// PromptRuleRepository 定义提示词注入规则的数据访问接口
type PromptRuleRepository interface {
	List(ctx context.Context) ([]*model.PromptRule, error)
	ListPage(ctx context.Context, params pagination.PaginationParams, filters PromptRuleListFilters) ([]*model.PromptRule, *pagination.PaginationResult, error)
	GetByID(ctx context.Context, id int64) (*model.PromptRule, error)
	Create(ctx context.Context, rule *model.PromptRule) (*model.PromptRule, error)
	Update(ctx context.Context, rule *model.PromptRule) (*model.PromptRule, error)
	Delete(ctx context.Context, id int64) error
}

// PromptRuleCache 定义提示词注入规则的缓存接口
type PromptRuleCache interface {
	Get(ctx context.Context) ([]*model.PromptRule, bool)
	Set(ctx context.Context, rules []*model.PromptRule) error
	Invalidate(ctx context.Context) error
	NotifyUpdate(ctx context.Context) error
	SubscribeUpdates(ctx context.Context, handler func())
}

// PromptRuleService 提示词注入规则服务
type PromptRuleService struct {
	repo  PromptRuleRepository
	cache PromptRuleCache

	localCache   []*model.PromptRule
	localCacheMu sync.RWMutex
}

// NewPromptRuleService 创建提示词注入规则服务
func NewPromptRuleService(
	repo PromptRuleRepository,
	cache PromptRuleCache,
) *PromptRuleService {
	svc := &PromptRuleService{
		repo:  repo,
		cache: cache,
	}

	ctx := context.Background()
	if err := svc.reloadRulesFromDB(ctx); err != nil {
		logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to load rules from DB on startup: %v", err)
		if fallbackErr := svc.refreshLocalCache(ctx); fallbackErr != nil {
			logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to load rules from cache fallback on startup: %v", fallbackErr)
		}
	}

	if cache != nil {
		cache.SubscribeUpdates(ctx, func() {
			if err := svc.refreshLocalCache(context.Background()); err != nil {
				logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to refresh cache on notification: %v", err)
			}
		})
	}

	return svc
}

func (s *PromptRuleService) List(ctx context.Context) ([]*model.PromptRule, error) {
	return s.repo.List(ctx)
}

func (s *PromptRuleService) ListPage(ctx context.Context, params pagination.PaginationParams, filters PromptRuleListFilters) ([]*model.PromptRule, *pagination.PaginationResult, error) {
	return s.repo.ListPage(ctx, params, filters)
}

func (s *PromptRuleService) GetByID(ctx context.Context, id int64) (*model.PromptRule, error) {
	return s.repo.GetByID(ctx, id)
}

func (s *PromptRuleService) Create(ctx context.Context, rule *model.PromptRule) (*model.PromptRule, error) {
	if err := rule.Validate(); err != nil {
		return nil, err
	}

	created, err := s.repo.Create(ctx, rule)
	if err != nil {
		return nil, err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	s.invalidateAndNotify(refreshCtx)

	return created, nil
}

func (s *PromptRuleService) Update(ctx context.Context, rule *model.PromptRule) (*model.PromptRule, error) {
	if err := rule.Validate(); err != nil {
		return nil, err
	}

	updated, err := s.repo.Update(ctx, rule)
	if err != nil {
		return nil, err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	s.invalidateAndNotify(refreshCtx)

	return updated, nil
}

func (s *PromptRuleService) Delete(ctx context.Context, id int64) error {
	if err := s.repo.Delete(ctx, id); err != nil {
		return err
	}

	refreshCtx, cancel := s.newCacheRefreshContext()
	defer cancel()
	s.invalidateAndNotify(refreshCtx)

	return nil
}

// GetMatchingRules 根据分组和模型获取匹配的规则，返回 prepend、append 和 replace 三组
func (s *PromptRuleService) GetMatchingRules(groupID *int64, modelID string) (prepend, append_, replace []*model.PromptRule) {
	rules := s.getCachedRules()
	if len(rules) == 0 {
		return nil, nil, nil
	}

	for _, rule := range rules {
		if !rule.Enabled {
			continue
		}
		if !s.groupMatches(rule, groupID) {
			continue
		}
		if !s.modelMatches(rule, modelID) {
			continue
		}
		switch rule.Action {
		case model.PromptActionPrepend:
			prepend = append(prepend, rule)
		case model.PromptActionReplace:
			replace = append(replace, rule)
		default:
			append_ = append(append_, rule)
		}
	}
	return prepend, append_, replace
}

func (s *PromptRuleService) groupMatches(rule *model.PromptRule, groupID *int64) bool {
	if len(rule.GroupIDs) == 0 {
		return false
	}
	if groupID == nil {
		return false
	}
	for _, id := range rule.GroupIDs {
		if id == *groupID {
			return true
		}
	}
	return false
}

func (s *PromptRuleService) modelMatches(rule *model.PromptRule, modelID string) bool {
	if len(rule.ModelIDs) == 0 {
		return true
	}
	for _, id := range rule.ModelIDs {
		if id == modelID {
			return true
		}
	}
	return false
}

func (s *PromptRuleService) getCachedRules() []*model.PromptRule {
	s.localCacheMu.RLock()
	rules := s.localCache
	s.localCacheMu.RUnlock()

	if rules != nil {
		return rules
	}

	ctx := context.Background()
	if err := s.refreshLocalCache(ctx); err != nil {
		logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to refresh cache: %v", err)
		return nil
	}

	s.localCacheMu.RLock()
	defer s.localCacheMu.RUnlock()
	return s.localCache
}

func (s *PromptRuleService) refreshLocalCache(ctx context.Context) error {
	if s.cache != nil {
		if rules, ok := s.cache.Get(ctx); ok {
			s.setLocalCache(rules)
			return nil
		}
	}
	return s.reloadRulesFromDB(ctx)
}

func (s *PromptRuleService) reloadRulesFromDB(ctx context.Context) error {
	rules, err := s.repo.List(ctx)
	if err != nil {
		return err
	}

	if s.cache != nil {
		if err := s.cache.Set(ctx, rules); err != nil {
			logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to set cache: %v", err)
		}
	}

	s.setLocalCache(rules)
	return nil
}

func (s *PromptRuleService) setLocalCache(rules []*model.PromptRule) {
	sorted := make([]*model.PromptRule, len(rules))
	copy(sorted, rules)
	sort.Slice(sorted, func(i, j int) bool {
		return sorted[i].Order < sorted[j].Order
	})

	s.localCacheMu.Lock()
	s.localCache = sorted
	s.localCacheMu.Unlock()
}

func (s *PromptRuleService) newCacheRefreshContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 3*time.Second)
}

func (s *PromptRuleService) invalidateAndNotify(ctx context.Context) {
	if s.cache != nil {
		if err := s.cache.Invalidate(ctx); err != nil {
			logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to invalidate cache: %v", err)
		}
	}

	if err := s.reloadRulesFromDB(ctx); err != nil {
		logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to refresh local cache: %v", err)
		s.localCacheMu.Lock()
		s.localCache = nil
		s.localCacheMu.Unlock()
	}

	if s.cache != nil {
		if err := s.cache.NotifyUpdate(ctx); err != nil {
			logger.LegacyPrintf("service.prompt_rule", "[PromptRuleService] Failed to notify cache update: %v", err)
		}
	}
}
