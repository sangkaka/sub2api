package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
	"github.com/gin-gonic/gin"
)

const (
	// traeRateLimitCooldown 是 4011/429/3004/4008 的账号冷却时长。
	traeRateLimitCooldown = 60 * time.Second
	// traeCatalogCacheTTL 是账号模型目录（descriptor）的进程内缓存时长。
	traeCatalogCacheTTL = 10 * time.Minute
	// traeUpstreamEndpoint 记录到 OpenAIForwardResult 供运维展示。
	traeUpstreamEndpoint = "/chat_sessions"
	// traeIDEUpstreamEndpoint 是 IDE/TraeWork 兼容会话端点（企业版账号的
	// 实际执行链，kimi-k3.1-pro 等 Remote 拒绝模型的回落路径）。
	traeIDEUpstreamEndpoint = "/api/agent/v3/llm_utils_chat"
	// traeStreamHeadKey 标记本请求的 trae SSE 头（+role 帧）已写：Remote
	// 回合内回落 IDE 时续写同一响应流而不重复写头。
	traeStreamHeadKey = "trae_sse_head_written"
)

// traeRuntimeCatalog 缓存账号级模型目录（含 descriptor）。descriptor 必须随
// 会话回带，逐请求拉取不可接受；上游目录随版本变动，按 TTL 失效。
type traeRuntimeCatalog struct {
	mu      sync.Mutex
	entries map[int64]traeCatalogEntry
}

type traeCatalogEntry struct {
	descriptors map[string]*trae.ModelDescriptor
	expiry      time.Time
}

func (m *traeRuntimeCatalog) get(accountID int64) map[string]*trae.ModelDescriptor {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.entries[accountID]
	if !ok || time.Now().After(entry.expiry) {
		return nil
	}
	return entry.descriptors
}

