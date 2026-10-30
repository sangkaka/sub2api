//go:build unit

package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
)

func anthropicRequestBody(stream bool) []byte {
	body := map[string]any{
		"model":      "GLM-5.3",
		"max_tokens": 500,
		"system":     "be concise",
		"stream":     stream,
		"messages": []map[string]any{
			{"role": "user", "content": []map[string]any{{"type": "text", "text": "hi"}}},
		},
	}
	out, _ := json.Marshal(body)
	return out
}

// 非流式：Anthropic 请求 → trae runtime → Anthropic Messages 响应。
func TestForwardAnthropicViaTraeBuffered(t *testing.T) {
	upstream := newTraeTestUpstream(t, traeStreamEvents())
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	c, rec := traeTestContext()
	c.Request.Header.Set("Content-Type", "application/json")
	account := newTraeTestAccount()

	result, err := svc.forwardAnthropicViaTrae(context.Background(), c, account, anthropicRequestBody(false), "GLM-5.3")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.False(t, result.Stream)

	var resp apicompat.AnthropicResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.Equal(t, "GLM-5.3", resp.Model)
	require.Equal(t, "hello world", extractAnthropicText(t, rec.Body.String()))
	require.NotContains(t, rec.Body.String(), "chat.completion", "不得泄漏 OpenAI 形响应")
	require.NotContains(t, rec.Body.String(), "\"object\":\"chat.completion\"")
}

// 流式：Anthropic 请求 → OpenAI SSE 增量 → Anthropic 事件流
// （message_start / content_block_delta / message_stop）。
func TestForwardAnthropicViaTraeStream(t *testing.T) {
	upstream := newTraeTestUpstream(t, traeStreamEvents())
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	c, rec := traeTestContext()
	c.Request.Header.Set("Content-Type", "application/json")
	account := newTraeTestAccount()

	result, err := svc.forwardAnthropicViaTrae(context.Background(), c, account, anthropicRequestBody(true), "GLM-5.3")
	require.NoError(t, err)
	require.True(t, result.Stream)

	out := rec.Body.String()
	for _, want := range []string{
		"event: message_start",
		"event: content_block_delta",
		"event: content_block_stop",
		"event: message_delta",
		"event: message_stop",
	} {
		require.Contains(t, out, want, "缺少 Anthropic 事件 "+want)
	}
	require.Contains(t, out, "\"text\":\"hel\"")
	require.Contains(t, out, "\"text\":\"lo world\"")
	require.Contains(t, out, "thinking_delta")
	require.NotContains(t, out, "chat.completion.chunk", "不得泄漏 OpenAI 流形状")
	require.NotContains(t, out, "[DONE]", "Anthropic 流不用 [DONE] 终止符")
}

// 上游错误（模型未绑定）在被拦截后由上层以 Anthropic 形写错误；
// 拦截 writer 不得把 OpenAI 形错误直接写给客户端。
func TestForwardAnthropicViaTraeErrorShape(t *testing.T) {
	upstream := newTraeTestUpstream(t, `event: error`+"\n"+`data: {"code":4028,"message":"model not bound"}`+"\n\n")
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	c, rec := traeTestContext()
	account := newTraeTestAccount()

	_, err := svc.forwardAnthropicViaTrae(context.Background(), c, account, anthropicRequestBody(false), "GLM-5.3")
	require.Error(t, err)
	require.NotContains(t, rec.Body.String(), "chat.completion")
	require.NotContains(t, rec.Body.String(), `"object":"chat.completion"`)
}

func extractAnthropicText(t *testing.T, body string) string {
	t.Helper()
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	var parts []string
	for _, block := range resp.Content {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "")
}

// CC 多轮工具会话回归：assistant 历史 tool_use 与 user tool_result 必须以
// 配对的 tool_calls/tool_call_id 到达 IDE 上游（丢 tool_calls 曾致 4027
// "tool_call_id is not found"，生产 CC 实测）。
func TestForwardAnthropicViaTraeToolHistoryWire(t *testing.T) {
	var ideBody map[string]any
	mux := setupToolHistoryUpstream(t, &ideBody)
	upstream := startTestServer(t, mux)
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)
	svc.traeClient = &trae.Client{
		RemoteBase:   upstream.URL + "/api/remote/v1",
		CloudIDEHost: upstream.URL,
	}

	body := map[string]any{
		"model": "GLM-5.3", "max_tokens": 500,
		"tools": []map[string]any{{
			"name": "read_file", "description": "read",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}}},
		}},
		"messages": []map[string]any{
			{"role": "user", "content": "读 README"},
			{"role": "assistant", "content": []map[string]any{
				{"type": "thinking", "thinking": "需要读文件", "signature": "sig"},
				{"type": "tool_use", "id": "toolu_01", "name": "read_file", "input": map[string]any{"path": "README.md"}},
			}},
			{"role": "user", "content": []map[string]any{
				{"type": "tool_result", "tool_use_id": "toolu_01", "content": "文件内容"},
			}},
		},
	}
	encoded, _ := json.Marshal(body)
	c, _ := traeTestContext()
	_, err := svc.forwardAnthropicViaTrae(context.Background(), c, newTraeTestAccount(), encoded, "GLM-5.3")
	require.NoError(t, err)

	messages := ideBody["messages"].([]any)
	var assistantCalls, toolIDs []string
	for _, raw := range messages {
		item := raw.(map[string]any)
		if item["role"] == "assistant" {
			if calls, ok := item["tool_calls"].([]any); ok {
				for _, callRaw := range calls {
					call := callRaw.(map[string]any)
					assistantCalls = append(assistantCalls, fmt.Sprintf("%v", call["id"]))
				}
			}
		}
		if item["role"] == "tool" {
			toolIDs = append(toolIDs, fmt.Sprintf("%v", item["tool_call_id"]))
		}
	}
	require.NotEmpty(t, assistantCalls, "assistant 历史 tool_calls 必须到达上游")
	require.Equal(t, []string{"toolu_01"}, toolIDs)
	require.Equal(t, toolIDs, assistantCalls, "tool 结果的 tool_call_id 必须与 assistant tool_calls 配对")
}

func setupToolHistoryUpstream(t *testing.T, ideBody *map[string]any) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"list":[{"function":"solo_agent_remote","models":[
			{"config_name":"GLM-5.3","provider":"zhipu","config_source":1,"is_preset":true}
		]}]}}`)
	})
	// Remote 对该模型直接 1005，强制走 IDE 通道（生产同款路径）。
	mux.HandleFunc("/api/remote/v1/chat_sessions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"chat_session_id":"s-1","message_id":"m-1"}}`)
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: error\ndata: {\"code\":1005}\n\n")
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/stop", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/api/agent/v3/llm_utils_chat", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(ideBody))
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: output` + "\n" + `data: {"response":"done","reasoning_content":null,"tool_calls":null}` + "\n\n",
			`event: token_usage` + "\n" + `data: {"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}` + "\n\n",
			`event: done` + "\n" + `data: {"finish_reason":"stop"}` + "\n\n",
		}, ""))
	})
	return mux
}

func startTestServer(t *testing.T, mux *http.ServeMux) *httptest.Server {
	t.Helper()
	return httptest.NewServer(mux)
}
