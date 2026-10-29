package trae

// IDE 适配器：TraeWork 兼容会话 POST /api/agent/v3/llm_utils_chat
// （function=solo_work_lite）。企业版（ToB 租户）账号的实际执行链——
// Remote chat_sessions 对 kimi-k3.1-pro 等模型返回业务码 1005（2026-10-29
// 实测，Agent/Work 两档均拒），而该端点正常。协议形状来自 Trae2api-cn
// send_chat_request / build_llm_chat_body 与线上抓包：
//
//   请求 POST {cloud-ide host}/api/agent/v3/llm_utils_chat
//   头   SOLO 客户端族（x-ide-version 0.1.52 / icube-ai / 83DG…）
//   体   {messages: 结构化 blocks + 原生 tool_calls, config_name, model,
//        function: solo_work_lite, stream: true, request_id, session_id,
//        max_tokens?, tools?, tool_choice?, parallel_tool_calls?}
//   响应 SSE：output{response,reasoning_content,tool_calls}（增量 delta）、
//        token_usage（扁平 OpenAI 形）、done{finish_reason}、error、
//        metadata/extra_info(provider 真名)/progress_notice/timing_cost。

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
)

const (
	ideChatPath = "/api/agent/v3/llm_utils_chat"
	ideFunction = "solo_work_lite"

	ideAppID          = "6eefa01c-1036-4c7e-9ca5-d891f63bfcd8"
	ideSoloVersion    = "0.1.52"
	ideSoloVersionCod = "20260811"
	ideDeviceBrand    = "83DG"
	ideOSVersion      = "Windows 11 Pro"
	ideLSCBDAID       = "787976"

	// IDEFallbackCode 是 Remote 会话需要回落本端点的上游业务码：错误事件
	// {"code":1005,"message":""}，实测 kimi-k3.1-pro 在 Agent/Work 两档均
	// 固定返回。上游不解释，按「该模型在此账号的 Remote 通道不可用」处理。
	IDEFallbackCode = "1005"

	// EventOutput 是 llm_utils_chat 的增量输出帧。
	EventOutput = "output"
)

// IDEChatRequest 是一次 llm_utils_chat 回合的输入。Messages 沿用网关侧
// OpenAI 形消息（ChatMessage）；工具走原生字段而非文本桥。
type IDEChatRequest struct {
	Messages          []ChatMessage
	ConfigName        string
	UserID            string
	MaxTokens         int
	Tools             []ToolDefinition
	ToolChoice        any
	ParallelToolCalls *bool
}