func (m *traeRuntimeCatalog) put(accountID int64, descriptors []*trae.ModelDescriptor) {
	byName := make(map[string]*trae.ModelDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		byName[descriptor.ConfigName] = descriptor
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.entries == nil {
		m.entries = map[int64]traeCatalogEntry{}
	}
	m.entries[accountID] = traeCatalogEntry{descriptors: byName, expiry: time.Now().Add(traeCatalogCacheTTL)}
}

// forwardTraeChatCompletions 是 TRAE 平台的 Chat Completions 转发分支：
// OpenAI 入站 → 扁平化 query → Remote 两步会话 → SSE 快照差分 → OpenAI
// 增量回写。流式/非流式共用同一翻译器。
func (s *OpenAIGatewayService) forwardTraeChatCompletions(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	startTime := time.Now()

	var chatReq apicompat.ChatCompletionsRequest
	if err := json.Unmarshal(body, &chatReq); err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "parse chat completions request: "+err.Error())
		return nil, err
	}
	originalModel := chatReq.Model
	if originalModel == "" {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, errors.New("missing model in request")
	}
	billingModel := resolveOpenAIForwardModel(account, originalModel, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	SetOpsUpstreamModel(c, upstreamModel)
	SetActualOpenAIUpstreamEndpoint(c, traeUpstreamEndpoint)

	if s.traeTokenProvider == nil {
		writeChatCompletionsError(c, http.StatusBadGateway, "api_error", "trAE token provider is not configured")
		return nil, errors.New("trae token provider is nil")
	}

	token, err := s.traeTokenProvider.GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("trae get access token: %w", err)
	}

	client := s.traeClientForAccount(ctx, account)
	// IDE 优先路由：llm_utils_chat 是裸 LLM 端点（无 Remote 会话的 agent
	// 脚手架——沙箱/规划/每请求 ~23.5K 系统注入，琐碎问题实测 20~41s vs
	// IDE 2~4s），且提示缓存只在 IDE 生效。图片附件与 1M Max 是 Remote 侧
	// 能力，带这类载荷或已确认 IDE 不服务的模型（4001/4023 缓存）走 Remote。
	mustRemote := len(trae.MediaQueryParts(toTraeMessages(chatReq.Messages))) > 0 ||
		isTraeCredentialTrue(account, "max_mode") ||
		s.traePrefersRemote(account.ID, upstreamModel)
	if !mustRemote {
		result, err := s.forwardTraeIDETurn(ctx, c, client, token, account, chatReq, originalModel, billingModel, upstreamModel, startTime)
		if err == nil || !isTraeRemoteFallbackError(err) {
			return result, err
		}
		// IDE 首事件即拒（模型不可服务，未写字节）：记偏好，改走 Remote。
		s.traeMarkRemotePreferred(account.ID, upstreamModel)
		SetActualOpenAIUpstreamEndpoint(c, traeUpstreamEndpoint)
	}
	descriptor, err := s.traeModelDescriptor(ctx, client, account, token, upstreamModel)
	if err != nil {
		writeChatCompletionsError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, err
	}

	messages := toTraeMessages(chatReq.Messages)
	// 客户端工具运行时提示（PR-3.1 文本桥）：有 tools 定义时注入 query。
	if toolPrompt := trae.BuildToolRuntimePrompt(toTraeToolDefinitions(chatReq.Tools), toolChoiceString(chatReq.ToolChoice), chatReq.ParallelToolCalls); toolPrompt != "" {
		messages = append([]trae.ChatMessage{{Role: "system", Content: json.RawMessage(`"` + escapeJSONString(toolPrompt) + `"`)}}, messages...)
	}
	// 图片附件（PR-3.2）：作为 renderer media parts 追加到 query。
	mediaParts := trae.MediaQueryParts(messages)
	// 1M Max（PR-3.3）：账号开启且 descriptor 显式声明档位时钉住。
	var maxModeFields map[string]any
	if isTraeCredentialTrue(account, "max_mode") {
		maxModeFields = descriptor.MaxModeFields()
	}
	query := trae.FlattenMessages(messages)
	effort := trae.ClampReasoningEffort(trae.NormalizeReasoningEffort(chatReq.ReasoningEffort), descriptor)
	commonParams := BuildTraeCommonParams(account, upstreamModel)

	// 建会话；token 失效强刷一次后重试。
	handle, err := client.CreateSession(ctx, token, trae.SessionRequest{
		Query:           query,
		ExtraQueryParts: mediaParts,
		Model:           upstreamModel,
		Descriptor:      descriptor,
		ReasoningEffort: effort,
		CommonParams:    commonParams,
		MaxModeFields:   maxModeFields,
	})
	if err != nil {
		if ue := trae.AsUpstreamError(err); ue != nil && ue.Kind == trae.ErrorKindAuth && s.traeTokenProvider != nil {
			if refreshed, rerr := s.traeTokenProvider.ForceRefreshAccessToken(ctx, account); rerr == nil {
				handle, err = client.CreateSession(ctx, refreshed, trae.SessionRequest{
					Query:           query,
					Model:           upstreamModel,
					Descriptor:      descriptor,
					ReasoningEffort: effort,
					CommonParams:    commonParams,
				})
			}
		}
		if err != nil {
			s.markTraeAccountRateLimited(ctx, account, err)
			return nil, fmt.Errorf("trae create session: %w", err)
		}
	}

	// 会话收尾尽力而为：客户端断开/正常结束都向上游发 stop。
	defer func() {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		_ = client.StopSession(stopCtx, token, handle.SessionID)
		cancel()
	}()

	eventsFn := func() (<-chan trae.Event, <-chan error) {
		return client.StreamEvents(ctx, token, handle.SessionID, handle.MessageID)
	}
	translator := trae.NewStreamTranslator(originalModel)
	result, err := s.runTraeTurn(ctx, c, account, eventsFn, translator, chatReq, originalModel, billingModel, upstreamModel, effort, traeUpstreamEndpoint, startTime)
	if err != nil && isTraeIDEFallbackError(err) {
		// Remote 对该模型固定 1005（kimi-k3.1-pro 实测）：回落 IDE 端点并
		// 缓存偏好，后续请求直连。
		s.traeMarkIDEPreferred(account.ID, upstreamModel)
		return s.forwardTraeIDETurn(ctx, c, client, token, account, chatReq, originalModel, billingModel, upstreamModel, startTime)
	}
	return result, err
}

// runTraeTurn 按请求形态（流式/缓冲）执行一个回合。
func (s *OpenAIGatewayService) runTraeTurn(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	eventsFn traeEventSource,
	translator traeTurnTranslator,
	chatReq apicompat.ChatCompletionsRequest,
	originalModel, billingModel, upstreamModel, effort, upstreamEndpoint string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	if chatReq.Stream {
		return s.streamTraeTurn(ctx, c, account, eventsFn, translator, chatReq, originalModel, billingModel, upstreamModel, effort, upstreamEndpoint, startTime)
	}
	return s.bufferedTraeTurn(ctx, c, account, eventsFn, translator, chatReq, originalModel, billingModel, upstreamModel, effort, upstreamEndpoint, startTime)
}

