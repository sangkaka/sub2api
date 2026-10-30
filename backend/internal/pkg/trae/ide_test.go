package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// llm_utils_chat 线上抓包形状（企业账号 kimi-k3.1-pro 实测）：output 为
// 增量 delta、token_usage 扁平 OpenAI 形、done 带 finish_reason。
func ideStreamFixture() string {
	return strings.Join([]string{
		`event: progress_notice` + "\n" + `data: "Processing_1793285215"` + "\n\n",
		`event: metadata` + "\n" + `data: {"model":"kimi-k3.1-pro","session_id":"93c6"}` + "\n\n",
		`event: output` + "\n" + `data: {"response":"","reasoning_content":"I need answer","tool_calls":null,"multimodal_contents":null,"phase":null}` + "\n\n",
		`event: output` + "\n" + `data: {"response":"1+1 等于 2。","reasoning_content":null,"tool_calls":null,"multimodal_contents":null,"phase":null}` + "\n\n",
		`event: extra_info` + "\n" + `data: {"model":"ep-20261024170518-882ll","content":"{}"}` + "\n\n",
		`event: token_usage` + "\n" + `data: {"prompt_tokens":96,"completion_tokens":68,"total_tokens":164,"cache_read_input_tokens":0,"reasoning_tokens":45}` + "\n\n",
		`event: done` + "\n" + `data: {"finish_reason":"stop"}` + "\n\n",
	}, "")
}

func TestIDEChatWire(t *testing.T) {
	var gotBody map[string]any
	var gotAuth, gotChannel, gotVersion string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, ideChatPath, r.URL.Path)
		gotAuth = r.Header.Get("Authorization")
		gotChannel = r.Header.Get("x-plugin-channel")
		gotVersion = r.Header.Get("x-ide-version")
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ideStreamFixture())
	}))
	defer server.Close()

	client := &Client{CloudIDEHost: server.URL}
	parallel := true
	events, errCh := client.IDEChat(context.Background(), "jwt-1", IDEChatRequest{
		Messages: []ChatMessage{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
			{Role: "assistant", Content: json.RawMessage(`"ok"`), ToolCalls: json.RawMessage(`[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]`)},
			{Role: "tool", Content: json.RawMessage(`"done"`), ToolCallID: "c1", Name: "f"},
		},
		ConfigName:        "kimi-k3.1-pro",
		UserID:            "433084928",
		MaxTokens:         500,
		Tools:             []ToolDefinition{{Type: "function", Function: json.RawMessage(`{"name":"f","description":"do f","parameters":{"type":"object","properties":{"path":{"type":"string"}}}}`)}},
		ToolChoice:        json.RawMessage(`"auto"`),
		ParallelToolCalls: &parallel,
	})

	translator := NewIDETranslator("kimi-k3.1-pro")
	var contentDeltas, reasoningDeltas []string
	for events != nil || errCh != nil {
		select {
		case ev, ok := <-events:
			if !ok {
				events = nil
				continue
			}
			for _, delta := range translator.ProcessEvent(ev) {
				if delta.Content != "" {
					contentDeltas = append(contentDeltas, delta.Content)
				}
				if delta.Reasoning != "" {
					reasoningDeltas = append(reasoningDeltas, delta.Reasoning)
				}
			}
		case err, ok := <-errCh:
			if !ok {
				errCh = nil
				continue
			}
			require.NoError(t, err)
		}
	}
	result, _ := translator.Finalize(false)

	require.Equal(t, "Cloud-IDE-JWT jwt-1", gotAuth)
	require.Equal(t, "icube-ai", gotChannel)
	require.Equal(t, ideSoloVersion, gotVersion)
	require.Equal(t, "kimi-k3.1-pro", gotBody["config_name"])
	require.Equal(t, ideFunction, gotBody["function"])
	require.Equal(t, true, gotBody["stream"])
	require.Equal(t, float64(500), gotBody["max_tokens"])
	require.Equal(t, "auto", gotBody["tool_choice"])
	require.Equal(t, true, gotBody["parallel_tool_calls"])
	// parameters 必须是 JSON 字符串（SOLO 编码）。
	tools := gotBody["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	require.IsType(t, "", fn["parameters"])
	require.Contains(t, fn["parameters"], "\"type\":\"object\"")
	messages := gotBody["messages"].([]any)
	require.Len(t, messages, 3)
	first := messages[0].(map[string]any)
	require.Equal(t, "user", first["role"])
	blocks := first["content"].([]any)
	require.Equal(t, map[string]any{"type": "text", "text": "hi"}, blocks[0])
	second := messages[1].(map[string]any)
	require.Equal(t, "assistant", second["role"])
	require.NotNil(t, second["tool_calls"])
	third := messages[2].(map[string]any)
	require.Equal(t, "tool", third["role"])
	require.Equal(t, "c1", third["tool_call_id"])

	require.Equal(t, []string{"1+1 等于 2。"}, contentDeltas)
	require.Equal(t, []string{"I need answer"}, reasoningDeltas)
	require.Equal(t, "1+1 等于 2。", result.Content)
	require.Equal(t, "I need answer", result.Reasoning)
	require.Equal(t, "stop", result.FinishReason)
	require.Nil(t, result.Err)
	require.False(t, result.Truncated)
	require.Equal(t, "ep-20261024170518-882ll", result.ProviderModel)
	require.NotNil(t, result.Usage)
	require.Equal(t, 96, result.Usage.PromptTokens)
	require.Equal(t, 68, result.Usage.CompletionTokens)
	require.Equal(t, 164, result.Usage.TotalTokens)
}

