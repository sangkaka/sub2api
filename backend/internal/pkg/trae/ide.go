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
	"log/slog"
	"math/big"
	"net/http"
	"os"
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

	// RemoteFallbackCode 是 IDE 端点需要回落 Remote 会话的哨兵码（本端
	// 派生，非上游业务码）：IDE 首事件即拒 4001（参数无效=模型名不识别）
	// / 4023（服务异常=模型不在 solo_work_lite 目录，DeepSeek 系实测），
	// 说明该模型只能在 Remote 通道服务。
	RemoteFallbackCode = "ide-model-unavailable"

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
	// ReasoningEffort 非空时下发 reasoning_effort（如 glm 无思考回合
	// 工具调用退化为 arguments:"{}" 时按模型强制 high）。
	ReasoningEffort string
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
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if len(req.Tools) > 0 {
		// 上游 FunctionDefinition 要求 parameters 为 JSON 字符串（SOLO 编码，
		// 实测对象形报 4001 "cannot unmarshal object ... of type string"）。
		body["tools"] = ideToolDefinitions(req.Tools)
		if req.ToolChoice != nil {
			body["tool_choice"] = req.ToolChoice
		}
		if req.ParallelToolCalls != nil {
			body["parallel_tool_calls"] = *req.ParallelToolCalls
		}
	}
	return json.Marshal(body)
}

// ideToolDefinitions 把 OpenAI 工具定义转换为 llm_utils_chat 的原生形状：
// parameters 序列化为字符串，其余字段原样。
func ideToolDefinitions(tools []ToolDefinition) []map[string]any {
	out := make([]map[string]any, 0, len(tools))
	for _, tool := range tools {
		var fn struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(tool.Function, &fn); err != nil || fn.Name == "" {
			continue
		}
		definition := map[string]any{"name": fn.Name}
		if fn.Description != "" {
			definition["description"] = fn.Description
		}
		if len(fn.Parameters) > 0 {
			definition["parameters"] = string(fn.Parameters)
		}
		kind := strings.TrimSpace(tool.Type)
		if kind == "" {
			kind = "function"
		}
		out = append(out, map[string]any{"type": kind, "function": definition})
	}
	return out
}

// toIDEWireMessages 把 OpenAI 形消息转换为 llm_utils_chat 的结构化消息：
// 文本进 content blocks；assistant.tool_calls 转为上游 FunctionCall 形状
// （键名 function_call 而非 function，OpenAI 键会让上游 RPC 报 2001
// "required field Name is not set"）；tool 结果保留 tool_call_id/name 关联。
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
			ToolCalls:  ideWireToolCalls(message.ToolCalls),
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

// ideWireToolCalls 把 OpenAI 形 tool_calls（function 键）转换为上游
// LLMRawMessage.ToolCalls 形状：function_call 键 + arguments 字符串化。
func ideWireToolCalls(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}
	var calls []struct {
		Index    *int   `json:"index"`
		ID       string `json:"id"`
		Type     string `json:"type"`
		Function struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil || len(calls) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		if call.Function.Name == "" {
			continue
		}
		entry := map[string]any{
			"function_call": map[string]any{
				"name":      call.Function.Name,
				"arguments": normalizeToolArguments(call.Function.Arguments),
			},
		}
		if call.ID != "" {
			entry["id"] = call.ID
		}
		if call.Index != nil {
			entry["index"] = *call.Index
		}
		if call.Type != "" {
			entry["type"] = call.Type
		}
		out = append(out, entry)
	}
	if len(out) == 0 {
		return nil
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return encoded
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
	toolAcc     map[int]*ideToolAccumulator
	toolAccByID map[string]*ideToolAccumulator
	toolAccSeq  []*ideToolAccumulator
	callSeq     int
	// 形状指纹与丢弃样本的观测上限：分片形状随上游漂移时日志可直接
	// 定位，而不是只剩客户端症状。
	shapeLogged int
	dropLogged  int
	// ToolsRequiringArgs：有必填参数的工具名集合（{"{}"} 完整参数视为
	// 退化）。nil 表示不过滤（行为与历史一致）。
	ToolsRequiringArgs map[string]bool
	// streamedOutput：是否已向客户端下发过 thinking/文本（作废重试的前
	// 提是客户端未收到任何本回合内容）。
	streamedOutput bool
	// forwardedTool：是否透传过任何工具片段（含空参首帧——它会宣告
	// tool_use 块）。作废重试要求一个都没发过。
	forwardedTool bool
	usage         *Usage
	finish        string
	provider      string
	upstreamErr   *UpstreamError
	result        TurnResult
	finalized     bool
}