// forwardTraeIDETurn 走 llm_utils_chat（IDE/TraeWork 兼容会话）执行回合：
// 原生工具字段而非文本桥；无两步会话与 stop 收尾。llm_utils_chat 有两个
// 入口域名且模型子集互补（2026-10-30 双主机实测：企业域名入口服务
// kimi/glm 系、DeepSeek 系 4023；模型网关入口服务 DeepSeek/glm 系、
// kimi 系 1005），按 (账号,模型) 缓存生效入口，未命中时依次探测。
func (s *OpenAIGatewayService) forwardTraeIDETurn(
	ctx context.Context,
	c *gin.Context,
	client *trae.Client,
	token string,
	account *Account,
	chatReq apicompat.ChatCompletionsRequest,
	originalModel, billingModel, upstreamModel string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	SetActualOpenAIUpstreamEndpoint(c, traeIDEUpstreamEndpoint)
	maxTokens := 0
	if chatReq.MaxCompletionTokens != nil && *chatReq.MaxCompletionTokens > 0 {
		maxTokens = *chatReq.MaxCompletionTokens
	} else if chatReq.MaxTokens != nil && *chatReq.MaxTokens > 0 {
		maxTokens = *chatReq.MaxTokens
	}
	ideReq := trae.IDEChatRequest{
		Messages:          toTraeMessages(chatReq.Messages),
		ConfigName:        upstreamModel,
		UserID:            account.GetCredential("user_id"),
		MaxTokens:         maxTokens,
		Tools:             toTraeToolDefinitions(chatReq.Tools),
		ToolChoice:        trae.NormalizeIDEToolChoice(chatReq.ToolChoice),
		ParallelToolCalls: chatReq.ParallelToolCalls,
	}
	var lastErr error
	for _, host := range s.traeIDEHostCandidates(client, account, upstreamModel) {
		at := client.WithCloudIDEHost(host)
		eventsFn := func() (<-chan trae.Event, <-chan error) {
			return at.IDEChat(ctx, token, ideReq)
		}
		result, err := s.runTraeTurn(ctx, c, account, eventsFn, trae.NewIDETranslator(originalModel), chatReq, originalModel, billingModel, upstreamModel, "", traeIDEUpstreamEndpoint, startTime)
		if err == nil {
			s.traeMarkIDEHost(account.ID, upstreamModel, host)
			return result, nil
		}
		lastErr = err
		if !isTraeRemoteFallbackError(err) {
			// 非入口级错误（限流/认证/传输）换域名无意义，原样上抛。
			return result, err
		}
		// 模型在该入口不可服务（首事件即拒，未写字节）：试下一入口。
	}
	// 双入口都不服务该模型：保留哨兵错误，由上层回落 Remote。
	return nil, lastErr
}

// traeIDEHostCandidates 返回 llm_utils_chat 的入口尝试顺序：生效缓存
// 优先，否则企业域名（身份端点同源，kimi/glm 实测可用）在前、模型网关
// （DeepSeek 系唯一可用入口，1.5s 首文本 + 提示缓存）在后，去重。
func (s *OpenAIGatewayService) traeIDEHostCandidates(client *trae.Client, account *Account, upstreamModel string) []string {
	gateway := client.ModelGatewayHost
	if gateway == "" {
		gateway = trae.DefaultModelGatewayHost
	}
	primary := client.CloudIDEHost
	seen := map[string]bool{}
	var out []string
	for _, host := range []string{s.traePreferredIDEHost(account.ID, upstreamModel), primary, gateway} {
		host = strings.TrimSpace(strings.TrimRight(host, "/"))
		if host == "" || seen[host] {
			continue
		}
		seen[host] = true
		out = append(out, host)
	}
	return out
}

// traeIDEHostPref 缓存「账号+模型 → 生效的 llm_utils_chat 入口域名」，
// 进程级、30 分钟过期（与通道偏好同 TTL），避免每请求重复探测。
var traeIDEHostPref struct {
	mu     sync.Mutex
	hosts  map[string]string
	expiry map[string]time.Time
}

func (s *OpenAIGatewayService) traeMarkIDEHost(accountID int64, model, host string) {
	key := fmt.Sprintf("%d|%s", accountID, model)
	traeIDEHostPref.mu.Lock()
	defer traeIDEHostPref.mu.Unlock()
	if traeIDEHostPref.hosts == nil {
		traeIDEHostPref.hosts = map[string]string{}
		traeIDEHostPref.expiry = map[string]time.Time{}
	}
	traeIDEHostPref.hosts[key] = host
	traeIDEHostPref.expiry[key] = time.Now().Add(traeIDEPreferredTTL)
}

func (s *OpenAIGatewayService) traePreferredIDEHost(accountID int64, model string) string {
	key := fmt.Sprintf("%d|%s", accountID, model)
	traeIDEHostPref.mu.Lock()
	defer traeIDEHostPref.mu.Unlock()
	if expiry, ok := traeIDEHostPref.expiry[key]; ok && time.Now().Before(expiry) {
		return traeIDEHostPref.hosts[key]
	}
	return ""
}