// output 帧原生 tool_calls：OpenAI delta 族形状，arguments 兼容对象。
func TestParseIDEToolCalls(t *testing.T) {
	calls := ParseIDEToolCalls([]any{
		map[string]any{"index": 0, "id": "c1", "type": "function",
			"function": map[string]any{"name": "read_file", "arguments": map[string]any{"path": "a.txt"}}},
		map[string]any{"id": "", "function_call": map[string]any{"name": "shell", "arguments": `{"cmd":"ls"}`}},
	})
	require.Len(t, calls, 2)
	require.Equal(t, "c1", calls[0].ID)
	require.Equal(t, "read_file", calls[0].Name)
	require.Equal(t, `{"path":"a.txt"}`, calls[0].Arguments)
	require.Equal(t, "shell", calls[1].Name)
	require.Equal(t, `{"cmd":"ls"}`, calls[1].Arguments)
	require.NotEmpty(t, calls[1].ID)
}

func TestIDEUsageKeyFamilies(t *testing.T) {
	openaiStyle := MapIDEUsage(map[string]any{"prompt_tokens": 10, "completion_tokens": 4})
	require.Equal(t, 14, openaiStyle.TotalTokens)
	legacyStyle := MapIDEUsage(map[string]any{"usage": map[string]any{"input_token": 7, "output_token": 3}})
	require.Equal(t, 7, legacyStyle.PromptTokens)
	require.Equal(t, 3, legacyStyle.CompletionTokens)
	require.Equal(t, 10, legacyStyle.TotalTokens)
}

// DeviceIDFor 与 service.TraeDeviceID 同源派生（sha256("trae"+uid) mod 1e16）。
func TestDeviceIDFor(t *testing.T) {
	id := DeviceIDFor("433084928")
	require.Len(t, id, 16)
	require.True(t, isAllDigits(id))
	require.Equal(t, id, DeviceIDFor("433084928"))
	require.NotEqual(t, id, DeviceIDFor("433129984"))
}

func feedIDEOutput(t *testing.T, tr *IDETranslator, data string) []Delta {
	t.Helper()
	var payload map[string]any
	require.NoError(t, json.Unmarshal([]byte(data), &payload))
	return tr.ProcessEvent(Event{Name: "output", Data: payload})
}