// ideToolAccumulator 聚合上游分片下发的单个工具调用：首帧带 id/name
// （arguments 常为空串），后续帧仅 index+arguments 片段。slot 是该调用
// 在 toolAccSeq 中的稳定槽位，透传片段以它为下游拼接键。
type ideToolAccumulator struct {
	id   string
	name string
	slot int
	args strings.Builder
	// wrapHoldState 控制 arguments 包装形状（模型模仿 function_call 信封
	// 产生 {"arguments":"<JSON 字符串>"}，可叠两层字符串）的处理：
	// undecided 在 wrapperKeyPrefix 长度的探测窗口内攒前缀；命中转 held
	// （片段只入账不下发，Finalize 拆包后一次性补发），不命中转 streaming
	// （探测期攒下的字节原样下发，行为与直通一致）。
	wrapHoldState int
	probe         []byte
	owesBytes     bool
	// degenHold：该调用的累计参数恰为 "{}" 且目标工具有必填参数（由
	// ToolsRequiringArgs 判定）——参数已完整、形状退化，扣住不下发，
	// 供回合级作废重试。
	degenHold bool
}

const (
	ideWrapUndecided = iota
	ideWrapHeld
	ideWrapStreaming
)

// wrapperKeyPrefix 是包装形状的起始指纹（模型输出为紧凑 JSON）。
var wrapperKeyPrefix = []byte(`{"arguments"`)

// NewIDETranslator 创建翻译器；model 是对外回显的模型名。
func NewIDETranslator(model string) *IDETranslator {
	return &IDETranslator{model: model, finish: "stop"}
}

// NewDegenerateGuardIDETranslator 创建带空参退化守卫的翻译器：
// toolsRequiringArgs 为有必填参数的工具名集合（nil 关闭守卫）。
func NewDegenerateGuardIDETranslator(model string, toolsRequiringArgs map[string]bool) *IDETranslator {
	tr := NewIDETranslator(model)
	tr.ToolsRequiringArgs = toolsRequiringArgs
	return tr
}

func (t *IDETranslator) Started() bool         { return t.started }
func (t *IDETranslator) SawOutput() bool       { return t.sawOutput }
func (t *IDETranslator) Model() string         { return t.model }
func (t *IDETranslator) ProviderModel() string { return t.provider }

// ShouldAbortTurn 判定空参退化回合可否整体作废：守卫开启、客户端未收到
// 任何本回合内容（thinking/文本/工具片段都未下发），且所有命名调用全部
// 被扣为 "{}" 退化。满足时上层可丢弃本回合带矫正提示重打。
func (t *IDETranslator) ShouldAbortTurn() bool {
	if t.ToolsRequiringArgs == nil || t.streamedOutput || t.forwardedTool {
		return false
	}
	any := false
	for _, acc := range t.toolAccSeq {
		if acc.name == "" {
			continue
		}
		if !acc.degenHold {
			return false
		}
		any = true
	}
	return any
}

// FallbackError 判定 IDE 流中需要换入口重试的错误事件：模型在该入口
// 不可服务（4001 参数无效 / 4023 服务异常 / 1005 不在目录——企业域名
// 入口对 DeepSeek 系 4023、模型网关入口对 kimi 系 1005，双主机实测）
// 且尚未观察到输出。此类错误实测为首事件即拒（~0.4s），未向客户端写
// 字节，上层可干净地换主机或回落 Remote。
func (t *IDETranslator) FallbackError(ev Event) *UpstreamError {
	if ev.Name != EventError || t.sawOutput {
		return nil
	}
	code, message := ExtractErrorDetails(ev.Data)
	if code != "4001" && code != "4023" && code != IDEFallbackCode {
		return nil
	}
	return &UpstreamError{
		Kind:    ErrorKindUpstream,
		Code:    RemoteFallbackCode,
		Message: "trae ide rejected this model (code " + code + ": " + message + "); retry via remote session",
	}
}

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
		t.streamedOutput = true
	}
	if content, ok := data["response"].(string); ok && content != "" {
		t.content.WriteString(content)
		deltas = append(deltas, Delta{Content: content})
		t.sawOutput = true
		t.streamedOutput = true
	}
	if items, ok := data["tool_calls"].([]any); ok && len(items) > 0 {
		// 工具调用按 OpenAI 惯例分片增量下发：首帧带 name/id、后续帧只带
		// index+arguments 片段。片段边收边以稳定槽位透传（客户端实时看到
		// tool_use 增量，长回合不会静默到收尾）；完整调用在 Finalize 组装
		// 进 TurnResult（非流式路径与 finish_reason 用）。
		if toolShapeDebug && t.shapeLogged < 3 {
			t.shapeLogged++
			raw, _ := json.Marshal(items)
			slog.Warn("trae ide tool_calls shape sample", "model", t.model, "raw", truncate(string(raw), 400))
		}
		if fragments := t.accumulateToolCalls(items); len(fragments) > 0 {
			deltas = append(deltas, Delta{ToolCalls: fragments})
			t.sawOutput = true
			t.forwardedTool = true
		}
	}
	return deltas
}

