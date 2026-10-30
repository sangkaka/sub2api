package trae

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// v2 body 契约（2026-10-30 实测）：五字段 + messages/tools SOLO 字符串化参数；
// 不带 tool_choice / max_tokens。
func TestBuildRawChatV2Body(t *testing.T) {
	tools := []ToolDefinition{{
		Type:     "function",
		Function: json.RawMessage(`{"name":"Echo","description":"echo","parameters":{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}}`),
	}}
	raw, err := buildRawChatV2Body(RawChatV2Request{
		Messages:     []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		ConfigName:   "glm-5.2",
		ModelName:    "glm-5.2__dev",
		ConfigSource: 1,
		Tools:        tools,
	})
	require.NoError(t, err)
	var body map[string]any
	require.NoError(t, json.Unmarshal(raw, &body))
	require.Equal(t, "glm-5.2", body["config_name"])
	require.Equal(t, "glm-5.2__dev", body["model_name"])
	require.Equal(t, "chat_v3", body["raw_chat_function"])
	require.Equal(t, float64(1), body["config_source"])
	require.Equal(t, true, body["stream"])
	require.NotEmpty(t, body["conversation_id"])
	require.Equal(t, body["conversation_id"], body["session_id"])
	require.NotContains(t, body, "tool_choice", "v2 带 tool_choice 实测 400")
	require.NotContains(t, body, "max_tokens", "v2 不下发 max_tokens")
	toolsArr, ok := body["tools"].([]any)
	require.True(t, ok)
	fn := toolsArr[0].(map[string]any)["function"].(map[string]any)
	params, ok := fn["parameters"].(string)
	require.True(t, ok, "parameters 必须字符串化（对象形 400）")
	require.Contains(t, params, `"text"`)
}

func TestPickV2DetailModel(t *testing.T) {
	require.Equal(t, "glm-5__dev", pickV2DetailModel([]any{
		map[string]any{"model_name": "glm-5__max"},
		map[string]any{"model_name": "glm-5__dev"},
	}))
	require.Equal(t, "first", pickV2DetailModel([]any{
		map[string]any{"model_name": "first"},
		map[string]any{},
	}))
	require.Equal(t, "", pickV2DetailModel(nil))
}

// 目录解析：config_info_list 顶层与 data 嵌套两种壳、__dev 优先。
func TestFetchV2ModelBindings(t *testing.T) {
	catalog := `{"config_info_list":[
		{"config_name":"glm-5.2","config_source":1,"model_detail_list":[{"model_name":"glm-5.2__dev"},{"model_name":"glm-5.2__max"}]},
		{"config_name":"no-detail","config_source":1}
	]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/ide/v1/get_detail_param", r.URL.Path)
		require.Equal(t, "chat_v3", r.Header.Get("X-App-Function"))
		require.Equal(t, "chat_v3", r.Header.Get("X-Ide-Function"))
		require.Equal(t, "desktop", r.Header.Get("x-plugin-channel"))
		_, _ = w.Write([]byte(catalog))
	}))
	defer srv.Close()
	client := &Client{CloudIDEHost: srv.URL}
	bindings, err := client.FetchV2ModelBindings(t.Context(), "tok", "u1")
	require.NoError(t, err)
	binding, ok := bindings["glm-5.2"]
	require.True(t, ok)
	require.Equal(t, "glm-5.2__dev", binding.ModelName)
	require.Equal(t, 1, binding.ConfigSource)
	_, ok = bindings["no-detail"]
	require.False(t, ok, "无 detail 的 config 不产出绑定")
}

// v2 请求头：函数头/Ide-Token/版本族叠加。
func TestRawChatV2HeadersAndStream(t *testing.T) {
	var gotPath, gotFn string
	sse := strings.Join([]string{
		`event: output` + "\n" + `data: {"response":"pong","reasoning_content":null,"tool_calls":null}` + "\n\n",
		`event: token_usage` + "\n" + `data: {"prompt_tokens":3,"completion_tokens":2,"total_tokens":5}` + "\n\n",
		`event: done` + "\n" + `data: {"finish_reason":"stop"}` + "\n\n",
	}, "")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFn = r.Header.Get("X-App-Function")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()
	client := &Client{CloudIDEHost: srv.URL}
	events, errs := client.RawChatV2(t.Context(), "tok", RawChatV2Request{
		Messages:   []ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		ConfigName: "glm-5.2", ModelName: "glm-5.2__dev", ConfigSource: 1,
	})
	var sawOutput bool
	for ev := range events {
		if ev.Name == EventOutput {
			sawOutput = true
		}
	}
	require.NoError(t, <-errs)
	require.Equal(t, "/api/ide/v2/llm_raw_chat", gotPath)
	require.Equal(t, "chat_v3", gotFn)
	require.True(t, sawOutput)
}