// traeIDEPreferred 缓存「账号+模型 → Remote 固定 1005」的偏好，避免每个
// 请求都先付一次失败的 Remote 会话。进程级，30 分钟过期后重探 Remote。
var traeIDEPreferred struct {
	mu      sync.Mutex
	expires map[string]time.Time
}

const traeIDEPreferredTTL = 30 * time.Minute

func (s *OpenAIGatewayService) traeMarkIDEPreferred(accountID int64, model string) {
	key := fmt.Sprintf("%d|%s", accountID, model)
	traeIDEPreferred.mu.Lock()
	defer traeIDEPreferred.mu.Unlock()
	if traeIDEPreferred.expires == nil {
		traeIDEPreferred.expires = map[string]time.Time{}
	}
	traeIDEPreferred.expires[key] = time.Now().Add(traeIDEPreferredTTL)
	// 偏好互斥：确认 IDE 可服务即清除反向偏好。
	traeRemotePreferred.mu.Lock()
	delete(traeRemotePreferred.expires, key)
	traeRemotePreferred.mu.Unlock()
}

func (s *OpenAIGatewayService) traePrefersIDE(accountID int64, model string) bool {
	key := fmt.Sprintf("%d|%s", accountID, model)
	traeIDEPreferred.mu.Lock()
	defer traeIDEPreferred.mu.Unlock()
	expiry, ok := traeIDEPreferred.expires[key]
	return ok && time.Now().Before(expiry)
}

// traeRemotePreferred 缓存「账号+模型 → IDE 不服务（4001/4023）」的偏好。
// IDE 优先路由下，仅 Remote 可服务的模型（DeepSeek 系实测）每进程只付
// 一次 ~0.4s 的快速失败探测，之后直连 Remote。
var traeRemotePreferred struct {
	mu      sync.Mutex
	expires map[string]time.Time
}

func (s *OpenAIGatewayService) traeMarkRemotePreferred(accountID int64, model string) {
	key := fmt.Sprintf("%d|%s", accountID, model)
	traeRemotePreferred.mu.Lock()
	defer traeRemotePreferred.mu.Unlock()
	if traeRemotePreferred.expires == nil {
		traeRemotePreferred.expires = map[string]time.Time{}
	}
	traeRemotePreferred.expires[key] = time.Now().Add(traeIDEPreferredTTL)
	// 偏好互斥：确认仅 Remote 可服务即清除反向偏好。
	traeIDEPreferred.mu.Lock()
	delete(traeIDEPreferred.expires, key)
	traeIDEPreferred.mu.Unlock()
}

func (s *OpenAIGatewayService) traePrefersRemote(accountID int64, model string) bool {
	key := fmt.Sprintf("%d|%s", accountID, model)
	traeRemotePreferred.mu.Lock()
	defer traeRemotePreferred.mu.Unlock()
	expiry, ok := traeRemotePreferred.expires[key]
	return ok && time.Now().Before(expiry)
}

// isTraeRemoteFallbackError 识别回合层返回的「IDE 不服务该模型」回落信号。
func isTraeRemoteFallbackError(err error) bool {
	ue := trae.AsUpstreamError(err)
	return ue != nil && ue.Code == trae.RemoteFallbackCode
}

// traeTurnTranslator 是回合管线消费的翻译器契约：Remote 快照翻译器与
// IDE（llm_utils_chat）增量翻译器共用同一套流式/缓冲收尾逻辑。
type traeTurnTranslator interface {
	Started() bool
	SawOutput() bool
	ProcessEvent(trae.Event) []trae.Delta
	Finalize(bool) (*trae.TurnResult, []trae.Delta)
	// FallbackError 判定本通道流中需要换通道重试的错误事件（未输出前）：
	// Remote 1005 → IDE；IDE 4001/4023（模型不可服务）→ Remote。
	FallbackError(trae.Event) *trae.UpstreamError
}

// traeEventSource 打开一个回合的事件流（Remote 两步会话第二步，或 IDE
// 单请求 SSE），供回合管线消费。
type traeEventSource func() (<-chan trae.Event, <-chan error)

