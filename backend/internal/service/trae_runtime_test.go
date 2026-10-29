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
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/trae"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// newTraeTestUpstream 起一个伪 TRAE 上游：/models、/chat_sessions、
// /chat_sessions/{id}/events（SSE 快照流）、/stop。
func newTraeTestUpstream(t *testing.T, events string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/remote/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"list":[{"function":"solo_agent_remote","models":[
			{"config_name":"GLM-5.3","provider":"zhipu","config_source":1,"is_preset":true}
		]}]}}`)
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Cloud-IDE-JWT jwt-token" {
			w.WriteHeader(http.StatusUnauthorized)
			fmt.Fprint(w, `{"error":{"code":1001,"message":"token expired"}}`)
			return
		}
		fmt.Fprint(w, `{"data":{"chat_session_id":"s-1","message_id":"m-1"}}`)
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/events", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, events)
		w.(http.Flusher).Flush()
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/stop", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

func traeStreamEvents() string {
	return strings.Join([]string{
		"event: heartbeat\n\n",
		`event: plan_item` + "\n" + `data: {"id":"p1","reasoning_content":"think"}` + "\n\n",
		`event: plan_item` + "\n" + `data: {"id":"p1","reasoning_content":"thinking","content":"hel"}` + "\n\n",
		`event: plan_item` + "\n" + `data: {"id":"p1","reasoning_content":"thinking","content":"hello world"}` + "\n\n",
		`data: {"event":"token_usage","usage":{"prompt_tokens":5,"output_tokens":3}}` + "\n\n",
		"event: done\ndata: [DONE]\n\n",
	}, "")
}

func newTraeTestAccount() *Account {
	return &Account{
		ID:       40,
		Platform: PlatformTrae,
		Type:     AccountTypeOAuth,
		Credentials: map[string]any{
			"token":             "jwt-token",
			"refresh_token":     "rt-1",
			"user_id":           "u-1",
			"client_id":         trae.PublicClientID,
			"expires_at":        time.Now().Add(2 * time.Hour).Format(time.RFC3339),
			"provider_specific": map[string]any{"webId": "w1", "bizUserId": "b1"},
		},
	}
}

func newTraeTestGateway(t *testing.T, upstream *httptest.Server) *OpenAIGatewayService {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	provider := NewTraeTokenProvider(nil, nil, nil)
	svc.SetTraeDeps(provider, nil, &trae.Client{RemoteBase: upstream.URL + "/api/remote/v1"})
	return svc
}

func traeTestContext() (*gin.Context, *httptest.ResponseRecorder) {
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	return c, rec
}

// 流式端到端：快照差分 → OpenAI 增量、心跳注释透传、usage 完成帧、[DONE]。
func TestForwardTraeChatCompletionsStream(t *testing.T) {
	upstream := newTraeTestUpstream(t, traeStreamEvents())
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	body, _ := json.Marshal(map[string]any{
		"model":    "GLM-5.3",
		"stream":   true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	c, rec := traeTestContext()
	account := newTraeTestAccount()

	result, err := svc.forwardTraeChatCompletions(context.Background(), c, account, body, "GLM-5.3")
	require.NoError(t, err)
	require.NotNil(t, result)
	require.True(t, result.Stream)
	require.Equal(t, 5, result.Usage.InputTokens)
	require.Equal(t, 3, result.Usage.OutputTokens)

	out := rec.Body.String()
	require.Contains(t, out, ": keepalive")
	require.Contains(t, out, `"reasoning_content":"think"`)
	require.Contains(t, out, `"reasoning_content":"ing"`)
	require.Contains(t, out, `"content":"hel"`)
	require.Contains(t, out, `"content":"lo world"`)
	require.Contains(t, out, `"prompt_tokens":5`)
	require.Contains(t, out, "data: [DONE]")
	require.NotContains(t, rec.Body.String(), `"content":"hello world"`, "must not replay full snapshots")
}

// 非流式端到端：单条 completion + reasoning_content + usage。
func TestForwardTraeChatCompletionsBuffered(t *testing.T) {
	upstream := newTraeTestUpstream(t, traeStreamEvents())
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	body, _ := json.Marshal(map[string]any{
		"model":    "GLM-5.3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	c, rec := traeTestContext()
	account := newTraeTestAccount()

	result, err := svc.forwardTraeChatCompletions(context.Background(), c, account, body, "GLM-5.3")
	require.NoError(t, err)
	require.False(t, result.Stream)
	require.Equal(t, 8, result.Usage.InputTokens+result.Usage.OutputTokens)

	var payload struct {
		Choices []struct {
			Message struct {
				Content          string `json:"content"`
				ReasoningContent string `json:"reasoning_content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &payload))
	require.Len(t, payload.Choices, 1)
	require.Equal(t, "hello world", payload.Choices[0].Message.Content)
	require.Equal(t, "thinking", payload.Choices[0].Message.ReasoningContent)
	require.Equal(t, "stop", payload.Choices[0].FinishReason)
}

