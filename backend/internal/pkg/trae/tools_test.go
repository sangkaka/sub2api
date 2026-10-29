package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// 文本桥扫描器：标记跨增量分段、多调用、未闭合按原文透出。
func TestToolCallScannerSplitsMarkers(t *testing.T) {
	s := &ToolCallScanner{}
	visible1, calls := s.Feed("Here is the plan. <openc")
	require.Equal(t, "Here is the plan. ", visible1)
	require.Empty(t, calls) // 半截标记被缓冲

	visible2, calls := s.Feed(`ode_tool_call>{"name":"read_file","input":{"path":"a.go"}}</opencode_tool_call> Done.`)
	require.Equal(t, " Done.", visible2)
	require.Len(t, calls, 1)
	require.Equal(t, "read_file", calls[0].Name)
	require.Equal(t, `{"path":"a.go"}`, calls[0].Arguments)
	require.NotEmpty(t, calls[0].ID)
	require.Equal(t, calls, s.Calls())
}

func TestToolCallScannerMultipleCalls(t *testing.T) {
	s := &ToolCallScanner{}
	_, calls := s.Feed(`<opencode_tool_call>{"name":"a","input":{}}</opencode_tool_call><opencode_tool_call>{"name":"b","input":{"x":1}}</opencode_tool_call>`)
	require.Len(t, calls, 2)
	require.Equal(t, "a", calls[0].Name)
	require.Equal(t, "b", calls[1].Name)
	require.NotEqual(t, calls[0].ID, calls[1].ID)
}

// 未闭合标记（模型截断）：Flush 按原文透出。
func TestToolCallScannerIncompleteMarkerFlushes(t *testing.T) {
	s := &ToolCallScanner{}
	visible, _ := s.Feed(`Answer <opencode_tool_call>{"name":"x"`)
	// 标记开头已完整出现：其前的文本立即透出，未闭合部分缓冲。
	require.Equal(t, "Answer ", visible)
	require.Equal(t, `<opencode_tool_call>{"name":"x"`, s.Flush())
}

// 非法 JSON 的标记整段丢弃（不影响后续文本）。
func TestToolCallScannerInvalidJSONIgnored(t *testing.T) {
	s := &ToolCallScanner{}
	visible, calls := s.Feed(`<opencode_tool_call>not-json</opencode_tool_call>tail`)
	require.Equal(t, "tail", visible)
	require.Empty(t, calls)
}

// 无标记文本全量透出，且标记前缀检查不影响普通文本。
func TestToolCallScannerPlainPassThrough(t *testing.T) {
	s := &ToolCallScanner{}
	visible, calls := s.Feed("plain < text and <open brackets")
	// 仅缓冲「尾部的标记前缀」；中部的 "<open" 后随已判定的非标记文本，
	// 不必缓冲，整段透出。
	require.Equal(t, "plain < text and <open brackets", visible)
	require.Empty(t, calls)
	require.Empty(t, s.pending)
}

func TestBuildToolRuntimePrompt(t *testing.T) {
	prompt := BuildToolRuntimePrompt([]ToolDefinition{
		{Type: "function", Function: json.RawMessage(`{"name":"get_weather","description":"w","parameters":{"type":"object"}}`)},
	}, "auto", nil)
	require.Contains(t, prompt, "opencode_tool_call")
	require.Contains(t, prompt, `"name":"get_weather"`)
	require.NotContains(t, prompt, "Tool choice is none")

	none := BuildToolRuntimePrompt([]ToolDefinition{{Function: json.RawMessage(`{"name":"x"}`)}}, "none", boolPtr(false))
	require.Contains(t, none, "Tool choice is none")
	require.Contains(t, none, "at most one tool per turn")

	require.Empty(t, BuildToolRuntimePrompt(nil, "", nil))
}

func boolPtr(v bool) *bool { return &v }