// streamTraeTurn 流式回写：SSE 头 + 增量 chunk + 心跳注释 + [DONE]。
// SSE 头惰性写入：首个输出/心跳前发生可重试错误（限流换号、IDE 回落）
// 时不向客户端写任何字节，上层可以干净地重派。
func (s *OpenAIGatewayService) streamTraeTurn(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	eventsFn traeEventSource,
	translator traeTurnTranslator,
	chatReq apicompat.ChatCompletionsRequest,
	originalModel, billingModel, upstreamModel, effort, upstreamEndpoint string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	var (
		firstTokenMs       *int
		clientDisconnected bool
		doneSeen           bool
		toolCallBase       int
		created            = time.Now().Unix()
		chunkID            = fmt.Sprintf("chatcmpl-trae-%d", created)
		headWritten        bool
	)
	writeChunk := func(delta apicompat.ChatDelta, finish *string) {
		if clientDisconnected {
			return
		}
		chunk := apicompat.ChatCompletionsChunk{
			ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
			Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: delta, FinishReason: finish}},
		}
		sse, err := apicompat.ChatChunkToSSE(chunk)
		if err != nil {
			return
		}
		if _, werr := fmt.Fprint(c.Writer, sse); werr != nil {
			clientDisconnected = true
			return
		}
		c.Writer.Flush()
	}
	ensureHead := func() {
		if headWritten {
			return
		}
		// 回落续流：Remote 回合已写过头时（同请求内回落 IDE），不重复写
		// 头与 role 帧，客户端看到的是单一连续 SSE 流。
		if written, ok := c.Get(traeStreamHeadKey); ok {
			headWritten, _ = written.(bool)
			if headWritten {
				return
			}
		}
		headWritten = true
		c.Set(traeStreamHeadKey, true)
		c.Writer.Header().Set("Content-Type", "text/event-stream")
		c.Writer.Header().Set("Cache-Control", "no-cache")
		c.Writer.Header().Set("Connection", "keep-alive")
		c.Writer.Header().Set("X-Accel-Buffering", "no")
		c.Writer.WriteHeader(http.StatusOK)
		writeChunk(apicompat.ChatDelta{Role: "assistant"}, nil)
	}
	writeDelta := func(delta apicompat.ChatDelta, finish *string) {
		ensureHead()
		writeChunk(delta, finish)
	}

	events, errCh := eventsFn()
	streamFailed := false
loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				if errCh == nil {
					break loop
				}
				continue
			}
			if ev.Name == trae.EventDone {
				doneSeen = true
			}
			if fallback := translator.FallbackError(ev); fallback != nil {
				// Remote 对该模型固定 1005：未写头，干净交上层回落 IDE。
				return nil, fallback
			}
			if firstTokenMs == nil && !isTraeHeartbeatEvent(ev) {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			deltas := translator.ProcessEvent(ev)
			for _, delta := range deltas {
				if delta.Heartbeat {
					// 心跳以 SSE 注释透传：客户端忽略注释但不会误判回合结束。
					if !clientDisconnected {
						ensureHead()
						fmt.Fprint(c.Writer, ": keepalive\n\n") //nolint:errcheck
						c.Writer.Flush()
					}
					continue
				}
				chatDelta := apicompat.ChatDelta{}
				emitted := false
				if delta.Reasoning != "" {
					chatDelta.ReasoningContent = &delta.Reasoning
					emitted = true
				}
				if delta.Content != "" {
					chatDelta.Content = &delta.Content
					emitted = true
				}
				if len(delta.ToolCalls) > 0 {
					chatDelta.ToolCalls = toChatToolCalls(delta.ToolCalls, toolCallBase)
					toolCallBase += len(delta.ToolCalls)
					emitted = true
				}
				if emitted {
					writeDelta(chatDelta, nil)
				}
			}
		case streamErr, ok := <-errCh:
			if !ok {
				errCh = nil
				if events == nil {
					break loop
				}
				continue
			}
			if streamErr != nil {
				streamFailed = true
				s.markTraeAccountRateLimited(ctx, account, streamErr)
				if ue := trae.AsUpstreamError(streamErr); ue != nil && ue.Kind == trae.ErrorKindRateLimit && !translator.SawOutput() {
					// 首模型事件前的限流：不写终态响应，交给上层换号重试。
					return nil, fmt.Errorf("trae stream: %w", streamErr)
				}
				if translator.SawOutput() && !clientDisconnected {
					// 已有输出：不重放，按当前状态收尾（usage 已累计则照常计费）。
					break loop
				}
				if !clientDisconnected {
					status := http.StatusBadGateway
					if ue := trae.AsUpstreamError(streamErr); ue != nil {
						status = traeErrorHTTPStatus(ue)
					}
					writeChatCompletionsError(c, status, "api_error", "trae upstream stream error: "+streamErr.Error())
				}
				return nil, fmt.Errorf("trae stream: %w", streamErr)
			}
		case <-ctx.Done():
			clientDisconnected = true
			break loop
		}
	}
	_ = streamFailed
	return s.finishTraeStream(c, account, translator, clientDisconnected, doneSeen, toolCallBase, firstTokenMs, originalModel, billingModel, upstreamModel, upstreamEndpoint, effort, chunkID, created, startTime, writeDelta, ensureHead)
}