// toolShapeDebug 开启后每回合采样前几帧 tool_calls 原文（抓上游形状漂移用）。
var toolShapeDebug = os.Getenv("TRAE_TOOL_SHAPE_DEBUG") == "1"

// logDroppedToolEntry 记录被丢弃的工具分片原文（每回合限次，防刷屏）。
func (t *IDETranslator) logDroppedToolEntry(entry map[string]any, reason string) {
	if t.dropLogged >= 8 {
		return
	}
	t.dropLogged++
	raw, _ := json.Marshal(entry)
	slog.Warn("trae ide tool fragment dropped", "model", t.model, "reason", reason, "entry", truncate(string(raw), 300))
}

func (t *IDETranslator) accumulateToolCalls(items []any) []ToolCall {
	var fragments []ToolCall
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
			t.logDroppedToolEntry(entry, "no function/function_call key")
			continue
		}
		id, _ := entry["id"].(string)
		name, _ := fn["name"].(string)
		frag := normalizeToolArguments(fn["arguments"])
		if id == "" && name == "" && frag == "" {
			continue
		}
		acc := t.resolveToolAccumulator(entry, id, name)
		if acc == nil {
			t.logDroppedToolEntry(entry, "unattributable: no index/id/name")
			continue
		}
		// 并入「最近调用」兜底只接受新值起点片段；孤立的 } ] 空串闭合
		// 帧是上游并发流遗留，丢弃（对齐 trae2api 的 junk-drop）。
		if id == "" && name == "" && frag != "" && !startsNewJSONValue(frag) && entry["index"] == nil {
			t.logDroppedToolEntry(entry, "orphan closing fragment")
			continue
		}
		if id != "" && acc.id == "" {
			acc.id = id
		}
		if name != "" {
			acc.name = name
		}
		if frag == "" && acc.wrapHoldState != ideWrapHeld {
			// 首帧常带空 arguments：照常透传（空串会在下游被跳过），
			// 不推进探测窗口。
			slot := acc.slot
			fragments = append(fragments, ToolCall{ID: id, Name: name, Arguments: "", Index: &slot})
			continue
		}
		// 上游并发工具流会把多个调用的参数串在同一个 index 下连续发：
		// 当前槽位参数已是完整 JSON 而新片段以 { / [ 开头时，是新调用
		// 复用了 index，拆新槽位并继承工具名（对齐 trae2api 4f9451e 的
		// 并发流防御，避免 {"a":1}{"b":2} 拼接污染）。
		if frag != "" && acc.args.Len() > 0 && startsNewJSONValue(frag) && json.Valid([]byte(acc.args.String())) {
			split := &ideToolAccumulator{name: acc.name}
			t.registerToolAcc(split)
			acc = split
		}
		acc.args.WriteString(frag)
		if t.ToolsRequiringArgs != nil && acc.wrapHoldState != ideWrapStreaming && acc.args.String() == "{}" && t.ToolsRequiringArgs[acc.name] {
			// 完整首帧参数恰为 "{}" 且目标工具有必填参数：退化形状，
			// 扣住不下发（重试轮 ToolsRequiringArgs 为 nil 不会进这里）。
			acc.degenHold = true
			continue
		}
		if acc.degenHold {
			// 后续又来了片段（正常 {} 调用不会）：放弃扣留按原样直通。
			acc.degenHold = false
		}
		if acc.wrapHoldState == ideWrapUndecided {
			acc.probe = append(acc.probe, frag...)
			effective := bytes.TrimLeft(acc.probe, " \t\r\n")
			if len(effective) >= len(wrapperKeyPrefix) {
				if bytes.HasPrefix(effective, wrapperKeyPrefix) {
					acc.wrapHoldState = ideWrapHeld
					acc.owesBytes = true
				} else {
					acc.wrapHoldState = ideWrapStreaming
					slot := acc.slot
					// 首个片段必须带 id/name：单帧完整调用（glm 常见形状）
					// 只有这一次下发机会，缺名下游会以空名宣布 tool_use。
					fragments = append(fragments, ToolCall{ID: acc.id, Name: acc.name, Arguments: string(effective), Index: &slot})
				}
			} else if !bytes.HasPrefix(wrapperKeyPrefix, effective) {
				acc.wrapHoldState = ideWrapStreaming
				slot := acc.slot
				fragments = append(fragments, ToolCall{ID: acc.id, Name: acc.name, Arguments: string(effective), Index: &slot})
			}
			// effective 仍是 {"arguments" 的真前缀：继续攒，暂不下发。
			continue
		}
		if acc.wrapHoldState == ideWrapHeld {
			acc.owesBytes = true
			continue
		}
		slot := acc.slot
		fragments = append(fragments, ToolCall{
			ID: id, Name: name, Arguments: frag, Index: &slot,
		})
	}
	return fragments
}

