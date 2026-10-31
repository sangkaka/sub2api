package service

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
)

// 把 translator 的 ToolCall 片段按 runtime 同款映射（自带 slot 直用）转成 ChatToolCall。
func xlateCall(c trae.ToolCall) apicompat.ChatToolCall {
	idx := 0
	if c.Index != nil {
		idx = *c.Index
	}
	return apicompat.ChatToolCall{Index: &idx, ID: c.ID, Type: "function",
		Function: apicompat.ChatFunctionCall{Name: c.Name, Arguments: c.Arguments}}
}

// validateCC 模拟 CC 的流校验：块生命周期 + delta 类型匹配。
func validateCC(evts []apicompat.AnthropicStreamEvent) string {
	type blk struct{ typ, name string; open bool }
	blocks := map[int]*blk{}
	next := 0
	started := false
	for i, e := range evts {
		switch e.Type {
		case "message_start":
			if started {
				return fmt.Sprintf("evt[%d] double message_start", i)
			}
			started = true
		case "content_block_start":
			if e.Index == nil || e.ContentBlock == nil {
				return fmt.Sprintf("evt[%d] start nil", i)
			}
			if _, ok := blocks[*e.Index]; ok {
				return fmt.Sprintf("evt[%d] start reopen idx=%d", i, *e.Index)
			}
			if *e.Index != next {
				return fmt.Sprintf("evt[%d] start nonseq idx=%d want=%d", i, *e.Index, next)
			}
			next++
			blocks[*e.Index] = &blk{typ: e.ContentBlock.Type, open: true}
		case "content_block_delta":
			if e.Index == nil || e.Delta == nil {
				return fmt.Sprintf("evt[%d] delta nil", i)
			}
			b, ok := blocks[*e.Index]
			if !ok {
				return fmt.Sprintf("evt[%d] delta UNSTARTED block idx=%d", i, *e.Index)
			}
			if !b.open {
				return fmt.Sprintf("evt[%d] delta CLOSED block idx=%d (%s)", i, *e.Index, b.typ)
			}
			switch e.Delta.Type {
			case "text_delta":
				if b.typ != "text" {
					return fmt.Sprintf("evt[%d] TYPE MISMATCH text_delta on %s idx=%d", i, b.typ, *e.Index)
				}
			case "thinking_delta":
				if b.typ != "thinking" {
					return fmt.Sprintf("evt[%d] TYPE MISMATCH thinking_delta on %s idx=%d", i, b.typ, *e.Index)
				}
			case "input_json_delta":
				if b.typ != "tool_use" {
					return fmt.Sprintf("evt[%d] TYPE MISMATCH input_json_delta on %s idx=%d", i, b.typ, *e.Index)
				}
			}
		case "content_block_stop":
			if e.Index == nil {
				return fmt.Sprintf("evt[%d] stop nil", i)
			}
			b, ok := blocks[*e.Index]
			if !ok {
				return fmt.Sprintf("evt[%d] stop UNSTARTED idx=%d", i, *e.Index)
			}
			if !b.open {
				return fmt.Sprintf("evt[%d] stop CLOSED idx=%d", i, *e.Index)
			}
			b.open = false
		case "message_delta", "message_stop":
		default:
			return fmt.Sprintf("evt[%d] unknown %s", i, e.Type)
		}
	}
	for idx, b := range blocks {
		if b.open {
			return fmt.Sprintf("block idx=%d left open", idx)
		}
	}
	return ""
}

func toolFrame(idx int, id, name, args string) trae.Event {
	return trae.Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": idx, "id": id,
			"function_call": map[string]any{"name": name, "arguments": args},
		}},
	}}
}

func textFrame(s string) trae.Event {
	return trae.Event{Name: "output", Data: map[string]any{"response": s}}
}

func thinkFrame(s string) trae.Event {
	return trae.Event{Name: "output", Data: map[string]any{"reasoning_content": s}}
}

// TestFuzzInterposerBlockLifecycle 随机上游序列 → 真实 translator + interposer，找 CC 会判 malformed 的序列。
func TestFuzzInterposerBlockLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip()
	}
	rng := rand.New(rand.NewSource(20261031))
	argFrags := []string{"", "{}", `{"a":`, `1}`, `{"command":"ls"}`, `{"`, `command"`, `:"x"}`, `{"file_path":"f"}`}
	found := 0
	for iter := 0; iter < 4000 && found < 5; iter++ {
		guard := map[string]bool{"Bash": true}
		var withArgs map[string]bool // 部分迭代不给 schema
		if iter%3 == 0 {
			withArgs = map[string]bool{"Run": true}
		}
		tr := trae.NewDegenerateGuardIDETranslator("m", guard)
		tr.ToolsWithArgumentsParam = withArgs
		var events []trae.Event
		n := 1 + rng.Intn(10)
		callSeq := 0
		for i := 0; i < n; i++ {
			switch rng.Intn(10) {
			case 0, 1:
				events = append(events, textFrame("t"+fmt.Sprint(i)))
			case 2:
				events = append(events, thinkFrame("h"+fmt.Sprint(i)))
			default:
				idx := rng.Intn(3)
				callSeq++
				name := ""
				if rng.Intn(3) > 0 {
					name = "Bash"
				}
				args := argFrags[rng.Intn(len(argFrags))]
				events = append(events, toolFrame(idx, fmt.Sprintf("call_%d", callSeq), name, args))
			}
		}
		// 依序喂给 translator，收集全部 Delta（含 Finalize 尾帧），再喂 interposer。
		var chatChunks []*apicompat.ChatCompletionsChunk
		addChunk := func(calls []apicompat.ChatToolCall, content, reasoning string) {
			d := apicompat.ChatDelta{}
			if content != "" {
				d.Content = &content
			}
			if reasoning != "" {
				d.ReasoningContent = &reasoning
			}
			d.ToolCalls = calls
			chatChunks = append(chatChunks, &apicompat.ChatCompletionsChunk{Choices: []apicompat.ChatChunkChoice{{Index: 0, Delta: d}}})
		}
		for _, ev := range events {
			for _, d := range tr.ProcessEvent(ev) {
				var calls []apicompat.ChatToolCall
				for _, c := range d.ToolCalls {
					calls = append(calls, xlateCall(c))
				}
				addChunk(calls, d.Content, d.Reasoning)
			}
		}
		_, tail := tr.Finalize(false)
		fin := "tool_calls"
		addChunk(nil, "", "")
		chatChunks[len(chatChunks)-1].Choices[0].FinishReason = &fin
		for _, d := range tail {
			var calls []apicompat.ChatToolCall
			for _, c := range d.ToolCalls {
				calls = append(calls, xlateCall(c))
			}
			addChunk(calls, d.Content, d.Reasoning)
		}
		// interposer
		state := apicompat.NewChatCompletionsToAnthropicStreamState("m")
		var evts []apicompat.AnthropicStreamEvent
		for _, c := range chatChunks {
			evts = append(evts, apicompat.ChatCompletionsChunkToAnthropicEvents(c, state)...)
		}
		evts = append(evts, apicompat.FinalizeChatCompletionsAnthropicStream(state)...)
		if v := validateCC(evts); v != "" {
			found++
			var sb strings.Builder
			for _, ev := range events {
				b, _ := json.Marshal(ev.Data)
				sb.WriteString(string(b) + "\n")
			}
			t.Errorf("VIOLATION iter=%d: %s\nupstream events:\n%s", iter, v, sb.String())
		}
	}
	if found == 0 {
		t.Log("no violation in 4000 random sequences")
	}
}
