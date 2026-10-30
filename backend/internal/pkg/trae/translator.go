package trae

import (
	"encoding/json"
	"fmt"
	"strings"
)

// SSE 事件名（归一化后）。
const (
	EventHeartbeat      = "heartbeat"
	EventModelConfig    = "model_config"
	EventPlanItem       = "plan_item"
	EventMessage        = "message"
	EventTokenUsage     = "token_usage"
	EventDone           = "done"
	EventError          = "error"
	EventRequestWaitInQ = "request_wait_in_queue"
)

// eventNameAliases 把上游各种拼写（无分隔/驼峰）折叠到归一名。
var eventNameAliases = map[string]string{
	"keepalive":          "keepalive",
	"modelconfig":        EventModelConfig,
	"planitem":           EventPlanItem,
	"requestwaitinqueue": EventRequestWaitInQ,
	"responsedone":       EventDone,
	"streamdone":         EventDone,
	"tokenusage":         EventTokenUsage,
}

// NormalizeEventName 把原始事件名归一化为 snake_case 并应用别名表。
func NormalizeEventName(value string) string {
	text := strings.TrimSpace(value)
	if text == "" {
		return ""
	}
	// 驼峰 → 下划线分隔。
	var b strings.Builder
	b.Grow(len(text) + 4)
	for i, r := range text {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				prev := rune(text[i-1])
				if (prev >= 'a' && prev <= 'z') || (prev >= '0' && prev <= '9') {
					b.WriteByte('_')
				}
			}
			b.WriteRune(r - 'A' + 'a')
			continue
		}
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	normalized := strings.Trim(b.String(), "_")
	normalized = strings.ReplaceAll(normalized, "__", "_")
	if alias, ok := eventNameAliases[strings.ReplaceAll(normalized, "_", "")]; ok {
		return alias
	}
	return normalized
}

// ChatMessage 是入站 OpenAI Chat Completions 消息的最小形状。
type ChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	Name       string          `json:"name,omitempty"`
}

// FlattenMessages 把 OpenAI 消息扁平化为 Remote query JSON 字符串。
// 角色以标记行区分（[System]/[Assistant]/[Client Tool Result]），用户消息
// 原文；整体包成 [{"type":"text","data":{"content":...}}]。
func FlattenMessages(messages []ChatMessage) string {
	parts := make([]string, 0, len(messages))
	for _, m := range messages {
		content := ContentToText(m.Content)
		switch strings.ToLower(strings.TrimSpace(m.Role)) {
		case "system", "developer":
			if strings.TrimSpace(content) != "" {
				parts = append(parts, "[System]\n"+content)
			}
		case "assistant":
			assistantParts := make([]string, 0, 2)
			if strings.TrimSpace(content) != "" {
				assistantParts = append(assistantParts, content)
			}
			if toolHistory := SerializeAssistantToolCalls(m.ToolCalls); toolHistory != "" {
				assistantParts = append(assistantParts, toolHistory)
			}
			if len(assistantParts) > 0 {
				parts = append(parts, "[Assistant]\n"+strings.Join(assistantParts, "\n\n"))
			}
		case "tool":
			toolID := m.ToolCallID
			if toolID == "" {
				toolID = "unknown"
			}
			toolName := m.Name
			if toolName == "" {
				toolName = "tool"
			}
			parts = append(parts, fmt.Sprintf("[Client Tool Result: %s %s status=succeeded]\n%s", toolID, toolName, content))
		default: // user
			if strings.TrimSpace(content) != "" {
				parts = append(parts, content)
			}
		}
	}
	text := strings.Join(parts, "\n\n")
	payload, _ := json.Marshal([]map[string]any{{"type": "text", "data": map[string]any{"content": text}}})
	return string(payload)
}

