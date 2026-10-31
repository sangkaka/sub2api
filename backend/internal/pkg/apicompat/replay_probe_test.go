package apicompat

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

func strp(s string) *string { return &s }
func intp(i int) *int       { return &i }

// replayChunks 按运行时顺序灌 chunk 并拼接 Finalize 事件。
func replayChunks(chunks []*ChatCompletionsChunk) []AnthropicStreamEvent {
	state := NewChatCompletionsToAnthropicStreamState("m")
	var evts []AnthropicStreamEvent
	for _, c := range chunks {
		evts = append(evts, ChatCompletionsChunkToAnthropicEvents(c, state)...)
	}
	evts = append(evts, FinalizeChatCompletionsAnthropicStream(state)...)
	return evts
}

// validateAnthropicSequence 严格校验事件序列的协议合法性，返回违规列表。
func validateAnthropicSequence(t *testing.T, evts []AnthropicStreamEvent) []string {
	t.Helper()
	var violations []string
	open := map[int]string{}
	nextIdx := 0
	sawStart, sawDelta, sawStop := false, false, false
	for i, e := range evts {
		switch e.Type {
		case "message_start":
			if sawStart {
				violations = append(violations, "double message_start")
			}
			if i != 0 {
				violations = append(violations, "message_start not first")
			}
			sawStart = true
		case "content_block_start":
			if e.ContentBlock == nil {
				violations = append(violations, "content_block_start without block")
				continue
			}
			idx := e.Index
			if idx == nil {
				violations = append(violations, "content_block_start nil index")
				continue
			}
			if _, dup := open[*idx]; dup {
				violations = append(violations, "content_block_start reopen open index")
			}
			if *idx != nextIdx {
				violations = append(violations, "content_block_start non-sequential index")
			}
			nextIdx++
			open[*idx] = e.ContentBlock.Type
			if sawDelta {
				violations = append(violations, "content_block_start after message_delta")
			}
		case "content_block_delta":
			if e.Index == nil {
				violations = append(violations, "delta nil index")
				continue
			}
			if _, ok := open[*e.Index]; !ok {
				violations = append(violations, "delta on closed/unopened block")
			}
			if sawDelta {
				violations = append(violations, "delta after message_delta")
			}
		case "content_block_stop":
			if e.Index == nil {
				violations = append(violations, "stop nil index")
				continue
			}
			if _, ok := open[*e.Index]; !ok {
				violations = append(violations, "stop on closed/unopened block")
			}
			delete(open, *e.Index)
		case "message_delta":
			if len(open) > 0 {
				violations = append(violations, "message_delta with open blocks")
			}
			if sawDelta {
				violations = append(violations, "double message_delta")
			}
			sawDelta = true
		case "message_stop":
			if len(open) > 0 {
				violations = append(violations, "message_stop with open blocks")
			}
			if !sawDelta {
				violations = append(violations, "message_stop without message_delta")
			}
			sawStop = true
		default:
			violations = append(violations, "unknown event " + e.Type)
		}
		if sawStop && i != len(evts)-1 {
			violations = append(violations, "events after message_stop")
		}
	}
	if !sawStop {
		violations = append(violations, "no message_stop")
	}
	return violations
}

func dumpEvents(evts []AnthropicStreamEvent) string {
	var b strings.Builder
	for _, e := range evts {
		idx := -1
		if e.Index != nil {
			idx = *e.Index
		}
		detail := ""
		if e.ContentBlock != nil {
			detail = " block=" + e.ContentBlock.Type + " name=" + e.ContentBlock.Name + " id=" + e.ContentBlock.ID
		}
		if e.Delta != nil {
			dj, _ := json.Marshal(e.Delta)
			detail = " delta=" + string(dj)
		}
		b.WriteString(e.Type + " idx=" + strconv.Itoa(idx) + detail + "\n")
	}
	return b.String()
}

// 场景 A：部署后失败形态——text 直通 + 收尾补发 held "{}" 工具块（id+name+args 同帧）+ finish + usage。
func TestReplayHeldEmptyToolTailSequence(t *testing.T) {
	chunks := []*ChatCompletionsChunk{
		{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Content: strp("明白。CSV 照常入库。")}}}},
		{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ToolCalls: []ChatToolCall{{Index: intp(0), ID: "call_x", Function: ChatFunctionCall{Name: "Bash", Arguments: "{}"}}}}}}},
		{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: strp("tool_calls")}}},
		{ID: "c1", Object: "chat.completion.chunk", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}}}, Usage: &ChatUsage{PromptTokens: 10, CompletionTokens: 5}},
	}
	evts := replayChunks(chunks)
	t.Log("\n" + dumpEvents(evts))
	if v := validateAnthropicSequence(t, evts); len(v) > 0 {
		t.Errorf("PROTOCOL VIOLATIONS: %v", v)
	}
}

// 场景 B：thinking + text + 收尾补发工具块（deepseek 常见形态）。
func TestReplayThinkingTextHeldTool(t *testing.T) {
	chunks := []*ChatCompletionsChunk{
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ReasoningContent: strp("思考中")}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Content: strp("跑一下。")}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ToolCalls: []ChatToolCall{{Index: intp(0), ID: "call_y", Function: ChatFunctionCall{Name: "Bash", Arguments: "{}"}}}}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: strp("tool_calls")}}},
	}
	evts := replayChunks(chunks)
	t.Log("\n" + dumpEvents(evts))
	if v := validateAnthropicSequence(t, evts); len(v) > 0 {
		t.Errorf("PROTOCOL VIOLATIONS: %v", v)
	}
}

// 场景 C：直通工具流（首帧 name/id 空 args + 后续 args 分片 + finish）——健康路径对照。
func TestReplayStreamingToolHealthy(t *testing.T) {
	frag := func(args string) ChatToolCall {
		return ChatToolCall{Index: intp(0), Function: ChatFunctionCall{Arguments: args}}
	}
	head := ChatToolCall{Index: intp(0), ID: "call_z", Function: ChatFunctionCall{Name: "add"}}
	chunks := []*ChatCompletionsChunk{
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ToolCalls: []ChatToolCall{head}}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ToolCalls: []ChatToolCall{frag(`{"a":`)}}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ToolCalls: []ChatToolCall{frag(`5,"b":6}`)}}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: strp("tool_calls")}}},
	}
	evts := replayChunks(chunks)
	t.Log("\n" + dumpEvents(evts))
	if v := validateAnthropicSequence(t, evts); len(v) > 0 {
		t.Errorf("PROTOCOL VIOLATIONS: %v", v)
	}
}

// 场景 D：多工具收尾补发（并行双调用，index 0/1）。
func TestReplayTwoHeldToolsTail(t *testing.T) {
	chunks := []*ChatCompletionsChunk{
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{Content: strp("两件事一起做。")}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{ToolCalls: []ChatToolCall{
			{Index: intp(0), ID: "call_a", Function: ChatFunctionCall{Name: "Bash", Arguments: `{"command":"ls"}`}},
			{Index: intp(1), ID: "call_b", Function: ChatFunctionCall{Name: "Read", Arguments: `{"file_path":"x"}`}},
		}}}}},
		{ID: "c1", Model: "m", Choices: []ChatChunkChoice{{Index: 0, Delta: ChatDelta{}, FinishReason: strp("tool_calls")}}},
	}
	evts := replayChunks(chunks)
	t.Log("\n" + dumpEvents(evts))
	if v := validateAnthropicSequence(t, evts); len(v) > 0 {
		t.Errorf("PROTOCOL VIOLATIONS: %v", v)
	}
}