// isTraeIDEFallbackError 识别回合层返回的 1005 回落信号。
func isTraeIDEFallbackError(err error) bool {
	ue := trae.AsUpstreamError(err)
	return ue != nil && ue.Code == trae.IDEFallbackCode
}

func isTraeHeartbeatEvent(ev trae.Event) bool {
	return ev.Name == trae.EventHeartbeat || ev.Name == "keepalive" || ev.Name == "ping"
}

// finishTraeStream 收尾：汇总 usage/finish chunk + [DONE]，构建转发结果。
func (s *OpenAIGatewayService) finishTraeStream(
	c *gin.Context,
	account *Account,
	translator traeTurnTranslator,
	clientDisconnected, doneSeen bool,
	toolCallBase int,
	firstTokenMs *int,
	originalModel, billingModel, upstreamModel, upstreamEndpoint, effort, chunkID string,
	created int64,
	startTime time.Time,
	writeDelta func(apicompat.ChatDelta, *string),
	ensureHead func(),
) (*OpenAIForwardResult, error) {
	ensureHead()
	result, tail := translator.Finalize(!doneSeen)
	for _, delta := range tail {
		chatDelta := apicompat.ChatDelta{}
		if delta.Reasoning != "" {
			chatDelta.ReasoningContent = &delta.Reasoning
		}
		if delta.Content != "" {
			chatDelta.Content = &delta.Content
		}
		if len(delta.ToolCalls) > 0 {
			chatDelta.ToolCalls = toChatToolCalls(delta.ToolCalls, toolCallBase)
			toolCallBase += len(delta.ToolCalls)
		}
		if chatDelta.Reasoning != nil || chatDelta.Content != nil || len(chatDelta.ToolCalls) > 0 {
			writeDelta(chatDelta, nil)
		}
	}
	if result.Err != nil {
		return nil, s.traeTerminalError(c, account, result.Err)
	}
	if !clientDisconnected {
		finish := result.FinishReason
		if finish == "" {
			finish = "stop"
		}
		if len(result.ToolCalls) > 0 {
			finish = "tool_calls"
		}
		writeDelta(apicompat.ChatDelta{}, &finish)
		if result.Usage != nil {
			usage := traeUsageToChatUsage(result.Usage)
			chunk := apicompat.ChatCompletionsChunk{
				ID: chunkID, Object: "chat.completion.chunk", Created: created, Model: originalModel,
				Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: apicompat.ChatDelta{}, FinishReason: nil}},
				Usage:   &usage,
			}
			if sse, err := apicompat.ChatChunkToSSE(chunk); err == nil {
				fmt.Fprint(c.Writer, sse) //nolint:errcheck
			}
		}
		fmt.Fprint(c.Writer, "data: [DONE]\n\n") //nolint:errcheck
		c.Writer.Flush()
	}
	return &OpenAIForwardResult{
		Usage:            traeUsageToOpenAIUsage(result.Usage),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: upstreamEndpoint,
		Stream:           true,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnected,
	}, nil
}

// bufferedTraeTurn 非流式：收完整回合后组装单条 completion 响应。
func (s *OpenAIGatewayService) bufferedTraeTurn(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	eventsFn traeEventSource,
	translator traeTurnTranslator,
	chatReq apicompat.ChatCompletionsRequest,
	originalModel, billingModel, upstreamModel, effort, upstreamEndpoint string,
	startTime time.Time,
) (*OpenAIForwardResult, error) {
	var firstTokenMs *int
	events, errCh := eventsFn()
	for events != nil || errCh != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			if fallback := translator.FallbackError(ev); fallback != nil {
				return nil, fallback
			}
			if firstTokenMs == nil && !isTraeHeartbeatEvent(ev) {
				ms := int(time.Since(startTime).Milliseconds())
				firstTokenMs = &ms
			}
			translator.ProcessEvent(ev)
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			if err != nil {
				s.markTraeAccountRateLimited(ctx, account, err)
				return nil, fmt.Errorf("trae stream: %w", err)
			}
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	result, _ := translator.Finalize(false)
	if result.Err != nil {
		s.markTraeAccountRateLimited(ctx, account, result.Err)
		if result.Err.Kind == trae.ErrorKindRateLimit && !result.Err.ObservedModelOutput {
			return nil, fmt.Errorf("trae upstream: %w", result.Err)
		}
		writeChatCompletionsError(c, traeErrorHTTPStatus(result.Err), "api_error", result.Err.Error())
		return nil, result.Err
	}

	reasoning := result.Reasoning
	message := gin.H{"role": "assistant", "content": result.Content, "reasoning_content": reasoning}
	finishReason := result.FinishReason
	if len(result.ToolCalls) > 0 {
		message["tool_calls"] = toChatToolCalls(result.ToolCalls, 0)
		finishReason = "tool_calls"
	}
	payload := gin.H{
		"id":      fmt.Sprintf("chatcmpl-trae-%d", time.Now().Unix()),
		"object":  "chat.completion",
		"created": time.Now().Unix(),
		"model":   originalModel,
		"choices": []gin.H{{
			"index":         0,
			"message":       message,
			"finish_reason": finishReason,
		}},
	}
	if result.Usage != nil {
		payload["usage"] = gin.H{
			"prompt_tokens":     result.Usage.PromptTokens,
			"completion_tokens": result.Usage.CompletionTokens,
			"total_tokens":      result.Usage.TotalTokens,
		}
	}
	c.JSON(http.StatusOK, payload)
	return &OpenAIForwardResult{
		Usage:            traeUsageToOpenAIUsage(result.Usage),
		Model:            originalModel,
		BillingModel:     billingModel,
		UpstreamModel:    upstreamModel,
		UpstreamEndpoint: upstreamEndpoint,
		Stream:           false,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
	}, nil
}