func TestSerializeAssistantToolCalls(t *testing.T) {
	out := SerializeAssistantToolCalls(json.RawMessage(`[{"id":"c1","function":{"name":"search","arguments":"{\"q\":\"x\"}"}}]`))
	require.Contains(t, out, "do not repeat")
	require.Contains(t, out, "search")
	require.Contains(t, out, "c1")
	require.Empty(t, SerializeAssistantToolCalls(nil))
}

// 图片 part 三种来源：OpenAI data URL / OpenAI 远端 URL / Anthropic base64。
func TestMediaQueryParts(t *testing.T) {
	parts := MediaQueryParts([]ChatMessage{
		{Role: "user", Content: json.RawMessage(`[
			{"type":"text","text":"look"},
			{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,aGVsbG8="}},
			{"type":"image_url","image_url":{"url":"https://cdn.example.com/cat.png"}},
			{"type":"image","source":{"type":"base64","media_type":"image/webp","data":"d2Vi cA=="}},
			{"type":"audio","input_audio":{"data":"x"}}
		]`)},
	})
	// 每张图片 = 1 个 media part + 1 个描述文本 part；音频不处理。
	require.Len(t, parts, 6)
	first := parts[0]
	require.Equal(t, "image_url", first["type"])
	data := first["data"].(map[string]any)
	require.Equal(t, "image/jpeg", data["mimeType"])
	require.Equal(t, "aGVsbG8=", data["data"])
	remote := parts[2]["data"].(map[string]any)
	require.Equal(t, "url", remote["mimeType"])
	require.Equal(t, "https://cdn.example.com/cat.png", remote["data"])
	anthropic := parts[4]["data"].(map[string]any)
	require.Equal(t, "image/webp", anthropic["mimeType"])
	// base64 数据去空白。
	require.Equal(t, "d2VicA==", anthropic["data"]) // base64 数据去空白
	require.Empty(t, MediaQueryParts([]ChatMessage{{Role: "user", Content: json.RawMessage(`"text only"`)}}))
}

// 1M Max：仅 descriptor 显式声明且三项上限齐备时可用。
func TestMaxModeDescriptor(t *testing.T) {
	unsupported := &ModelDescriptor{Raw: map[string]any{"config_name": "m"}}
	require.False(t, unsupported.SupportsMaxMode())
	require.Nil(t, unsupported.MaxModeFields())

	noLimits := &ModelDescriptor{Raw: map[string]any{"max_mode": true}}
	require.False(t, noLimits.SupportsMaxMode()) // 缺上限不可合成

	full := &ModelDescriptor{Raw: map[string]any{
		"max_mode":            true,
		"context_window_size": 1048576, "prompt_max_tokens": 936000, "max_tokens": 65536,
	}}
	require.True(t, full.SupportsMaxMode())
	fields := full.MaxModeFields()
	require.Equal(t, "max", fields["model_selection_strategy"])
	require.Equal(t, 1048576, fields["context_window_size"])
	require.Equal(t, 1, fields["mode_type"])
}

// 签到设备 id：同账号稳定、随代数变化；9074 映射 RateLimited。
func TestCheckinDeviceID(t *testing.T) {
	base := CheckinDeviceID("user-1", 0)
	require.Len(t, base, 16)
	require.Equal(t, base, CheckinDeviceID("user-1", 0))
	next := CheckinDeviceID("user-1", 1)
	require.Len(t, next, 16)
	require.NotEqual(t, base, next)
	require.Empty(t, CheckinDeviceID("", 0))
}