// resolveToolAccumulator 归属分片：index 优先（OpenAI 惯例），无 index 按
// id 归位；两者皆无且带 name 视为新的单帧完整调用，无法归属则丢弃。
func (t *IDETranslator) resolveToolAccumulator(entry map[string]any, id, name string) *ideToolAccumulator {
	switch v := entry["index"].(type) {
	case float64:
		return t.toolAccSlot(int(v))
	case int:
		return t.toolAccSlot(v)
	}
	if id != "" {
		if acc, ok := t.toolAccByID[id]; ok {
			return acc
		}
		acc := &ideToolAccumulator{id: id}
		if t.toolAccByID == nil {
			t.toolAccByID = make(map[string]*ideToolAccumulator)
		}
		t.registerToolAcc(acc)
		t.toolAccByID[id] = acc
		return acc
	}
	if name != "" {
		acc := &ideToolAccumulator{name: name}
		t.registerToolAcc(acc)
		return acc
	}
	// 无 index 无 id 无 name 的片段：并入最近一个已命名调用（上游并发
	// 流的有序续片）。调用方负责丢弃不以 { / [ 开头的孤立闭合帧。
	for i := len(t.toolAccSeq) - 1; i >= 0; i-- {
		if t.toolAccSeq[i].name != "" {
			return t.toolAccSeq[i]
		}
	}
	return nil
}

// startsNewJSONValue 判断片段是否以新的 JSON 对象/数组开头。
func startsNewJSONValue(frag string) bool {
	for _, r := range frag {
		switch r {
		case ' ', '\t', '\r', '\n':
			continue
		case '{', '[':
			return true
		default:
			return false
		}
	}
	return false
}

func (t *IDETranslator) toolAccSlot(index int) *ideToolAccumulator {
	if acc, ok := t.toolAcc[index]; ok {
		return acc
	}
	acc := &ideToolAccumulator{}
	if t.toolAcc == nil {
		t.toolAcc = make(map[int]*ideToolAccumulator)
	}
	t.registerToolAcc(acc)
	t.toolAcc[index] = acc
	return acc
}

func (t *IDETranslator) registerToolAcc(acc *ideToolAccumulator) {
	acc.slot = len(t.toolAccSeq)
	t.toolAccSeq = append(t.toolAccSeq, acc)
}

func (t *IDETranslator) assembleToolCalls() []ToolCall {
	if len(t.toolAccSeq) == 0 {
		return nil
	}
	out := make([]ToolCall, 0, len(t.toolAccSeq))
outer:
	for _, acc := range t.toolAccSeq {
		if acc.name == "" {
			continue
		}
		if acc.degenHold && acc.args.String() == "{}" {
			// 混合回合中的 {} 头帧不进最终结果（真调用已拆到新槽位）。
			for _, other := range t.toolAccSeq {
				if other.name != "" && !other.degenHold {
					continue outer
				}
			}
		}
		if repaired := repairWrappedToolArguments(acc.args.String()); repaired != acc.args.String() {
			acc.args.Reset()
			acc.args.WriteString(repaired)
		}
		id := acc.id
		if id == "" {
			id = fmt.Sprintf("idecall_%d", t.callSeq)
			t.callSeq++
		}
		arguments := acc.args.String()
		if strings.TrimSpace(arguments) == "" {
			arguments = "{}"
		}
		out = append(out, ToolCall{ID: id, Name: acc.name, Arguments: arguments})
	}
	return out
}

