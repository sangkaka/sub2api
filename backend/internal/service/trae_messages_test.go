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
	isolateTraeChannelPrefs(t)

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
	isolateTraeChannelPrefs(t)

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
	// 走 1005→IDE 回落会写包级偏好缓存；隔离并恢复，避免污染同账号同模型
	// 的其它测试（包级状态按测试串行共享）。
	isolateTraeChannelPrefs(t)

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
	// 上游 FunctionCall 形状：function_call 键 + name 必须存在（OpenAI 的
	// function 键会致 2001 "required field Name is not set"）。
	for _, raw := range messages {
		item := raw.(map[string]any)
		if item["role"] != "assistant" {
			continue
		}
		for _, callRaw := range item["tool_calls"].([]any) {
			call := callRaw.(map[string]any)
			fn, ok := call["function_call"].(map[string]any)
			require.True(t, ok, "历史 tool_calls 必须用 function_call 键")
			require.Equal(t, "read_file", fn["name"])
			require.IsType(t, "", fn["arguments"])
		}
	}
}

// CC 生产事故回归（"Invalid tool parameters" 循环）：大参数工具调用被上游
// 按 index 分片增量下发，流式出口必须拼出完整 tool_use input，而非空/半截
// JSON（旧实现丢无 name 的片段，客户端参数校验必败）。
func TestForwardAnthropicViaTraeStreamFragmentedTools(t *testing.T) {
	var ideBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"list":[{"function":"solo_agent_remote","models":[
			{"config_name":"GLM-5.3","provider":"zhipu","config_source":1,"is_preset":true}
		]}]}}`)
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"chat_session_id":"s-1","message_id":"m-1"}}`)
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: error\ndata: {\"code\":1005}\n\n")
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/stop", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/api/agent/v3/llm_utils_chat", func(w http.ResponseWriter, r *http.Request) {
		require.NoError(t, json.NewDecoder(r.Body).Decode(&ideBody))
		w.Header().Set("Content-Type", "text/event-stream")
		frames := []string{
			`{"response":"writing","reasoning_content":null,"tool_calls":null}`,
			`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"id":"c1","type":"function","function_call":{"name":"write_file","arguments":""}}]}`,
			`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"function_call":{"arguments":"{\"path\":\"a"}}]}`,
			`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"function_call":{"arguments":".txt\",\"content\":\"hel"}}]}`,
			`{"response":"","reasoning_content":null,"tool_calls":[{"index":0,"function_call":{"arguments":"lo world\"}"}}]}`,
		}
		var sse strings.Builder
		for _, frame := range frames {
			fmt.Fprintf(&sse, "event: output\ndata: %s\n\n", frame)
		}
		sse.WriteString(`event: token_usage` + "\n" + `data: {"prompt_tokens":9,"completion_tokens":8,"total_tokens":17}` + "\n\n")
		sse.WriteString(`event: done` + "\n" + `data: {"finish_reason":"stop"}` + "\n\n")
		fmt.Fprint(w, sse.String())
	})
	upstream := startTestServer(t, mux)
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)
	svc.traeClient = &trae.Client{
		RemoteBase:   upstream.URL + "/api/remote/v1",
		CloudIDEHost: upstream.URL,
	}
	isolateTraeChannelPrefs(t)

	body := map[string]any{
		"model": "GLM-5.3", "max_tokens": 500, "stream": true,
		"tools": []map[string]any{{
			"name": "write_file", "description": "write",
			"input_schema": map[string]any{"type": "object", "properties": map[string]any{
				"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"},
			}},
		}},
		"messages": []map[string]any{{"role": "user", "content": "写 a.txt"}},
	}
	encoded, _ := json.Marshal(body)
	c, rec := traeTestContext()
	_, err := svc.forwardAnthropicViaTrae(context.Background(), c, newTraeTestAccount(), encoded, "GLM-5.3")
	require.NoError(t, err)

	// 解析 Anthropic SSE：tool_use 块名 + 增量拼接后的 input JSON。
	out := rec.Body.String()
	var toolName, inputJSON string
	inputDeltaEvents := 0
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var evt struct {
			Type  string `json:"type"`
			ContentBlock *struct {
				Type  string `json:"type"`
				Name  string `json:"name"`
				ID    string `json:"id"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &evt) != nil {
			continue
		}
		if evt.Type == "content_block_start" && evt.ContentBlock != nil && evt.ContentBlock.Type == "tool_use" {
			toolName = evt.ContentBlock.Name
		}
		if evt.Type == "content_block_delta" && evt.Delta != nil && evt.Delta.Type == "input_json_delta" {
			inputJSON += evt.Delta.PartialJSON
			inputDeltaEvents++
		}
	}
	require.Equal(t, "write_file", toolName, "必须出现工具块宣告")
	require.NotEmpty(t, inputJSON, "必须下发工具参数")
	require.JSONEq(t, `{"path":"a.txt","content":"hello world"}`, inputJSON, "分片参数必须拼成完整 JSON")
	// 片段必须增量到达（三个参数片段 → 多次 input_json_delta），而不是
	// 收尾一次性下发——纯工具长回合静默到收尾会被客户端空闲超时掐流。
	require.GreaterOrEqual(t, inputDeltaEvents, 3, "参数片段必须随流增量下发")
	require.Contains(t, out, `"stop_reason":"tool_use"`)
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