// 首模型事件前的限流：写账号冷却且错误向上返回（上层可换号）。
func TestForwardTraeChatCompletionsRateLimit(t *testing.T) {
	events := strings.Join([]string{
		`data: {"event":"error","code":4011,"message":"parallel limit"}` + "\n\n",
	}, "")
	upstream := newTraeTestUpstream(t, events)
	defer upstream.Close()

	repo := &traeRateLimitRepo{mockAccountRepoForGemini: &mockAccountRepoForGemini{}}
	gin.SetMode(gin.TestMode)
	svc := &OpenAIGatewayService{}
	svc.SetTraeDeps(NewTraeTokenProvider(nil, nil, nil), nil, &trae.Client{RemoteBase: upstream.URL + "/api/remote/v1"})

	body, _ := json.Marshal(map[string]any{
		"model":    "GLM-5.3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	c, _ := traeTestContext()
	account := newTraeTestAccount()
	setAccountRepoForTrae(t, svc, repo)

	_, err := svc.forwardTraeChatCompletions(context.Background(), c, account, body, "GLM-5.3")
	require.Error(t, err)
	require.True(t, repo.rateLimited, "4011 must mark the account rate limited")
}

// 未绑定模型：明确报错，禁止上游静默换模型。
func TestForwardTraeChatCompletionsUnboundModel(t *testing.T) {
	upstream := newTraeTestUpstream(t, traeStreamEvents())
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	body, _ := json.Marshal(map[string]any{
		"model":    "kimi-k2",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	c, rec := traeTestContext()
	account := newTraeTestAccount()

	_, err := svc.forwardTraeChatCompletions(context.Background(), c, account, body, "")
	require.Error(t, err)
	require.Contains(t, rec.Body.String(), "not available for this account")
}

// 探活端到端：GetUserInfo + entitlement 槽位捕获（写回账号 concurrency）+
// 最小会话探活事件。
func TestTestTraeAccountConnection(t *testing.T) {
	gin.SetMode(gin.TestMode)
	apiMux := http.NewServeMux()
	apiMux.HandleFunc("/cloudide/api/v3/trae/GetUserInfo", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"Result":{"UserID":"u-1","ScreenName":"trae-user"}}`)
	})
	apiMux.HandleFunc("/trae/api/v2/pay/ide_user_ent_usage", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"quota":{"solo_agent_parallel_limit":2}}}`)
	})
	apiServer := httptest.NewServer(apiMux)
	defer apiServer.Close()

	remoteMux := http.NewServeMux()
	remoteMux.HandleFunc("/api/remote/v1/models", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"list":[{"function":"solo_agent_remote","models":[{"config_name":"GLM-5.3"}]}]}}`)
	})
	remoteMux.HandleFunc("/api/remote/v1/chat_sessions", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":{"chat_session_id":"s-1","message_id":"m-1"}}`)
	})
	remoteMux.HandleFunc("/api/remote/v1/chat_sessions/s-1/events", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "event: done\ndata: [DONE]\n\n")
	})
	remoteMux.HandleFunc("/api/remote/v1/chat_sessions/s-1/stop", func(w http.ResponseWriter, r *http.Request) {})
	remoteServer := httptest.NewServer(remoteMux)
	defer remoteServer.Close()

	repo := &traeRateLimitRepo{mockAccountRepoForGemini: &mockAccountRepoForGemini{}}
	svc := &AccountTestService{accountRepo: repo}
	svc.SetTraeTokenProvider(NewTraeTokenProvider(nil, nil, nil), nil, &trae.Client{
		APIHost:    apiServer.URL,
		RemoteBase: remoteServer.URL + "/api/remote/v1",
	})

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/accounts/40/test", nil)

	account := newTraeTestAccount()
	require.NoError(t, svc.testTraeAccountConnection(c, account, "GLM-5.3"))
	require.NotNil(t, repo.updated, "concurrency must be persisted from entitlement")
	require.Equal(t, 2, repo.updated.Concurrency)
	require.Contains(t, rec.Body.String(), "test_complete")
}