// traeTerminalError 流中途的 error 事件：已有输出则记录并结束，否则写错误响应。
func (s *OpenAIGatewayService) traeTerminalError(c *gin.Context, account *Account, ue *trae.UpstreamError) error {
	if account != nil && ue.Kind == trae.ErrorKindRateLimit {
		ctx := context.Background()
		_ = s.accountRepo.SetRateLimited(ctx, account.ID, time.Now().Add(traeRateLimitCooldown))
	}
	message := ue.Error()
	if ue.Kind == trae.ErrorKindRateLimit && !ue.ObservedModelOutput {
		// 未产生输出的限流：上层可换号，不写终态响应。
		return errors.New(message)
	}
	writeChatCompletionsError(c, traeErrorHTTPStatus(ue), "api_error", message)
	return errors.New(message)
}

func (s *OpenAIGatewayService) markTraeAccountRateLimited(ctx context.Context, account *Account, err error) {
	if account == nil || s.accountRepo == nil {
		return
	}
	ue := trae.AsUpstreamError(err)
	if ue == nil || ue.Kind != trae.ErrorKindRateLimit {
		return
	}
	_ = s.accountRepo.SetRateLimited(ctx, account.ID, time.Now().Add(traeRateLimitCooldown))
}

func traeErrorHTTPStatus(ue *trae.UpstreamError) int {
	switch ue.Kind {
	case trae.ErrorKindRateLimit:
		return http.StatusTooManyRequests
	case trae.ErrorKindAuth:
		return http.StatusUnauthorized
	default:
		return http.StatusBadGateway
	}
}

// traeModelDescriptor 取模型 descriptor（含绑定校验：目录没有该模型时报错，
// 防止上游静默回落默认模型）。目录按账号缓存，miss 时拉取上游。
func (s *OpenAIGatewayService) traeModelDescriptor(
	ctx context.Context,
	client *trae.Client,
	account *Account,
	token, model string,
) (*trae.ModelDescriptor, error) {
	if s.traeCatalog == nil {
		s.traeCatalog = &traeRuntimeCatalog{}
	}
	if cached := s.traeCatalog.get(account.ID); cached != nil {
		if descriptor, ok := cached[model]; ok {
			return descriptor, nil
		}
		return nil, fmt.Errorf("trAE model %q is not available for this account (bound catalog lookup failed)", model)
	}
	descriptors, err := client.FetchModels(ctx, token)
	if err != nil {
		return nil, fmt.Errorf("trAE fetch model catalog: %w", err)
	}
	s.traeCatalog.put(account.ID, descriptors)
	for _, descriptor := range descriptors {
		if descriptor.ConfigName == model {
			return descriptor, nil
		}
	}
	return nil, fmt.Errorf("trAE model %q is not available for this account", model)
}

func (s *OpenAIGatewayService) traeUpstreamClient() *trae.Client {
	if s.traeClient != nil {
		return s.traeClient
	}
	return &trae.Client{}
}

// traeClientForAccount 返回带账号代理的上游客户端（无 oauth service 时退
// 回默认端点，测试路径走注入 client）。
func (s *OpenAIGatewayService) traeClientForAccount(ctx context.Context, account *Account) *trae.Client {
	if s.traeClient != nil {
		return s.traeClient
	}
	if s.traeOAuthService != nil {
		return s.traeOAuthService.UpstreamClient(ctx, account)
	}
	return &trae.Client{}
}