// ContentToText 把 OpenAI content（字符串或 blocks 数组）提取为纯文本。
func ContentToText(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
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
			blockType := strings.ToLower(anyToString(block["type"]))
			if blockType == "image_url" || blockType == "input_audio" {
				continue // v1 不支持多模态，忽略附件块
			}
			if text, ok := block["text"].(string); ok {
				parts = append(parts, text)
			}
		}
		return strings.Join(parts, "")
	}
	return ""
}

// textAccumulator 是累计快照差分器：上游整段重发文本，按公共前缀长度计算
// 增量；快照回退（当前是之前的前缀）时返回空增量。
type textAccumulator struct {
	previous string
}

func (a *textAccumulator) delta(current string) string {
	if current == "" {
		return ""
	}
	if strings.HasPrefix(current, a.previous) {
		delta := current[len(a.previous):]
		a.previous = current
		return delta
	}
	if strings.HasPrefix(a.previous, current) {
		return ""
	}
	common := commonPrefixLength(a.previous, current)
	delta := current[common:]
	a.previous = current
	return delta
}

func commonPrefixLength(left, right string) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	index := 0
	for index < limit && left[index] == right[index] {
		index++
	}
	return index
}

// suffixPrefixOverlap 返回 left 后缀与 right 前缀的最长重叠（< minimum 视为无重叠）。
func suffixPrefixOverlap(left, right string, minimum int) int {
	limit := len(left)
	if len(right) < limit {
		limit = len(right)
	}
	for size := limit; size >= minimum; size-- {
		if left[len(left)-size:] == right[:size] {
			return size
		}
	}
	return 0
}

// mergePlanMessageText 合并 plan 快照流与 message 快照流而不重放任何一段。
func mergePlanMessageText(planText, messageText string) string {
	plan, message := planText, messageText
	if plan == "" {
		return message
	}
	if message == "" {
		return plan
	}
	if plan == message || strings.HasPrefix(plan, message) || strings.HasSuffix(plan, message) {
		return plan
	}
	if strings.HasPrefix(message, plan) {
		return message
	}
	if overlap := suffixPrefixOverlap(plan, message, 4); overlap > 0 {
		return plan + message[overlap:]
	}
	return strings.TrimRight(plan, " \t\n") + "\n\n" + strings.TrimLeft(message, " \t\n")
}

// Delta 是一次事件处理后待下发的 OpenAI 增量。
type Delta struct {
	// Content 进 delta.content。
	Content string
	// Reasoning 进 delta.reasoning_content。
	Reasoning string
	// ToolCalls 是文本桥扫描出的客户端工具调用（delta.tool_calls）。
	ToolCalls []ToolCall
	// Heartbeat 为 true 时下发 SSE 注释行（: keepalive）而非 chunk。
	Heartbeat bool
}

// Usage 是映射后的 OpenAI usage 形状。
type Usage struct {
	PromptTokens        int                 `json:"prompt_tokens"`
	CompletionTokens    int                 `json:"completion_tokens"`
	TotalTokens         int                 `json:"total_tokens"`
	CreditsConsumed     *float64            `json:"credits_consumed,omitempty"`
	PromptTokensDetails *UsageTokensDetails `json:"prompt_tokens_details,omitempty"`
}

// UsageTokensDetails 携带缓存命中等细分。
type UsageTokensDetails struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}

// MapUsage 把 TRAE token_usage 载荷映射为 OpenAI usage（键名兼容蛇形/驼峰）。
func MapUsage(raw map[string]any) *Usage {
	usage := raw
	if nested, ok := raw["usage"].(map[string]any); ok {
		usage = nested
	}
	prompt := numberOf(usage, "prompt_tokens", "input_tokens", "inputTokens")
	completion := numberOf(usage, "completion_tokens", "output_tokens", "outputTokens")
	total := numberOf(usage, "total_tokens", "totalTokens")
	if total == 0 {
		total = prompt + completion
	}
	out := &Usage{PromptTokens: prompt, CompletionTokens: completion, TotalTokens: total}
	if credits := optionalNumber(usage, "credits_consumed", "consumed_credits", "credit_cost", "credits_cost", "creditsFloat"); credits != nil {
		out.CreditsConsumed = credits
	}
	if cached := optionalNumber(usage, "cache_read_tokens", "cacheReadTokens", "cached_tokens", "cachedTokens"); cached != nil {
		out.PromptTokensDetails = &UsageTokensDetails{CachedTokens: int(*cached)}
	}
	return out
}