// Finalize 收尾：无 done 帧的结束按截断处理。
func (t *IDETranslator) Finalize(streamEndedWithoutDone bool) (*TurnResult, []Delta) {
	if t.finalized {
		return &t.result, nil
	}
	t.finalized = true
	t.toolCalls = t.assembleToolCalls()
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
	// 作废判定落档（流式路径由运行时在 Finalize 前调用 ShouldAbortTurn
	// 提前返回；此处赋值供非流式/日志使用）。
	t.result.DegenerateToolCalls = t.ShouldAbortTurn()

	// 包装形状被扣住的调用：拆包（assembleToolCalls 已改写 acc.args）后
	// 一次性补发完整参数；未能拆包的原样补发，保证不丢字节。
	// 退化扣留（degenHold）的调用仅在不作废（客户端已见过本回合内容）时
	// 补发，保持行为完整。
	var tail []Delta
	aborting := t.result.DegenerateToolCalls
	// 同 index 复用拆分后可能出现「{} 头帧 + 真参数调用」混合回合：
	// 空头帧是上游复用痕迹，直接抑制，只交付真调用。
	hasRealCall := false
	for _, acc := range t.toolAccSeq {
		if acc.name != "" && !acc.degenHold {
			hasRealCall = true
			break
		}
	}
	for _, acc := range t.toolAccSeq {
		if acc.name == "" {
			continue
		}
		if acc.degenHold && !acc.owesBytes {
			if !aborting && !hasRealCall {
				slot := acc.slot
				tail = append(tail, Delta{ToolCalls: []ToolCall{{
					ID: acc.id, Name: acc.name, Index: &slot, Arguments: acc.args.String(),
				}}})
			}
			continue
		}
		if !acc.owesBytes {
			continue
		}
		slot := acc.slot
		// 首帧即完整参数且被扣住的调用，id/name 从未透传过：补发片段必须
		// 带上，供下游宣布 tool_use 块。
		tail = append(tail, Delta{ToolCalls: []ToolCall{{
			ID:        acc.id,
			Name:      acc.name,
			Index:     &slot,
			Arguments: acc.args.String(),
		}}})
		acc.owesBytes = false
	}
	return &t.result, tail
}

// repairWrappedToolArguments 拆掉模型模仿 function_call 信封产生的
// {"arguments":"<JSON 字符串>"} 包装：单键 arguments、值为字符串（可叠
// 1~2 层 JSON 字符串转义）、最内层为 JSON 对象时取最内层对象；其它形状
// 原样返回（含真实以 arguments 为参数的工具）。
func repairWrappedToolArguments(raw string) string {
	body := strings.TrimSpace(raw)
	if !bytes.HasPrefix([]byte(body), wrapperKeyPrefix) {
		return raw
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(body), &obj); err != nil || len(obj) != 1 {
		return raw
	}
	val, ok := obj["arguments"]
	if !ok {
		return raw
	}
	for i := 0; i < 3; i++ {
		var s string
		if json.Unmarshal(val, &s) != nil {
			break
		}
		inner := json.RawMessage(strings.TrimSpace(s))
		var probe any
		if json.Unmarshal(inner, &probe) != nil {
			return raw
		}
		val = inner
	}
	var final any
	if json.Unmarshal(val, &final) != nil {
		return raw
	}
	if _, isObject := final.(map[string]any); !isObject {
		return raw
	}
	return string(val)
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

// NormalizeIDEToolChoice 归一化 tool_choice：上游 llm_utils_chat 的字段是
// 纯字符串，对象形态（OpenAI 指定工具/any）会直接 400。字符串透传，对象
// 降级为 "auto"（上游不支持按名强制），空值返回 nil（不下发该键）。
func NormalizeIDEToolChoice(raw json.RawMessage) any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil
	}
	if s, ok := v.(string); ok {
		return s
	}
	return "auto"
}