func toTraeMessages(messages []apicompat.ChatMessage) []trae.ChatMessage {
	out := make([]trae.ChatMessage, 0, len(messages))
	for _, m := range messages {
		wire := trae.ChatMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCallID: m.ToolCallID,
			Name:       m.Name,
		}
		// assistant 的历史 tool_calls 必须透传：丢了它，后续 tool 结果消息
		// 引用的 tool_call_id 在上游找不到对应调用 → 业务码 4027
		// "tool_call_id is not found"（CC 多轮工具会话实测）。
		if len(m.ToolCalls) > 0 {
			if encoded, err := json.Marshal(m.ToolCalls); err == nil {
				wire.ToolCalls = encoded
			}
		}
		out = append(out, wire)
	}
	return out
}

func traeUsageToOpenAIUsage(usage *trae.Usage) OpenAIUsage {
	if usage == nil {
		return OpenAIUsage{}
	}
	out := OpenAIUsage{
		InputTokens:  usage.PromptTokens,
		OutputTokens: usage.CompletionTokens,
	}
	if usage.PromptTokensDetails != nil {
		out.CacheReadInputTokens = usage.PromptTokensDetails.CachedTokens
	}
	return out
}

func traeUsageToChatUsage(usage *trae.Usage) apicompat.ChatUsage {
	if usage == nil {
		return apicompat.ChatUsage{}
	}
	out := apicompat.ChatUsage{
		PromptTokens:     usage.PromptTokens,
		CompletionTokens: usage.CompletionTokens,
		TotalTokens:      usage.TotalTokens,
	}
	if usage.PromptTokensDetails != nil {
		out.PromptTokensDetails = &apicompat.ChatTokenDetails{CachedTokens: usage.PromptTokensDetails.CachedTokens}
	}
	return out
}

// SetTraeDeps 注入 TRAE 平台转发依赖（wire 装配时调用一次）。client 为
// 测试注入的端点覆盖；生产路径经 traeOAuthService 按账号解析代理。
func (s *OpenAIGatewayService) SetTraeDeps(tokenProvider *TraeTokenProvider, oauthService *TraeOAuthService, client *trae.Client) {
	s.traeTokenProvider = tokenProvider
	s.traeOAuthService = oauthService
	if client != nil {
		s.traeClient = client
	}
}

// toTraeToolDefinitions 转换 apicompat 工具定义为桥接定义。
func toTraeToolDefinitions(tools []apicompat.ChatTool) []trae.ToolDefinition {
	out := make([]trae.ToolDefinition, 0, len(tools))
	for _, tool := range tools {
		definition := trae.ToolDefinition{Type: tool.Type}
		if tool.Function != nil {
			if payload, err := json.Marshal(tool.Function); err == nil {
				definition.Function = payload
			}
		}
		if definition.Type == "" {
			definition.Type = "function"
		}
		out = append(out, definition)
	}
	return out
}

// toolChoiceString 从 ToolChoice 原文提取 none/required/函数名（对象形态）。
func toolChoiceString(raw json.RawMessage) string {
	value := strings.TrimSpace(string(raw))
	if value == "" || value == "null" {
		return ""
	}
	if unquoted := strings.Trim(value, `"`); unquoted != value {
		return unquoted
	}
	var parsed struct {
		Type     string `json:"type"`
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ""
	}
	if parsed.Function.Name != "" {
		return parsed.Function.Name
	}
	return parsed.Type
}

// toChatToolCalls 把扫描出的调用映射为 OpenAI wire 形状（Index 基数可配，
// 流式与缓冲共用）。
func toChatToolCalls(calls []trae.ToolCall, baseIndex int) []apicompat.ChatToolCall {
	out := make([]apicompat.ChatToolCall, 0, len(calls))
	for i, call := range calls {
		index := baseIndex + i
		if call.Index != nil {
			// 增量片段自带稳定槽位：下游（OpenAI 流客户端 / Anthropic 状态
			// 机）按 index 拼接片段，不得重新编号。
			index = *call.Index
		}
		out = append(out, apicompat.ChatToolCall{
			Index: &index,
			ID:    call.ID,
			Type:  "function",
			Function: apicompat.ChatFunctionCall{
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	return out
}

func escapeJSONString(value string) string {
	payload, err := json.Marshal(value)
	if err != nil {
		return value
	}
	return string(payload[1 : len(payload)-1])
}

// isTraeCredentialTrue 判定 credentials 布尔标记（容忍 "true"/true 两种存法）。
func isTraeCredentialTrue(account *Account, key string) bool {
	switch v := account.Credentials[key].(type) {
	case bool:
		return v
	case string:
		return v == "true" || v == "1" || v == "yes" || v == "on"
	default:
		return false
	}
}