type traeRateLimitRepo struct {
	*mockAccountRepoForGemini
	rateLimited bool
	lastResetAt time.Time
	updated     *Account
}

func (r *traeRateLimitRepo) SetRateLimited(_ context.Context, _ int64, resetAt time.Time) error {
	r.rateLimited = true
	r.lastResetAt = resetAt
	return nil
}

func (r *traeRateLimitRepo) Update(_ context.Context, account *Account) error {
	r.updated = account
	return nil
}

func setAccountRepoForTrae(t *testing.T, svc *OpenAIGatewayService, repo *traeRateLimitRepo) {
	t.Helper()
	svc.accountRepo = repo
}

// 工具桥端到端（PR-3.1）：正文里的 <opencode_tool_call> 标记转 delta.tool_calls，
// finish_reason=tool_calls，标记文本不出现在可见正文。
func TestForwardTraeChatCompletionsToolBridge(t *testing.T) {
	events := strings.Join([]string{
		`event: plan_item` + "\n" + `data: {"id":"p1","content":"Let me check <opencode_tool_call>{\"name\":\"read_file\",\"input\":{\"path\":\"main.go\"}}</opencode_tool_call>"}` + "\n\n",
		`data: {"event":"token_usage","usage":{"prompt_tokens":9,"output_tokens":2}}` + "\n\n",
		"event: done\ndata: [DONE]\n\n",
	}, "")
	upstream := newTraeTestUpstream(t, events)
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)

	body, _ := json.Marshal(map[string]any{
		"model":  "GLM-5.3",
		"stream": true,
		"tools": []map[string]any{{
			"type":     "function",
			"function": map[string]any{"name": "read_file", "parameters": map[string]any{"type": "object"}},
		}},
		"messages": []map[string]any{{"role": "user", "content": "read main.go"}},
	})
	c, rec := traeTestContext()
	account := newTraeTestAccount()

	result, err := svc.forwardTraeChatCompletions(context.Background(), c, account, body, "GLM-5.3")
	require.NoError(t, err)
	require.Equal(t, 9, result.Usage.InputTokens)

	out := rec.Body.String()
	require.Contains(t, out, `"tool_calls"`)
	require.Contains(t, out, `"name":"read_file"`)
	require.Contains(t, out, `"finish_reason":"tool_calls"`)
	require.NotContains(t, out, "opencode_tool_call")
	require.Contains(t, out, "Let me check ")
}

