package repository

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const (
	promptRuleCacheKey  = "prompt_rules"
	promptRulePubSubKey = "prompt_rules_updated"
	promptRuleCacheTTL  = 24 * time.Hour
)

type promptRuleCache struct {
	rdb        *redis.Client
	localCache []*model.PromptRule
	localMu    sync.RWMutex
}

func NewPromptRuleCache(rdb *redis.Client) service.PromptRuleCache {
	return &promptRuleCache{rdb: rdb}
}

func (c *promptRuleCache) Get(ctx context.Context) ([]*model.PromptRule, bool) {
	c.localMu.RLock()
	if c.localCache != nil {
		rules := c.localCache
		c.localMu.RUnlock()
		return rules, true
	}
	c.localMu.RUnlock()

	data, err := c.rdb.Get(ctx, promptRuleCacheKey).Bytes()
	if err != nil {
		if err != redis.Nil {
			log.Printf("[PromptRuleCache] Failed to get from Redis: %v", err)
		}
		return nil, false
	}

	var rules []*model.PromptRule
	if err := json.Unmarshal(data, &rules); err != nil {
		log.Printf("[PromptRuleCache] Failed to unmarshal rules: %v", err)
		return nil, false
	}

	c.localMu.Lock()
	c.localCache = rules
	c.localMu.Unlock()

	return rules, true
}

func (c *promptRuleCache) Set(ctx context.Context, rules []*model.PromptRule) error {
	data, err := json.Marshal(rules)
	if err != nil {
		return err
	}

	if err := c.rdb.Set(ctx, promptRuleCacheKey, data, promptRuleCacheTTL).Err(); err != nil {
		return err
	}

	c.localMu.Lock()
	c.localCache = rules
	c.localMu.Unlock()

	return nil
}

func (c *promptRuleCache) Invalidate(ctx context.Context) error {
	c.localMu.Lock()
	c.localCache = nil
	c.localMu.Unlock()

	return c.rdb.Del(ctx, promptRuleCacheKey).Err()
}

func (c *promptRuleCache) NotifyUpdate(ctx context.Context) error {
	return c.rdb.Publish(ctx, promptRulePubSubKey, "refresh").Err()
}

func (c *promptRuleCache) SubscribeUpdates(ctx context.Context, handler func()) {
	go func() {
		sub := c.rdb.Subscribe(ctx, promptRulePubSubKey)
		defer func() { _ = sub.Close() }()

		ch := sub.Channel()
		for {
			select {
			case <-ctx.Done():
				return
			case msg := <-ch:
				if msg == nil {
					return
				}
				c.localMu.Lock()
				c.localCache = nil
				c.localMu.Unlock()

				handler()
			}
		}
	}()
}