type ideWireMessage struct {
	Role       string          `json:"role"`
	Content    []ideTextBlock  `json:"content,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

type ideTextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// IDEChat 发起一次 llm_utils_chat 回合并返回归一化事件流。生命周期与
// StreamEvents 相同：事件 channel 消费完毕即回合结束；errC 携带终态错误。
// 回合是单请求 SSE（无两步会话、无 stop 收尾）。
func (c *Client) IDEChat(ctx context.Context, token string, req IDEChatRequest) (<-chan Event, <-chan error) {
	events := make(chan Event, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		if err := ctx.Err(); err != nil {
			errs <- err
			return
		}
		body, err := buildIDEChatBody(req)
		if err != nil {
			errs <- err
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cloudIDEHost()+ideChatPath, bytes.NewReader(body))
		if err != nil {
			errs <- err
			return
		}
		c.setIDEHeaders(httpReq, token, req.UserID)
		// 与 StreamEvents 同理：事件流不吃公共总超时，生命周期交给 ctx。
		streamClient := &http.Client{Transport: c.httpClient().Transport, Timeout: 0}
		resp, err := streamClient.Do(httpReq)
		if err != nil {
			errs <- fmt.Errorf("trae: ide chat: %w", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			raw, _ := readLimited(resp.Body, 1<<20)
			errs <- ClassifyUpstreamError(
				strconv.Itoa(resp.StatusCode),
				fmt.Sprintf("llm_utils_chat [%d]: %s", resp.StatusCode, truncate(raw, 300)),
				false,
			)
			return
		}
		readSSEEvents(ctx, resp.Body, events)
	}()
	return events, errs
}

func buildIDEChatBody(req IDEChatRequest) ([]byte, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("trae: ide chat messages cannot be empty")
	}
	config := strings.TrimSpace(req.ConfigName)
	if config == "" {
		return nil, fmt.Errorf("trae: ide chat config name is empty")
	}
	session := randomID()
	body := map[string]any{
		"messages":    toIDEWireMessages(req.Messages),
		"config_name": config,
		"model":       config,
		"function":    ideFunction,
		"stream":      true,
		"request_id":  session,
		"session_id":  session,
	}
	if req.MaxTokens > 0 {
		body["max_tokens"] = req.MaxTokens
	}
	if len(req.Tools) > 0 {
		body["tools"] = req.Tools
		if req.ToolChoice != nil {
			body["tool_choice"] = req.ToolChoice
		}
		if req.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	return json.Marshal(body)
}

// toIDEWireMessages 把 OpenAI 形消息转换为 llm_utils_chat 的结构化消息：
// 文本进 content blocks；assistant.tool_calls 原样透传（上游接受 OpenAI
// 形状）；tool 结果保留 tool_call_id/name 关联。
func toIDEWireMessages(messages []ChatMessage) []ideWireMessage {
	out := make([]ideWireMessage, 0, len(messages))
	for _, message := range messages {
		role := strings.ToLower(strings.TrimSpace(message.Role))
		switch role {
		case "developer":
			role = "system"
		case "function":
			role = "tool"
		case "system", "user", "assistant", "tool":
		default:
			role = "user"
		}
		item := ideWireMessage{
			Role:       role,
			ToolCalls:  message.ToolCalls,
			ToolCallID: message.ToolCallID,
			Name:       message.Name,
		}
		if text := contentText(message.Content); text != "" || (item.ToolCalls == nil && item.ToolCallID == "") {
			item.Content = []ideTextBlock{{Type: "text", Text: text}}
		}
		out = append(out, item)
	}
	return out
}

// contentText 从 OpenAI 形 content（字符串或 blocks 数组）提取文本。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var asString string
	if err := json.Unmarshal(raw, &asString); err == nil {
		return asString
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err == nil {
		var parts []string
		for _, block := range blocks {
			if text, ok := block["text"].(string); ok && text != "" {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

func (c *Client) setIDEHeaders(req *http.Request, token, userID string) {
	deviceID := DeviceIDFor(userID)
	h := req.Header
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("Connection", "keep-alive")
	h.Set("Authorization", "Cloud-IDE-JWT "+token)
	h.Set("X-Cloudide-Token", token)
	if userID != "" {
		h.Set("x-uid", userID)
	}
	h.Set("x-app-id", ideAppID)
	h.Set("x-device-id", deviceID)
	h.Set("x-machine-id", hex.EncodeToString(sha256Sum("traework-linux-probe\x1f"+deviceID)))
	h.Set("x-request-id", randomID())
	h.Set("x-ide-version", ideSoloVersion)
	h.Set("x-ide-version-code", ideSoloVersionCod)
	h.Set("x-ide-version-type", "stable")
	h.Set("x-app-version", "default")
	h.Set("x-app-version-code", ideSoloVersionCod)
	h.Set("x-version-code", ideSoloVersionCod)
	h.Set("x-device-cpu", "AMD")
	h.Set("x-device-brand", ideDeviceBrand)
	h.Set("x-device-type", "windows")
	h.Set("x-device-platform", "windows")
	h.Set("x-os-version", ideOSVersion)
	h.Set("x-system-type", "Windows")
	h.Set("package-type", "stable_cn")
	h.Set("x-lscbd-aid", ideLSCBDAID)
	h.Set("x-lscbd-platform", "windows")
	h.Set("x-ss-dp", ideLSCBDAID)
	h.Set("x-plugin-channel", "icube-ai")
	h.Set("app-version", ideSoloVersion)
	h.Set("x-bridge-transport", "aha")
	h.Set("x-ahanet-timeout", "86400")
	h.Set("x-lgw-req-sdk-type", "3")
	h.Set("x-net-sdk-domain-dispatch", "1")
	h.Set("x-ttnet-bypass-decompression", "1")
	h.Set("x-ttnet-bypass-cookie", "0")
	h.Set("request-traffic-type", "prod")
	h.Set("User-Agent", "Trae/"+ideSoloVersion)
}

// DeviceIDFor 按账号稳定派生 16 位数字设备标识（与 service.TraeDeviceID
// 同源派生：sha256("trae"+userID) 全量 hex 转 big.Int 对 10^16 取模），
// 保持 Remote / IDE 两通道设备身份一致。
func DeviceIDFor(userID string) string {
	digest := hex.EncodeToString(sha256Sum("trae" + userID))
	value, ok := new(big.Int).SetString(digest, 16)
	if !ok {
		return "0000000000000000"
	}
	mod := new(big.Int).Mod(value, big.NewInt(1e16))
	return fmt.Sprintf("%016s", mod.String())
}

func sha256Sum(value string) []byte {
	sum := sha256.Sum256([]byte(value))
	return sum[:]
}

func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "00000000-0000-4000-8000-000000000000"
	}
	buf[6] = (buf[6] & 0x0f) | 0x40
	buf[8] = (buf[8] & 0x3f) | 0x80
	hexed := hex.EncodeToString(buf)
	return hexed[0:8] + "-" + hexed[8:12] + "-" + hexed[12:16] + "-" + hexed[16:20] + "-" + hexed[20:32]
}

// readSSEEvents 解析 SSE 字节流为归一化事件（StreamEvents 与 IDEChat 共用）：
// event:/data: 行、data: [DONE] 即 done、注释行忽略、EOF 时遗留 buffer 的
// 尾帧不丢弃。
func readSSEEvents(ctx context.Context, body io.Reader, events chan<- Event) {
	reader := bufio.NewReader(body)
	var (
		eventName string
		dataLines []string
	)
	flush := func(final bool) {
		defer func() {
			eventName = ""
			dataLines = nil
		}()
		payload := strings.TrimSpace(strings.Join(dataLines, "\n"))
		if payload == "[DONE]" {
			select {
			case events <- Event{Name: EventDone, Data: map[string]any{}}:
			case <-ctx.Done():
			}
			return
		}
		if payload == "" && eventName == "" {
			return
		}
		data := map[string]any{}
		if payload != "" {
			if err := json.Unmarshal([]byte(payload), &data); err != nil {
				data = map[string]any{"_raw": payload}
			}
		}
		name := eventName
		if name == "" {
			name = firstStringValue([]map[string]any{data}, "event", "event_type", "eventType", "type")
		}
		if name == "" {
			if !final {
				return
			}
			name = EventMessage
		}
		select {
		case events <- Event{Name: NormalizeEventName(name), Data: data}:
		case <-ctx.Done():
		}
	}
	for {
		line, err := reader.ReadString('\n')
		if len(line) > 0 || err == nil {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case strings.HasPrefix(trimmed, ":"):
				// SSE 注释（心跳）忽略。
			case strings.HasPrefix(trimmed, "event:"):
				eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
			case trimmed == "":
				flush(false)
			}
		}
		if err != nil {
			if len(dataLines) > 0 || eventName != "" {
				flush(true)
			}
			return
		}
	}
}

// IDETranslator 把 llm_utils_chat 事件流翻译为 OpenAI 增量。与
// StreamTranslator 同一方法契约（runtime 的回合管线按接口消费两者）；
// 区别在 output 帧是增量 delta，无需快照差分。
type IDETranslator struct {
	model       string
	started     bool
	sawOutput   bool
	content     strings.Builder
	reasoning   strings.Builder
	toolCalls   []ToolCall
	callSeq     int
	usage       *Usage
	finish      string
	provider    string
	upstreamErr *UpstreamError
	result      TurnResult
	finalized   bool
}

// NewIDETranslator 创建翻译器；model 是对外回显的模型名。
func NewIDETranslator(model string) *IDETranslator {
	return &IDETranslator{model: model, finish: "stop"}
}

func (t *IDETranslator) Started() bool         { return t.started }
func (t *IDETranslator) SawOutput() bool       { return t.sawOutput }
func (t *IDETranslator) Model() string         { return t.model }
func (t *IDETranslator) ProviderModel() string { return t.provider }

// ProcessEvent 消费一帧事件，返回应下发的增量。
func (t *IDETranslator) ProcessEvent(ev Event) []Delta {
	switch ev.Name {
	case EventOutput:
		return t.processOutput(ev.Data)
	case EventTokenUsage:
		t.usage = MapIDEUsage(ev.Data)
		t.sawOutput = true
	case EventDone:
		if reason := firstStringValue([]map[string]any{ev.Data}, "finish_reason", "finishReason"); reason != "" {
			t.finish = reason
		}
		t.started = true
	case EventError:
		code, message := ExtractErrorDetails(ev.Data)
		t.upstreamErr = ClassifyUpstreamError(code, message, t.sawOutput)
	case "extra_info":
		if provider := firstStringValue([]map[string]any{ev.Data}, "model"); provider != "" {
			t.provider = provider
		}
	case "metadata", "timing_cost", "progress_notice", EventRequestWaitInQ, EventHeartbeat:
		// 排队/心跳/遥测不改变回合内容。
	}
	return nil
}

func (t *IDETranslator) processOutput(data map[string]any) []Delta {
	t.started = true
	var deltas []Delta
	if reasoning, ok := data["reasoning_content"].(string); ok && reasoning != "" {
		t.reasoning.WriteString(reasoning)
		deltas = append(deltas, Delta{Reasoning: reasoning})
		t.sawOutput = true
	}
	if content, ok := data["response"].(string); ok && content != "" {
		t.content.WriteString(content)
		deltas = append(deltas, Delta{Content: content})
		t.sawOutput = true
	}
	if calls := ParseIDEToolCalls(data["tool_calls"]); len(calls) > 0 {
		t.toolCalls = append(t.toolCalls, calls...)
		deltas = append(deltas, Delta{ToolCalls: calls})
		t.sawOutput = true
	}
	return deltas
}

// Finalize 收尾：无 done 帧的结束按截断处理。
func (t *IDETranslator) Finalize(streamEndedWithoutDone bool) (*TurnResult, []Delta) {
	if t.finalized {
		return &t.result, nil
	}
	t.finalized = true
	if t.sawOutput || t.usage != nil {
		t.finish = "stop"
		if len(t.toolCalls) > 0 {
			t.finish = "tool_calls"
		}
	}
	t.result = TurnResult{
		Content:       t.content.String(),
		Reasoning:     t.reasoning.String(),
		Usage:         t.usage,
		FinishReason:  t.finish,
		ProviderModel: t.provider,
		Truncated:     streamEndedWithoutDone,
		Err:           t.upstreamErr,
		ToolCalls:     t.toolCalls,
	}
	return &t.result, nil
}

// MapIDEUsage 解析 llm_utils_chat 的扁平 token_usage（键名兼容
// prompt_tokens/input_token 两种族系；抓包实测为前者）。
func MapIDEUsage(raw map[string]any) *Usage {
	usage := raw
	if nested, ok := raw["usage"].(map[string]any); ok {
		usage = nested
	}
	prompt := intValueOf(usage, "prompt_tokens", "input_token", "inputToken")
	completion := intValueOf(usage, "completion_tokens", "output_token", "outputToken")
	total := intValueOf(usage, "total_tokens", "totalToken")
	if total == 0 {
		total = prompt + completion
	}
	out := &Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
	if cached := intValueOf(usage, "cache_read_input_tokens", "cacheReadInputTokens"); cached > 0 {
		out.PromptTokensDetails = &UsageTokensDetails{CachedTokens: cached}
	}
	return out
}

func intValueOf(source map[string]any, keys ...string) int {
	for _, key := range keys {
		switch v := source[key].(type) {
		case float64:
			return int(v)
		case int:
			return v
		case json.Number:
			if n, err := v.Int64(); err == nil {
				return int(n)
			}
		case string:
			if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
				return n
			}
		}
	}
	return 0
}

// ParseIDEToolCalls 解析 output 帧的原生 tool_calls 数组。形状为 OpenAI
// delta 族：{index?, id?, type, function|function_call: {name, arguments}}，
// arguments 兼容字符串与对象（对象序列化为 JSON 字符串）。
func ParseIDEToolCalls(raw any) []ToolCall {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	var out []ToolCall
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		fn, _ := entry["function"].(map[string]any)
		if fn == nil {
			fn, _ = entry["function_call"].(map[string]any)
		}
		if fn == nil {
			continue
		}
		name, _ := fn["name"].(string)
		if name == "" {
			continue
		}
		arguments := normalizeToolArguments(fn["arguments"])
		id, _ := entry["id"].(string)
		if id == "" {
			id = fmt.Sprintf("idecall_%d", len(out))
		}
		out = append(out, ToolCall{ID: id, Name: name, Arguments: arguments})
	}
	return out
}

func normalizeToolArguments(raw any) string {
	switch v := raw.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		if encoded, err := json.Marshal(v); err == nil {
			return string(encoded)
		}
		return ""
	}
}