func numberOf(m map[string]any, keys ...string) int {
	if v := optionalNumber(m, keys...); v != nil {
		return int(*v)
	}
	return 0
}

func optionalNumber(m map[string]any, keys ...string) *float64 {
	for _, key := range keys {
		var value float64
		switch v := m[key].(type) {
		case float64:
			value = v
		case int:
			value = float64(v)
		case int64:
			value = float64(v)
		default:
			continue
		}
		if value < 0 {
			value = 0
		}
		return &value
	}
	return nil
}

// TurnResult 是一轮会话的最终汇总（流式收尾/非流式组装共用）。
type TurnResult struct {
	Content       string
	Reasoning     string
	Usage         *Usage
	FinishReason  string
	ProviderModel string
	// Truncated 为 true 表示事件流结束但未见 done 帧（上游截断）。
	Truncated bool
	// Err 是 error 事件的归类结果（nil 表示正常收尾）。
	Err *UpstreamError
	// ToolCalls 是文本桥扫描出的全部调用（非流式组装 message.tool_calls 用）。
	ToolCalls []ToolCall
}

// StreamTranslator 把 Remote SSE 事件流翻译为 OpenAI 增量。非并发安全，
// 每个上游会话轮次一个实例。
type StreamTranslator struct {
	model         string
	toolScanner   ToolCallScanner
	hasToolCalls  bool
	started       bool
	sawOutput     bool // 已观察到模型输出（正文/思考/usage），错误时不得重放
	reasoning     map[string]*textAccumulator
	reasoningKeys []string
	planVisible   map[string]*textAccumulator
	message       textAccumulator
	messageText   string
	streamed      string // 扫描后的可见正文（对外 Content）
	streamedRaw   string // 扫描前的原始正文（快照差分基准，含工具标记）
	planText      string
	usage         *Usage
	finish        string
	provider      string
	result        TurnResult
	finalized     bool
}

// NewStreamTranslator 创建翻译器；model 是对外回显的模型名。
func NewStreamTranslator(model string) *StreamTranslator {
	return &StreamTranslator{
		model:       model,
		reasoning:   map[string]*textAccumulator{},
		planVisible: map[string]*textAccumulator{},
		finish:      "stop",
	}
}

// Started 报告是否已下发过角色帧（服务层据此决定是否补发 role chunk）。
func (t *StreamTranslator) Started() bool { return t.started }

// Model 返回构造时的对外模型名。
func (t *StreamTranslator) Model() string { return t.model }

// ProviderModel 返回 model_config 事件回报的上游实际模型名。
func (t *StreamTranslator) ProviderModel() string { return t.provider }

// SawOutput 报告是否已观察到模型输出（错误映射的不可重放判定）。
func (t *StreamTranslator) SawOutput() bool { return t.sawOutput }

// FallbackError 判定 Remote 流中需要回落 IDE 端点的错误事件：业务码
// 1005 且尚未观察到模型输出（kimi-k3.1-pro 实测固定复现）。
func (t *StreamTranslator) FallbackError(ev Event) *UpstreamError {
	if ev.Name != EventError || t.sawOutput {
		return nil
	}
	code, _ := ExtractErrorDetails(ev.Data)
	if code != IDEFallbackCode {
		return nil
	}
	return &UpstreamError{
		Kind:    ErrorKindUpstream,
		Code:    IDEFallbackCode,
		Message: "trae remote rejected this model (code 1005); retry via llm_utils_chat",
	}
}