// 大参数工具调用会分片增量下发（生产 CC 实测："Invalid tool parameters"
// 事故）：首帧带 index/id/name、arguments 为空串，后续帧仅 index+
// arguments 片段。片段必须实时透传（带稳定槽位，长回合不得静默到收尾，
// 否则客户端空闲超时掐流——"response stopped arriving"），完整调用在
// Finalize 组装进 TurnResult。
func TestIDETranslatorToolCallFragments(t *testing.T) {
	tr := NewIDETranslator("kimi")

	var fragments []ToolCall
	for _, frame := range []string{
		`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"c1","type":"function","function_call":{"name":"write_file","arguments":""}}]}`,
		`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"function_call":{"arguments":"{\"path\":\"a"}}]}`,
		`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"function_call":{"arguments":".txt\",\"content\":\"hel"}}]}`,
		`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"function_call":{"arguments":"lo world\"}"}}]}`,
	} {
		for _, d := range feedIDEOutput(t, tr, frame) {
			fragments = append(fragments, d.ToolCalls...)
		}
	}
	// 首帧宣告（id+name），三个参数片段各成一帧，全部携带同一槽位。
	require.Len(t, fragments, 4)
	require.Equal(t, "c1", fragments[0].ID)
	require.Equal(t, "write_file", fragments[0].Name)
	require.Empty(t, fragments[0].Arguments)
	for _, frag := range fragments {
		require.NotNil(t, frag.Index, "片段必须携带槽位")
		require.Equal(t, 0, *frag.Index, "同一调用的片段槽位一致")
	}
	require.Equal(t, `{"path":"a`, fragments[1].Arguments)
	require.Equal(t, `lo world"}`, fragments[3].Arguments)

	// Finalize 组装完整调用，且不再有尾部增量（流中已交付）。
	result, tail := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 1)
	call := result.ToolCalls[0]
	require.Equal(t, "c1", call.ID)
	require.Equal(t, "write_file", call.Name)
	require.JSONEq(t, `{"path":"a.txt","content":"hello world"}`, call.Arguments)
	require.Equal(t, "tool_calls", result.FinishReason)
	require.Empty(t, tail)
}

// 并行多工具按 index 交错分片：片段各归其位（槽位不漂移），组装保持
// 首次出现顺序。
func TestIDETranslatorToolCallFragmentsInterleaved(t *testing.T) {
	tr := NewIDETranslator("kimi")
	var fragments []ToolCall
	for _, frame := range []string{
		`{"tool_calls":[{"index":0,"id":"c1","function_call":{"name":"read_file","arguments":""}},{"index":1,"id":"c2","function_call":{"name":"shell","arguments":""}}]}`,
		`{"tool_calls":[{"index":1,"function_call":{"arguments":"{\"cmd\":\"ls\"}"}}]}`,
		`{"tool_calls":[{"index":0,"function_call":{"arguments":"{\"path\":\"a.txt\"}"}}]}`,
	} {
		for _, d := range feedIDEOutput(t, tr, frame) {
			fragments = append(fragments, d.ToolCalls...)
		}
	}
	require.Len(t, fragments, 4)
	require.Equal(t, 1, *fragments[2].Index, "index=1 的片段必须归位槽位 1")
	require.Equal(t, 0, *fragments[3].Index, "index=0 的片段必须归位槽位 0")

	result, _ := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 2)
	require.Equal(t, "c1", result.ToolCalls[0].ID)
	require.JSONEq(t, `{"path":"a.txt"}`, result.ToolCalls[0].Arguments)
	require.Equal(t, "c2", result.ToolCalls[1].ID)
	require.JSONEq(t, `{"cmd":"ls"}`, result.ToolCalls[1].Arguments)
}

// 单帧完整调用（无分片）仍直接工作：带 id、仅 name、以及空参数补 "{}"。
func TestIDETranslatorToolCallSingleFrame(t *testing.T) {
	tr := NewIDETranslator("kimi")
	feedIDEOutput(t, tr, `{"tool_calls":[{"id":"c9","function_call":{"name":"shell","arguments":"{\"cmd\":\"pwd\"}"}}]}`)
	feedIDEOutput(t, tr, `{"tool_calls":[{"function":{"name":"ping","arguments":""}}]}`)

	result, _ := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 2)
	require.Equal(t, "c9", result.ToolCalls[0].ID)
	require.JSONEq(t, `{"cmd":"pwd"}`, result.ToolCalls[0].Arguments)
	require.Equal(t, "ping", result.ToolCalls[1].Name)
	require.JSONEq(t, `{}`, result.ToolCalls[1].Arguments)
	require.NotEmpty(t, result.ToolCalls[1].ID, "无 id 时必须生成稳定 id")
}