func TestClaimCheckin9074(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.NotEmpty(t, r.Header.Get("x-device-id"))
		require.Equal(t, "Cloud-IDE-JWT jwt", r.Header.Get("Authorization"))
		if r.URL.Path == checkinClaimPath {
			fmt.Fprint(w, `{"code":9074,"message":"too many users, retry later"}`)
			return
		}
		fmt.Fprint(w, `{"enable":true,"checked_in":false,"credits":0}`)
	}))
	defer server.Close()

	client := &Client{APIHost: server.URL}
	result, err := client.ClaimCheckin(context.Background(), "jwt", "user-1", 0)
	require.NoError(t, err)
	require.True(t, result.RateLimited)
	require.False(t, result.Claimed)

	status, err := client.CheckinStatus(context.Background(), "jwt", "user-1", 0)
	require.NoError(t, err)
	require.True(t, status.Enabled)
	require.False(t, status.CheckedIn)
}

// 会话线路上 max 字段与媒体 parts 的合并。
func TestCreateSessionMaxModeAndMedia(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewDecoder(r.Body).Decode(&gotBody)
		fmt.Fprint(w, `{"data":{"chat_session_id":"s","message_id":"m"}}`)
	}))
	defer server.Close()

	client := &Client{RemoteBase: server.URL}
	descriptor := &ModelDescriptor{ConfigName: "GLM-5.3", Raw: map[string]any{
		"config_name": "GLM-5.3", "provider": "zhipu", "config_source": 1,
		"max_mode": true, "context_window_size": 1048576, "prompt_max_tokens": 936000, "max_tokens": 65536,
	}}
	_, err := client.CreateSession(context.Background(), "jwt", SessionRequest{
		Query:           FlattenMessages([]ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}),
		ExtraQueryParts: []map[string]any{{"type": "image_url", "data": map[string]any{"mimeType": "image/png", "data": "abc"}}},
		Model:           "GLM-5.3",
		Descriptor:      descriptor,
		MaxModeFields:   descriptor.MaxModeFields(),
	})
	require.NoError(t, err)
	initial := gotBody["initial_message"].(map[string]any)
	require.Equal(t, "max", initial["model_selection_strategy"])
	require.Equal(t, float64(1048576), initial["context_window_size"])
	require.Equal(t, float64(1), initial["mode_type"])
	var queryParts []map[string]any
	require.NoError(t, json.Unmarshal([]byte(initial["query"].(string)), &queryParts))
	require.Len(t, queryParts, 2) // 文本 part + 图片 part
	require.Equal(t, "image_url", queryParts[1]["type"])
}

// 翻译器端到端：正文中的工具标记转为 ToolCalls 增量，finish_reason 语义由
// runtime 决定；此处验证 Delta/结果聚合。
func TestStreamTranslatorToolBridge(t *testing.T) {
	tr := NewStreamTranslator("m")
	deltas := tr.ProcessEvent(Event{Name: EventPlanItem, Data: map[string]any{
		"id": "p1", "content": `Checking <opencode_tool_call>{"name":"read","input":{"p":1}}</opencode_tool_call>`,
	}})
	var text, calls string
	var toolCalls []ToolCall
	for _, d := range deltas {
		text += d.Content
		toolCalls = append(toolCalls, d.ToolCalls...)
		if len(d.ToolCalls) > 0 {
			calls = "seen"
		}
	}
	require.Equal(t, "Checking ", text)
	require.Equal(t, "seen", calls)
	require.Len(t, toolCalls, 1)
	require.Equal(t, "read", toolCalls[0].Name)

	result, _ := tr.Finalize(false)
	require.Len(t, result.ToolCalls, 1)
	require.Equal(t, "Checking ", result.Content)
}

// assistant 历史带 tool_calls 时序列化进 [Assistant] 段。
func TestFlattenMessagesAssistantToolHistory(t *testing.T) {
	query := FlattenMessages([]ChatMessage{
		{Role: "assistant", Content: json.RawMessage(`""`), ToolCalls: json.RawMessage(`[{"id":"c1","function":{"name":"search","arguments":"{}"}}]`)},
	})
	require.Contains(t, query, "[Assistant]")
	require.Contains(t, query, "do not repeat")
	require.Contains(t, query, "search") // 内层 JSON 键序/转义不锁定
}
