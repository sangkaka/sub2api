package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