// Remote 对某模型固定 1005（kimi-k3.1-pro 实测）：回合先在 Remote 收到
// error 事件，未写任何客户端字节即回落 llm_utils_chat；偏好缓存让第二笔
// 请求跳过 Remote 直连 IDE。
func TestForwardTraeChatCompletionsIDEFallback(t *testing.T) {
	var ideHits, remoteEventHits int
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
		remoteEventHits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: heartbeat\n\n"+`event: error`+"\n"+`data: {"code":1005,"message":""}`+"\n\n")
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/stop", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/api/agent/v3/llm_utils_chat", func(w http.ResponseWriter, r *http.Request) {
		ideHits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: output` + "\n" + `data: {"response":"hello ide","reasoning_content":null,"tool_calls":null}` + "\n\n",
			`event: token_usage` + "\n" + `data: {"prompt_tokens":96,"completion_tokens":8,"total_tokens":104}` + "\n\n",
			`event: done` + "\n" + `data: {"finish_reason":"stop"}` + "\n\n",
		}, ""))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)
	svc.traeClient = &trae.Client{
		RemoteBase:   upstream.URL + "/api/remote/v1",
		CloudIDEHost: upstream.URL,
	}
	// 隔离偏好缓存（包级状态）。
	traeIDEPreferred.mu.Lock()
	saved := traeIDEPreferred.expires
	traeIDEPreferred.expires = map[string]time.Time{}
	traeIDEPreferred.mu.Unlock()
	t.Cleanup(func() {
		traeIDEPreferred.mu.Lock()
		traeIDEPreferred.expires = saved
		traeIDEPreferred.mu.Unlock()
	})

	body, _ := json.Marshal(map[string]any{
		"model":    "GLM-5.3",
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})

	// 第一笔：Remote 1005 → 回落 IDE（非流式，干净重试）。
	c, rec := traeTestContext()
	result, err := svc.forwardTraeChatCompletions(context.Background(), c, newTraeTestAccount(), body, "GLM-5.3")
	require.NoError(t, err)
	require.Equal(t, traeIDEUpstreamEndpoint, result.UpstreamEndpoint)
	require.Equal(t, 96, result.Usage.InputTokens)
	require.Contains(t, rec.Body.String(), "hello ide")
	require.Equal(t, 1, ideHits)
	require.Equal(t, 1, remoteEventHits)

	// 第二笔：偏好缓存命中，直连 IDE（不再打 Remote events）。
	c2, rec2 := traeTestContext()
	_, err = svc.forwardTraeChatCompletions(context.Background(), c2, newTraeTestAccount(), body, "GLM-5.3")
	require.NoError(t, err)
	require.Equal(t, 2, ideHits)
	require.Equal(t, 1, remoteEventHits)
	require.Contains(t, rec2.Body.String(), "hello ide")
}

// 流式回落：Remote 心跳（头未写，被丢弃）→ 1005 → IDE 流式正常输出，
// 客户端只看到一个连续 SSE 流。
func TestForwardTraeChatCompletionsIDEFallbackStream(t *testing.T) {
	var ideHits int
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
		fmt.Fprint(w, "event: heartbeat\n\n"+`event: error`+"\n"+`data: {"code":1005}`+"\n\n")
	})
	mux.HandleFunc("/api/remote/v1/chat_sessions/s-1/stop", func(w http.ResponseWriter, r *http.Request) {})
	mux.HandleFunc("/api/agent/v3/llm_utils_chat", func(w http.ResponseWriter, r *http.Request) {
		ideHits++
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, strings.Join([]string{
			`event: output` + "\n" + `data: {"response":"流式回归","reasoning_content":"思"}` + "\n\n",
			`event: token_usage` + "\n" + `data: {"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}` + "\n\n",
			`event: done` + "\n" + `data: {"finish_reason":"stop"}` + "\n\n",
		}, ""))
	})
	upstream := httptest.NewServer(mux)
	defer upstream.Close()
	svc := newTraeTestGateway(t, upstream)
	svc.traeClient = &trae.Client{
		RemoteBase:   upstream.URL + "/api/remote/v1",
		CloudIDEHost: upstream.URL,
	}
	traeIDEPreferred.mu.Lock()
	saved := traeIDEPreferred.expires
	traeIDEPreferred.expires = map[string]time.Time{}
	traeIDEPreferred.mu.Unlock()
	t.Cleanup(func() {
		traeIDEPreferred.mu.Lock()
		traeIDEPreferred.expires = saved
		traeIDEPreferred.mu.Unlock()
	})

	body, _ := json.Marshal(map[string]any{
		"model":  "GLM-5.3",
		"stream": true,
		"messages": []map[string]any{
			{"role": "user", "content": "hi"},
		},
	})
	c, rec := traeTestContext()
	result, err := svc.forwardTraeChatCompletions(context.Background(), c, newTraeTestAccount(), body, "GLM-5.3")
	require.NoError(t, err)
	require.True(t, result.Stream)
	require.Equal(t, traeIDEUpstreamEndpoint, result.UpstreamEndpoint)
	require.Equal(t, 1, ideHits)
	out := rec.Body.String()
	require.Contains(t, out, `"content":"流式回归"`)
	require.Contains(t, out, `"reasoning_content":"思"`)
	require.Contains(t, out, "data: [DONE]")
	require.Equal(t, 1, strings.Count(out, `"role":"assistant"`), "SSE 头只写一次")
}