// 通道回落判定：IDE 模型不可服务（4001/4023）且未输出 → Remote 哨兵；
// 已有输出或其它错误码不回落。
func TestIDETranslatorFallbackError(t *testing.T) {
	tr := NewIDETranslator("m")
	require.Nil(t, tr.FallbackError(Event{Name: "error", Data: map[string]any{"code": 1001, "message": "auth"}}), "认证错误不构成通道回落")
	require.Nil(t, tr.FallbackError(Event{Name: "output", Data: map[string]any{"response": "hi"}}), "输出帧不构成回落")
	ue := tr.FallbackError(Event{Name: "error", Data: map[string]any{"code": 4023, "message": "not served"}})
	require.NotNil(t, ue)
	require.Equal(t, RemoteFallbackCode, ue.Code)
	tr2 := NewIDETranslator("m")
	tr2.ProcessEvent(Event{Name: "output", Data: map[string]any{"response": "hi"}})
	require.Nil(t, tr2.FallbackError(Event{Name: "error", Data: map[string]any{"code": 4023, "message": "late"}}), "已有输出不得回落")
}

// 上游 llm_utils_chat 的 tool_choice 是纯字符串；对象形态必须降级，否则 400。
func TestNormalizeIDEToolChoice(t *testing.T) {
	require.Equal(t, "auto", NormalizeIDEToolChoice(json.RawMessage(`"auto"`)))
	require.Equal(t, "none", NormalizeIDEToolChoice(json.RawMessage(`"none"`)))
	require.Equal(t, "auto", NormalizeIDEToolChoice(json.RawMessage(`{"type":"function","function":{"name":"Bash"}}`)))
	require.Equal(t, "auto", NormalizeIDEToolChoice(json.RawMessage(`{"type":"any"}`)))
	require.Nil(t, NormalizeIDEToolChoice(nil))
	require.Nil(t, NormalizeIDEToolChoice(json.RawMessage(``)))
	require.Nil(t, NormalizeIDEToolChoice(json.RawMessage(`null`)))
}