// ProcessEvent 消费一个事件，返回待下发增量（可能为空）。
func (t *StreamTranslator) ProcessEvent(ev Event) []Delta {
	switch ev.Name {
	case EventHeartbeat, "keepalive", "ping":
		// 心跳必须透传：静默丢弃会让下游客户端误判回合结束而重放请求。
		return []Delta{{Heartbeat: true}}
	case EventError:
		code, message := ExtractErrorDetails(ev.Data)
		t.result.Err = ClassifyUpstreamError(code, message, t.sawOutput)
		return nil
	case EventModelConfig:
		if name := providerModelName(ev.Data); name != "" {
			t.provider = name
		}
		return nil
	case EventTokenUsage:
		t.usage = MapUsage(ev.Data)
		t.sawOutput = true
		return nil
	case EventPlanItem:
		return t.processPlanItem(ev.Data)
	case EventMessage, "assistant_message", "response", "text", "output":
		return t.processMessage(ev.Data)
	case EventDone:
		if reason := payloadFinishReason(ev.Data); reason != "" {
			t.finish = reason
		}
		return nil
	default:
		// 未知事件（如 request_wait_in_queue）忽略。
		return nil
	}
}

func (t *StreamTranslator) processPlanItem(data map[string]any) []Delta {
	var deltas []Delta
	planID := planStateKey(data, len(t.reasoningKeys))

	// 思考链快照差分 → reasoning_content。当前布局携带 reasoning_content
	// 字段；老布局用 thought 承载思考。
	reasoningSnapshot := planReasoning(data)
	if state, ok := t.reasoning[planID]; ok {
		if delta := state.delta(reasoningSnapshot); delta != "" {
			t.sawOutput = true
			deltas = append(deltas, Delta{Reasoning: delta})
		}
	} else {
		t.reasoning[planID] = &textAccumulator{}
		t.reasoningKeys = append(t.reasoningKeys, planID)
		if reasoningSnapshot != "" {
			t.reasoning[planID].delta(reasoningSnapshot)
			t.sawOutput = true
			deltas = append(deltas, Delta{Reasoning: reasoningSnapshot})
		}
	}

	// 正文快照差分（content 是唯一公开答案文本面）。
	visibleState, ok := t.planVisible[planID]
	if !ok {
		visibleState = &textAccumulator{}
		t.planVisible[planID] = visibleState
	}
	if contentDelta := visibleState.delta(planContent(data)); contentDelta != "" {
		t.planText += contentDelta
		if public := textDelta(t.streamedRaw, mergePlanMessageText(t.planText, t.messageText)); public != "" {
			t.sawOutput = true
			deltas = append(deltas, t.contentDelta(public)...)
		}
	}
	return deltas
}

// contentDelta 把正文增量经工具标记扫描器分流：标记外文本透出（并计入
// streamed），完整 <opencode_tool_call> 块转为 ToolCalls 增量。
func (t *StreamTranslator) contentDelta(text string) []Delta {
	t.streamedRaw += text
	visible, calls := t.toolScanner.Feed(text)
	t.streamed += visible
	var deltas []Delta
	if visible != "" {
		deltas = append(deltas, Delta{Content: visible})
	}
	if len(calls) > 0 {
		t.hasToolCalls = true
		deltas = append(deltas, Delta{ToolCalls: calls})
	}
	return deltas
}

func (t *StreamTranslator) processMessage(data map[string]any) []Delta {
	text := messageText(data)
	if text == "" {
		return nil
	}
	if delta := t.message.delta(text); delta != "" {
		t.messageText = t.message.previous
		if public := textDelta(t.streamedRaw, mergePlanMessageText(t.planText, t.messageText)); public != "" {
			t.sawOutput = true
			return t.contentDelta(public)
		}
	}
	return nil
}

