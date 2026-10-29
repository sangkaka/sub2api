package service

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
	"github.com/gin-gonic/gin"
)

// SetTraeTokenProvider 注入 TRAE token provider 与可选上游客户端（wire 装配
// 时调用；client 为 nil 用默认端点，测试注入 httptest 端点）。
func (s *AccountTestService) SetTraeTokenProvider(provider *TraeTokenProvider, oauthService *TraeOAuthService, client *trae.Client) {
	s.traeTokenProvider = provider
	s.traeOAuthService = oauthService
	if client != nil {
		s.traeClient = client
	}
}

func (s *AccountTestService) traeUpstreamClient() *trae.Client {
	if s.traeClient != nil {
		return s.traeClient
	}
	return &trae.Client{}
}

// traeClientForAccount 优先测试注入的端点覆盖，生产经 oauth service 按账号
// 解析代理。
func (s *AccountTestService) traeClientForAccount(ctx context.Context, account *Account) *trae.Client {
	if s.traeClient != nil {
		return s.traeClient
	}
	if s.traeOAuthService != nil {
		return s.traeOAuthService.UpstreamClient(ctx, account)
	}
	return &trae.Client{}
}

// testTraeAccountConnection 探活 TRAE 账号：GetUserInfo（身份/凭据有效性）+
// entitlement（并发槽位，写回账号 concurrency）+ 一次最小会话（真实链路）。
func (s *AccountTestService) testTraeAccountConnection(c *gin.Context, account *Account, modelID string) error {
	ctx := c.Request.Context()
	if s.traeTokenProvider == nil {
		return s.sendErrorAndEnd(c, "TRAE token provider not configured")
	}
	token, err := s.traeTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Failed to get TRAE token: %s", err.Error()))
	}

	client := s.traeClientForAccount(ctx, account)
	userInfo, err := client.GetUserInfo(ctx, token)
	if err != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("GetUserInfo failed: %s", err.Error()))
	}

	s.sendEvent(c, TestEvent{Type: "test_start", Model: strings.TrimSpace(modelID)})

	// 并发槽位：entitlement quota.solo_agent_parallel_limit 写回账号。
	if ent, entErr := client.FetchEntitlementUsage(ctx, token); entErr == nil && ent.ParallelLimit > 0 && account.Concurrency != ent.ParallelLimit {
		account.Concurrency = ent.ParallelLimit
		if updErr := s.accountRepo.Update(ctx, account); updErr != nil {
			s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("Warning: failed to persist concurrency=%d: %s\n", ent.ParallelLimit, updErr.Error())})
		}
	}

	display := userInfo.ScreenName
	if display == "" {
		display = userInfo.UserID
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("TRAE account verified (user: %s, parallel limit captured).\n", display)})

	// 最小会话探活：建会话 → 等首个模型事件 → stop。
	testModel := strings.TrimSpace(modelID)
	if testModel == "" {
		if mapped := account.GetMappedModel("GLM-5.3"); strings.TrimSpace(mapped) != "" {
			testModel = mapped
		} else {
			testModel = "GLM-5.3"
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	if probeErr := s.probeTraeSession(probeCtx, client, token, account, testModel); probeErr != nil {
		return s.sendErrorAndEnd(c, fmt.Sprintf("Session probe failed: %s", probeErr.Error()))
	}
	s.sendEvent(c, TestEvent{Type: "content", Text: fmt.Sprintf("Model %s responded successfully.\n", testModel)})
	s.sendEvent(c, TestEvent{Type: "test_complete", Success: true})
	return nil
}

// probeTraeSession 建立一次最小 Remote 会话并等待首个模型事件（不要求完整
// 回答，验证的是两步协议链路本身）。
func (s *AccountTestService) probeTraeSession(ctx context.Context, client *trae.Client, token string, account *Account, model string) error {
	descriptors, err := client.FetchModels(ctx, token)
	if err != nil {
		return fmt.Errorf("fetch models: %w", err)
	}
	var descriptor *trae.ModelDescriptor
	for _, candidate := range descriptors {
		if candidate.ConfigName == model {
			descriptor = candidate
			break
		}
	}
	if descriptor == nil {
		return fmt.Errorf("model %q is not available for this account", model)
	}
	handle, err := client.CreateSession(ctx, token, trae.SessionRequest{
		Query:        trae.FlattenMessages([]trae.ChatMessage{{Role: "user", Content: []byte(`"ping"`)}}),
		Model:        model,
		Descriptor:   descriptor,
		CommonParams: BuildTraeCommonParamsForProbe(account, model),
	})
	if err != nil {
		return err
	}
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = client.StopSession(stopCtx, token, handle.SessionID)
		cancel()
	}()
	events, errCh := client.StreamEvents(ctx, token, handle.SessionID, handle.MessageID)
	for events != nil || errCh != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			switch ev.Name {
			case trae.EventPlanItem, trae.EventMessage, trae.EventTokenUsage, trae.EventDone:
				return nil // 首个模型事件即证明链路可用
			case trae.EventError:
				code, message := trae.ExtractErrorDetails(ev.Data)
				return fmt.Errorf("upstream error [%s]: %s", code, message)
			}
		case streamErr, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if streamErr != nil {
				return streamErr
			}
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return fmt.Errorf("no model event observed before stream close")
}

// fetchTraeUpstreamModels 拉取账号可用的 TRAE 模型目录（config_name 清单）。
func (s *AccountTestService) fetchTraeUpstreamModels(ctx context.Context, account *Account) ([]string, error) {
	if s.traeTokenProvider == nil {
		return nil, fmt.Errorf("trAE token provider not configured")
	}
	token, err := s.traeTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, err
	}
	descriptors, err := s.traeUpstreamClient().FetchModels(ctx, token)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(descriptors))
	for _, descriptor := range descriptors {
		names = append(names, descriptor.ConfigName)
	}
	return names, nil
}