// 模型模仿 function_call 信封输出 {"arguments":"<JSON 字符串>"}（可叠两层
// 转义）：片段必须被扣住不下发，Finalize 拆包后一次性补发最内层对象。
func TestIDETranslatorToolCallWrappedArgumentsRepaired(t *testing.T) {
	cases := []struct {
		name string
		frag []string
		want string
	}{
		{"single-wrap", []string{`{"arguments": ` + strconv.Quote(`{"command":"ls -la"}`) + `}`}, `{"command":"ls -la"}`},
		{"double-wrap", []string{`{"arguments": ` + strconv.Quote(strconv.Quote(`{"command": "sed -n 1p f"}`)) + `}`}, `{"command": "sed -n 1p f"}`},
		{"split-prefix", []string{`{"arg`, `uments": ` + strconv.Quote(`{"command":"git status"}`) + `}`}, `{"command":"git status"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tr := NewIDETranslator("m")
			var streamed []string
			for _, frag := range tc.frag {
				for _, d := range tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
					"tool_calls": []any{map[string]any{
						"index":         0,
						"id":            "call_1",
						"function_call": map[string]any{"name": "Bash", "arguments": frag},
					}},
				}}) {
					for _, c := range d.ToolCalls {
						if c.Arguments != "" {
							streamed = append(streamed, c.Arguments)
						}
					}
				}
			}
			require.Empty(t, streamed, "包装形状片段不得增量下发（客户端无法收回）")
			result, tail := tr.Finalize(false)
			require.Len(t, result.ToolCalls, 1)
			require.Equal(t, "Bash", result.ToolCalls[0].Name)
			require.Equal(t, tc.want, result.ToolCalls[0].Arguments)
			require.NotEmpty(t, tail, "扣住的字节必须在收尾补发")
			var forwarded string
			for _, d := range tail {
				for _, c := range d.ToolCalls {
					forwarded += c.Arguments
				}
			}
			require.Equal(t, tc.want, forwarded)
		})
	}
}

// 正常参数不被误扣：前缀不匹配立即转直通，行为与无检测时一致。
func TestIDETranslatorToolCallNormalArgumentsStreamThrough(t *testing.T) {
	tr := NewIDETranslator("m")
	frag1 := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1",
			"function_call": map[string]any{"name": "Bash", "arguments": "{\"command\":"},
		}},
	}})
	require.Len(t, frag1, 1)
	require.Equal(t, `{"command":`, frag1[0].ToolCalls[0].Arguments)
	frag2 := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index":         0,
			"function_call": map[string]any{"arguments": "\"ls\"}"},
		}},
	}})
	require.Equal(t, `"ls"}`, frag2[0].ToolCalls[0].Arguments)
	result, tail := tr.Finalize(false)
	require.Equal(t, `{"command":"ls"}`, result.ToolCalls[0].Arguments)
	require.Empty(t, tail, "直通调用收尾无尾增")
}

// 单键 arguments 但内容非 JSON 字符串/非对象：不是包装形状，原样保留。
func TestIDETranslatorToolCallLegitArgumentsKeyKept(t *testing.T) {
	tr := NewIDETranslator("m")
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_1",
			"function_call": map[string]any{"name": "Run", "arguments": `{"arguments":"just a plain string"}`},
		}},
	}})
	result, tail := tr.Finalize(false)
	require.Equal(t, `{"arguments":"just a plain string"}`, result.ToolCalls[0].Arguments)
	require.Len(t, tail, 1, "扣住的原始字节必须补发（内容不变）")
}

// 单帧完整调用（name+参数同帧）：首个转发片段必须携带 id/name——下游据此
// 宣布 tool_use 块，缺名会生成无名工具调用（客户端无法派发）。
func TestIDETranslatorSingleFrameNameAndArgsForwarded(t *testing.T) {
	tr := NewIDETranslator("m")
	deltas := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_9",
			"function_call": map[string]any{"name": "Bash", "arguments": `{"command":"ls"}`},
		}},
	}})
	require.Len(t, deltas, 1)
	call := deltas[0].ToolCalls[0]
	require.Equal(t, "call_9", call.ID)
	require.Equal(t, "Bash", call.Name)
	require.Equal(t, `{"command":"ls"}`, call.Arguments)
	result, tail := tr.Finalize(false)
	require.Equal(t, "Bash", result.ToolCalls[0].Name)
	require.Empty(t, tail)
}

// 单帧包装形状（name+被扣参数同帧）：收尾补发片段也必须携带 id/name。
func TestIDETranslatorWrappedSingleFrameTailCarriesName(t *testing.T) {
	tr := NewIDETranslator("m")
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_w",
			"function_call": map[string]any{"name": "Bash", "arguments": `{"arguments": "{\"command\":\"ls\"}"}`},
		}},
	}})
	_, tail := tr.Finalize(false)
	require.Len(t, tail, 1)
	call := tail[0].ToolCalls[0]
	require.Equal(t, "call_w", call.ID)
	require.Equal(t, "Bash", call.Name)
	require.Equal(t, `{"command":"ls"}`, call.Arguments)
}

// 空参退化守卫：有必填参数的工具收到完整 "{}" 参数 → 扣住不下发、
// ShouldAbortTurn 为真（上层可作废重打）；Finalize 不补发。
func TestIDETranslatorDegenerateEmptyArgsAborted(t *testing.T) {
	tr := NewDegenerateGuardIDETranslator("m", map[string]bool{"Bash": true})
	deltas := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_d",
			"function_call": map[string]any{"name": "Bash", "arguments": "{}"},
		}},
	}})
	require.Empty(t, deltas, "退化帧不得下发")
	require.True(t, tr.ShouldAbortTurn())
	result, tail := tr.Finalize(false)
	require.True(t, result.DegenerateToolCalls)
	require.Empty(t, tail)
	require.Len(t, result.ToolCalls, 1)
	require.Equal(t, "Bash", result.ToolCalls[0].Name)
}

// 已流出 thinking 的回合不作废：扣住的 {} 调用在收尾补发（内容完整交付）。
func TestIDETranslatorDegenerateFlushedWhenStreamed(t *testing.T) {
	tr := NewDegenerateGuardIDETranslator("m", map[string]bool{"Bash": true})
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{"reasoning_content": "thinking..."}})
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_d",
			"function_call": map[string]any{"name": "Bash", "arguments": "{}"},
		}},
	}})
	require.False(t, tr.ShouldAbortTurn())
	result, tail := tr.Finalize(false)
	require.False(t, result.DegenerateToolCalls)
	require.Len(t, tail, 1)
	call := tail[0].ToolCalls[0]
	require.Equal(t, "Bash", call.Name)
	require.Equal(t, "{}", call.Arguments)
	_ = result
}

// 守卫外（无必填参数的工具 / guard 关闭）：{} 照常透传。
func TestIDETranslatorEmptyArgsPassWithoutGuard(t *testing.T) {
	for _, guard := range []map[string]bool{nil, {"Write": true}} {
		tr := NewDegenerateGuardIDETranslator("m", guard)
		deltas := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_p",
				"function_call": map[string]any{"name": "NoParams", "arguments": "{}"},
			}},
		}})
		require.Len(t, deltas, 1)
		require.Equal(t, "{}", deltas[0].ToolCalls[0].Arguments)
		require.False(t, tr.ShouldAbortTurn())
	}
}

// 同 index 多调用串发（trae2api 4f9451e 实证的上游行为）：前一个调用参数
// 已闭合、新片段以 { 开头 → 拆新槽位继承工具名，不产生 {"a":1}{"b":2} 拼接。
func TestIDETranslatorIndexReuseSplitsCalls(t *testing.T) {
	tr := NewIDETranslator("m")
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_a",
			"function_call": map[string]any{"name": "Bash", "arguments": `{"command":"ls"}`},
		}},
	}})
	deltas := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index":         0,
			"function_call": map[string]any{"arguments": `{"command":"git status"}`},
		}},
	}})
	// 新片段必须以新槽位（index 1）下发且带继承名。
	require.Len(t, deltas, 1)
	call := deltas[0].ToolCalls[0]
	require.NotNil(t, call.Index)
	require.Equal(t, 1, *call.Index)
	require.Equal(t, "Bash", call.Name)
	require.Equal(t, `{"command":"git status"}`, call.Arguments)

	result, _ := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 2, "闭合后复用 index 的新片段是独立调用")
	require.Equal(t, `{"command":"ls"}`, result.ToolCalls[0].Arguments)
	require.Equal(t, `{"command":"git status"}`, result.ToolCalls[1].Arguments)
	require.Equal(t, "Bash", result.ToolCalls[1].Name)
}

// {} 头帧 + 同 index 真参数（上游复用痕迹的真身）：空头帧抑制、真调用交付。
func TestIDETranslatorEmptyHeadFramePlusRealCall(t *testing.T) {
	tr := NewDegenerateGuardIDETranslator("m", map[string]bool{"Bash": true})
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_h",
			"function_call": map[string]any{"name": "Bash", "arguments": "{}"},
		}},
	}})
	// 真调用片段在第二个事件即时流式下发（新槽位 + 继承名），不在收尾。
	deltas2 := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index":         0,
			"function_call": map[string]any{"arguments": `{"command":"ls -la"}`},
		}},
	}})
	require.False(t, tr.ShouldAbortTurn(), "已有真调用，不作废")
	tr.ProcessEvent(Event{Name: "done", Data: map[string]any{"finish_reason": "tool_calls"}})
	require.Len(t, deltas2, 1)
	call := deltas2[0].ToolCalls[0]
	require.NotNil(t, call.Index)
	require.Equal(t, 1, *call.Index)
	require.Equal(t, "Bash", call.Name)
	require.Equal(t, `{"command":"ls -la"}`, call.Arguments)
	result, tail := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 1, "{} 头帧不进结果")
	require.Equal(t, `{"command":"ls -la"}`, result.ToolCalls[0].Arguments)
	require.Equal(t, "Bash", result.ToolCalls[0].Name)
	require.Empty(t, tail)
}

// 孤立闭合帧（无 index/id/name、不以 { [ 开头）丢弃；无 index 的有序无名
// 片段并入最近命名调用。
func TestIDETranslatorOrphanAndOrderedFragments(t *testing.T) {
	tr := NewIDETranslator("m")
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_o",
			"function_call": map[string]any{"name": "Bash", "arguments": ""},
		}},
	}})
	// 无 index 无名续片 → 并入 call_o。
	tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"function_call": map[string]any{"arguments": `{"command":"pwd"}`},
		}},
	}})
	// 孤立闭合帧 → 丢弃。
	deltas := tr.ProcessEvent(Event{Name: "output", Data: map[string]any{
		"tool_calls": []any{map[string]any{
			"function_call": map[string]any{"arguments": `}"`},
		}},
	}})
	require.Empty(t, deltas)
	result, _ := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 1)
	require.Equal(t, `{"command":"pwd"}`, result.ToolCalls[0].Arguments)
}