// Finalize 收尾：汇总回合结果。streamEndedWithoutDone 为 true 表示事件流
// EOF 但未收到 done 帧（判为截断）。
func (t *StreamTranslator) Finalize(streamEndedWithoutDone bool) (*TurnResult, []Delta) {
	if t.finalized {
		return &t.result, nil
	}
	t.finalized = true
	var deltas []Delta
	// 各 plan 可见态收尾（清空未决快照）。
	for _, key := range sortedKeys(t.planVisible) {
		if state, ok := t.planVisible[key]; ok {
			if contentDelta := state.delta(state.previous); contentDelta != "" {
				t.planText += contentDelta
			}
		}
	}
	merged := mergePlanMessageText(t.planText, t.messageText)
	if public := textDelta(t.streamedRaw, merged); public != "" && strings.TrimSpace(public) != "" {
		deltas = append(deltas, t.contentDelta(public)...)
	}
	if tail := t.toolScanner.Flush(); tail != "" {
		t.streamed += tail
		deltas = append(deltas, Delta{Content: tail})
	}
	t.result = TurnResult{
		Content:       t.streamed,
		ToolCalls:     t.toolScanner.Calls(),
		Reasoning:     joinReasoning(t.reasoning, t.reasoningKeys),
		Usage:         t.usage,
		FinishReason:  t.finish,
		ProviderModel: t.provider,
		Truncated:     streamEndedWithoutDone,
		Err:           t.result.Err,
	}
	return &t.result, deltas
}

func textDelta(previous, current string) string {
	if current == "" {
		return ""
	}
	if strings.HasPrefix(current, previous) {
		return current[len(previous):]
	}
	if strings.HasPrefix(previous, current) {
		return ""
	}
	return current[commonPrefixLength(previous, current):]
}

func joinReasoning(states map[string]*textAccumulator, order []string) string {
	var parts []string
	for _, key := range order {
		if state, ok := states[key]; ok && state.previous != "" {
			parts = append(parts, state.previous)
		}
	}
	return strings.Join(parts, "\n")
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	// map 无序但 plan id 的插入序已在 reasoningKeys；可见态按同序遍历。
	sortStrings(keys)
	return keys
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// planStateKey 是 plan_item 差分状态的键：会话 id + plan item id（缺失时按
// 到达序生成稳定占位）。
func planStateKey(data map[string]any, fallbackIndex int) string {
	planID := firstStringValue([]map[string]any{data}, "id", "plan_item_id", "planItemId", "planId", "plan_id")
	if planID == "" {
		planID = fmt.Sprintf("plan-%d", fallbackIndex)
	}
	return planID
}

// planReasoning 提取思考链快照。新布局带 reasoning_content 字段（thought
// 是可见答案）；老布局 thought 即思考。
func planReasoning(data map[string]any) string {
	if hasSplitReasoning(data) {
		if v, ok := data["reasoning_content"].(string); ok && v != "" {
			return v
		}
		if v, ok := data["reasoningContent"].(string); ok && v != "" {
			return v
		}
		return ""
	}
	if v, ok := data["thought"].(string); ok {
		return v
	}
	return ""
}

func hasSplitReasoning(data map[string]any) bool {
	_, hasReasoning := data["reasoning_content"]
	_, hasReasoningCamel := data["reasoningContent"]
	return hasReasoning || hasReasoningCamel
}

// planContent 提取 plan_item 的公开答案快照：content 字段；新布局下 content
// 为空时 thought 承载答案。
func planContent(data map[string]any) string {
	if content := messageText(map[string]any{"content": data["content"]}); content != "" {
		return content
	}
	if hasSplitReasoning(data) {
		if thought, ok := data["thought"].(string); ok {
			return thought
		}
	}
	return ""
}

// messageText 提取 message/assistant_message 事件的可见文本（content 为
// 字符串或 blocks 数组，跳过思考块）。
func messageText(data map[string]any) string {
	for _, key := range []string{"message", "agent_message", "assistant_message"} {
		if nested, ok := data[key].(map[string]any); ok {
			data = nested
			break
		}
	}
	switch content := data["content"].(type) {
	case string:
		return content
	case []any:
		var parts []string
		for _, block := range content {
			switch b := block.(type) {
			case string:
				parts = append(parts, b)
			case map[string]any:
				blockType := strings.ToLower(anyToString(b["type"]))
				if blockType == "reasoning" || blockType == "thinking" || blockType == "reasoning_text" {
					continue
				}
				if text, ok := b["text"].(string); ok {
					parts = append(parts, text)
				} else if text, ok := b["content"].(string); ok {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "")
	}
	for _, key := range []string{"text", "response", "answer", "output"} {
		switch v := data[key].(type) {
		case string:
			return v
		case []any:
			var parts []string
			for _, block := range v {
				if s, ok := block.(string); ok {
					parts = append(parts, s)
				} else if m, ok := block.(map[string]any); ok {
					if text, ok := m["text"].(string); ok {
						parts = append(parts, text)
					}
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, "")
			}
		}
	}
	return ""
}

// providerModelName 从 model_config（或附带 timing_cost 的帧）提取上游实际
// provider 模型名。
func providerModelName(data map[string]any) string {
	if v := firstStringValue([]map[string]any{data}, "provider_model_name", "providerModelName", "model_provider_name", "modelProviderName"); v != "" {
		return v
	}
	if timing, ok := data["timing_cost"].(map[string]any); ok {
		if v := firstStringValue([]map[string]any{timing}, "provider_model_name", "providerModelName", "model_provider_name", "modelProviderName"); v != "" {
			return v
		}
	}
	if config, ok := data["model_config"].(map[string]any); ok {
		if v := firstStringValue([]map[string]any{config}, "provider_model_name", "providerModelName", "model_name", "modelName"); v != "" {
			return v
		}
	}
	if v := firstStringValue([]map[string]any{data}, "model_name", "modelName"); v != "" {
		return v
	}
	return ""
}

// payloadFinishReason 提取 done 帧的 finish_reason。
func payloadFinishReason(data map[string]any) string {
	return firstStringValue([]map[string]any{data}, "finish_reason", "finishReason", "reason")
}

// reasoningEffortLevels 与 ClampReasoningEffort 实现 OpenAI 思考强度到
// TRAE 原生档位（light/high/extra_high）的映射与按 descriptor 钳制。
var reasoningEffortOrder = []string{"light", "high", "extra_high"}

var reasoningEffortAliases = map[string]string{
	"none": "light", "minimal": "light", "min": "light", "low": "light", "light": "light",
	"medium": "high", "mid": "high", "normal": "high", "default": "high", "high": "high",
	"xhigh": "extra_high", "x-high": "extra_high", "extra_high": "extra_high",
	"extra-high": "extra_high", "very_high": "extra_high", "max": "extra_high",
	"maximum": "extra_high", "ultra": "extra_high",
}

// NormalizeReasoningEffort 把 OpenAI 风格 effort 归一为 TRAE 档位；未知值
// 返回空（不传，让上游用模型默认）。
func NormalizeReasoningEffort(value string) string {
	key := strings.ToLower(strings.TrimSpace(value))
	key = strings.ReplaceAll(key, " ", "_")
	return reasoningEffortAliases[key]
}

// ClampReasoningEffort 按模型 descriptor 声明的档位钳制；descriptor 未声明
// 时原样返回（空则不生效）。
func ClampReasoningEffort(level string, descriptor *ModelDescriptor) string {
	if level == "" || descriptor == nil {
		return level
	}
	allowed := descriptor.ReasoningEfforts()
	if len(allowed) == 0 {
		return level
	}
	for _, candidate := range allowed {
		if candidate == level {
			return level
		}
	}
	// 取不超请求档位的最大值；请求高于全部声明档位则取最高。
	requested := reasoningEffortRank(level)
	best := allowed[0]
	for _, candidate := range allowed {
		if reasoningEffortRank(candidate) <= requested {
			best = candidate
		}
	}
	return best
}

func reasoningEffortRank(level string) int {
	for i, candidate := range reasoningEffortOrder {
		if candidate == level {
			return i
		}
	}
	return len(reasoningEffortOrder)
}
