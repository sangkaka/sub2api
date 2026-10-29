package trae

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestNormalizeEventName(t *testing.T) {
	cases := map[string]string{
		"heartbeat":          EventHeartbeat,
		"keepalive":          "keepalive",
		"model_config":       EventModelConfig,
		"modelConfig":        EventModelConfig,
		"modelconfig":        EventModelConfig,
		"plan_item":          EventPlanItem,
		"planItem":           EventPlanItem,
		"planitem":           EventPlanItem,
		"token_usage":        EventTokenUsage,
		"tokenUsage":         EventTokenUsage,
		"tokenusage":         EventTokenUsage,
		"done":               EventDone,
		"responseDone":       EventDone,
		"responsedone":       EventDone,
		"streamdone":         EventDone,
		"requestwaitinqueue": EventRequestWaitInQ,
		"message":            EventMessage,
	}
	for raw, want := range cases {
		require.Equal(t, want, NormalizeEventName(raw), raw)
	}
	require.Empty(t, NormalizeEventName(""))
}

func TestFlattenMessages(t *testing.T) {
	messages := []ChatMessage{
		{Role: "system", Content: json.RawMessage(`"you are helpful"`)},
		{Role: "user", Content: json.RawMessage(`"hello"`)},
		{Role: "assistant", Content: json.RawMessage(`"hi there"`)},
		{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"part1"},{"type":"text","text":"part2"}]`)},
		{Role: "tool", Content: json.RawMessage(`"result body"`), ToolCallID: "call_1", Name: "get_weather"},
	}
	query := FlattenMessages(messages)
	var parts []map[string]any
	require.NoError(t, json.Unmarshal([]byte(query), &parts))
	require.Len(t, parts, 1)
	require.Equal(t, "text", parts[0]["type"])
	content := parts[0]["data"].(map[string]any)["content"].(string)
	require.Contains(t, content, "[System]\nyou are helpful")
	require.Contains(t, content, "[Assistant]\nhi there")
	require.Contains(t, content, "[Client Tool Result: call_1 get_weather status=succeeded]\nresult body")
	require.Contains(t, content, "part1part2")
	// 每段之间以空行分隔，段落顺序保持消息顺序。
	require.Less(t, strings.Index(content, "you are helpful"), strings.Index(content, "hello"))
}

func TestTextAccumulatorSnapshotDiff(t *testing.T) {
	acc := &textAccumulator{}
	require.Equal(t, "abc", acc.delta("abc"))
	require.Equal(t, "def", acc.delta("abcdef")) // 增长：追加后缀
	require.Equal(t, "", acc.delta("abc"))       // 回退：忽略
	require.Equal(t, "xyz", acc.delta("abcxyz")) // 分叉：按公共前缀差分
	require.Equal(t, "", acc.delta(""))          // 空快照
}

func TestMergePlanMessageText(t *testing.T) {
	require.Equal(t, "msg", mergePlanMessageText("", "msg"))
	require.Equal(t, "plan", mergePlanMessageText("plan", ""))
	require.Equal(t, "plan", mergePlanMessageText("plan", "plan"))
	require.Equal(t, "plan full", mergePlanMessageText("plan full", "plan"))
	require.Equal(t, "plan full", mergePlanMessageText("plan", "plan full"))
	// 后缀/前缀重叠（≥4 字符）只拼一次。
	require.Equal(t, "hello world peace", mergePlanMessageText("hello world", "world peace"))
	// 低于重叠下限按无重叠处理。
	require.Equal(t, "abcd\n\ndef", mergePlanMessageText("abcd", "def"))
	// 无重叠时空行连接。
	require.Equal(t, "a\n\nb", mergePlanMessageText("a", "b"))
	require.Equal(t, "plan\n\nfull plan", mergePlanMessageText("plan", "full plan"))
}

// 快照差分流式翻译：plan_item 的 thought/reasoning_content 与 content 均为
// 累计快照，翻译器应输出增量而非重放全文。
func TestStreamTranslatorPlanSnapshotDiff(t *testing.T) {
	tr := NewStreamTranslator("glm-5.3")
	roleDeltas := tr.ProcessEvent(Event{Name: EventPlanItem, Data: map[string]any{
		"id": "p1", "reasoning_content": "thinking...",
	}})
	require.Len(t, roleDeltas, 1)
	require.Equal(t, "thinking...", roleDeltas[0].Reasoning)

	deltas := tr.ProcessEvent(Event{Name: EventPlanItem, Data: map[string]any{
		"id": "p1", "reasoning_content": "thinking... more", "content": "ans",
	}})
	require.Len(t, deltas, 2)
	require.Equal(t, " more", deltas[0].Reasoning)
	require.Equal(t, "ans", deltas[1].Content)

	deltas = tr.ProcessEvent(Event{Name: EventPlanItem, Data: map[string]any{
		"id": "p1", "reasoning_content": "thinking... more", "content": "answer grows",
	}})
	require.Len(t, deltas, 1)
	require.Equal(t, "wer grows", deltas[0].Content)

	// 心跳必须透传为注释帧。
	deltas = tr.ProcessEvent(Event{Name: EventHeartbeat})
	require.Len(t, deltas, 1)
	require.True(t, deltas[0].Heartbeat)

	// usage 映射 + done。
	deltas = tr.ProcessEvent(Event{Name: EventTokenUsage, Data: map[string]any{
		"usage": map[string]any{"prompt_tokens": 12, "outputTokens": 7, "cache_read_tokens": 5},
	}})
	require.Empty(t, deltas)
	require.True(t, tr.SawOutput())

	deltas = tr.ProcessEvent(Event{Name: EventDone})
	require.Empty(t, deltas)

	result, tail := tr.Finalize(false)
	require.Empty(t, tail)
	require.Equal(t, "answer grows", result.Content)
	require.Equal(t, "thinking... more", result.Reasoning)
	require.Equal(t, "stop", result.FinishReason)
	require.False(t, result.Truncated)
	require.NotNil(t, result.Usage)
	require.Equal(t, 12, result.Usage.PromptTokens)
	require.Equal(t, 7, result.Usage.CompletionTokens)
	require.Equal(t, 19, result.Usage.TotalTokens)
	require.Equal(t, 5, result.Usage.PromptTokensDetails.CachedTokens)
}

// model_config 回填上游实际模型名。
func TestStreamTranslatorProviderModel(t *testing.T) {
	tr := NewStreamTranslator("glm-5.3")
	tr.ProcessEvent(Event{Name: EventModelConfig, Data: map[string]any{
		"model_config": map[string]any{"provider_model_name": "glm-5.3-trae"},
	}})
	result, _ := tr.Finalize(false)
	require.Equal(t, "glm-5.3-trae", result.ProviderModel)
}

// 事件流 EOF 无 done 帧判为截断。
func TestStreamTranslatorTruncation(t *testing.T) {
	tr := NewStreamTranslator("m")
	tr.ProcessEvent(Event{Name: EventPlanItem, Data: map[string]any{"id": "p1", "content": "partial"}})
	result, _ := tr.Finalize(true)
	require.True(t, result.Truncated)
	require.Equal(t, "partial", result.Content)
}

func errorCases() map[string]struct {
	kind     ErrorKind
	code     string
	observed bool
} {
	return map[string]struct {
		kind     ErrorKind
		code     string
		observed bool
	}{
		"4011":        {ErrorKindRateLimit, "4011", false},
		"429":         {ErrorKindRateLimit, "429", false},
		"3004":        {ErrorKindRateLimit, "3004", false},
		"4008":        {ErrorKindRateLimit, "4008", false},
		"1001":        {ErrorKindAuth, "1001", false},
		"401":         {ErrorKindAuth, "401", false},
		"text-quota":  {ErrorKindRateLimit, "", false},
		"other-code":  {ErrorKindUpstream, "500", false},
		"post-output": {ErrorKindRateLimit, "4011", true},
	}
}

func TestStreamTranslatorErrorMapping(t *testing.T) {
	cases := errorCases()
	for name, want := range cases {
		t.Run(name, func(t *testing.T) {
			tr := NewStreamTranslator("m")
			if want.observed {
				tr.ProcessEvent(Event{Name: EventPlanItem, Data: map[string]any{"id": "p1", "content": "text"}})
			}
			data := map[string]any{}
			switch name {
			case "text-quota":
				data["message"] = "Requests have exceeded the quota"
			case "post-output":
				data["code"] = 4011
				data["message"] = "parallel limit"
			case "other-code":
				data["code"] = "500"
			default:
				data["code"] = name
			}
			deltas := tr.ProcessEvent(Event{Name: EventError, Data: data})
			require.Empty(t, deltas)
			result, _ := tr.Finalize(false)
			require.NotNil(t, result.Err)
			require.Equal(t, want.kind, result.Err.Kind)
			require.Equal(t, want.observed, result.Err.ObservedModelOutput)
			if want.code != "" {
				require.Equal(t, want.code, result.Err.Code)
			}
		})
	}
}

func TestNormalizeExpireAt(t *testing.T) {
	// 秒级时间戳。
	sec, ok := NormalizeExpireAt("1762300000")
	require.True(t, ok)
	require.Equal(t, int64(1762300000), sec.Unix())
	// 毫秒级时间戳。
	ms, ok := NormalizeExpireAt("1762300000123")
	require.True(t, ok)
	require.Equal(t, int64(1762300000123), ms.UnixMilli())
	// RFC3339。
	iso, ok := NormalizeExpireAt("2026-11-01T12:00:00Z")
	require.True(t, ok)
	require.Equal(t, 2026, iso.Year())
	// naive ISO（无时区按 UTC）。
	naive, ok := NormalizeExpireAt("2026-11-01T12:00:00")
	require.True(t, ok)
	require.Equal(t, 12, naive.Hour())
	// 空与非法。
	_, ok = NormalizeExpireAt("")
	require.False(t, ok)
	_, ok = NormalizeExpireAt("not-a-time")
	require.False(t, ok)
}

func TestValidateRefreshRotation(t *testing.T) {
	require.NoError(t, ValidateRefreshRotation("rt-1", "rt-1"))
	require.NoError(t, ValidateRefreshRotation("rt-1", "")) // 当前值为空不拦（首刷）
	require.Error(t, ValidateRefreshRotation("rt-1", "rt-2"))
}

func TestClampReasoningEffort(t *testing.T) {
	require.Equal(t, "light", NormalizeReasoningEffort("low"))
	require.Equal(t, "high", NormalizeReasoningEffort("medium"))
	require.Equal(t, "extra_high", NormalizeReasoningEffort("ultra"))
	require.Empty(t, NormalizeReasoningEffort("bogus"))

	descriptor := &ModelDescriptor{Raw: map[string]any{
		"reasoning_effort_config": map[string]any{
			"reasoning_effort_levels": []any{"light", "high"},
		},
	}}
	require.Equal(t, "high", ClampReasoningEffort("extra_high", descriptor)) // 高于声明档位取最高
	require.Equal(t, "light", ClampReasoningEffort("light", descriptor))
	require.Equal(t, "high", ClampReasoningEffort("high", descriptor))
	// 未声明档位：原样。
	noConfig := &ModelDescriptor{Raw: map[string]any{}}
	require.Equal(t, "extra_high", ClampReasoningEffort("extra_high", noConfig))
}

// SSE 线路解析：event:/data: 帧、[DONE]、注释行、无结尾空行的尾帧。
func TestStreamEventsWireParsing(t *testing.T) {
	var sawAuth, sawAccept string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		sawAccept = r.Header.Get("Accept")
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		fmt.Fprint(w, ": comment line\n\n")
		fmt.Fprint(w, "event: heartbeat\n\n")
		fmt.Fprint(w, "event: plan_item\ndata: {\"id\":\"p1\",\"content\":\"hello\"}\n\n")
		fmt.Fprint(w, "data: {\"event\":\"token_usage\",\"usage\":{\"prompt_tokens\":3,\"output_tokens\":4}}\n\n")
		// 尾帧无结尾空行：上游常见行为，解析器不得丢弃。
		fmt.Fprint(w, "event: done\ndata: [DONE]")
		flusher.Flush()
	}))
	defer server.Close()

	client := &Client{RemoteBase: server.URL}
	events, errs := client.StreamEvents(context.Background(), "jwt-token", "sid-1", "mid-1")
	var names []string
	var lastErr error
loop:
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				break loop
			}
			names = append(names, ev.Name)
		case err, ok := <-errs:
			if ok && err != nil {
				lastErr = err
			}
		}
	}
	require.NoError(t, lastErr)
	require.Equal(t, []string{EventHeartbeat, EventPlanItem, EventTokenUsage, EventDone}, names)
	require.Equal(t, "Cloud-IDE-JWT jwt-token", sawAuth)
	require.Equal(t, "text/event-stream", sawAccept)
}

func TestExchangeTokenWire(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, exchangeTokenPath, r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		fmt.Fprint(w, `{"Result":{"Token":"jwt-2","RefreshToken":"rt-2","TokenExpireAt":"1762300000","UserInfo":{"UserID":"u1","WebId":"w1"}}}`)
	}))
	defer server.Close()

	client := &Client{APIHost: server.URL}
	result, err := client.ExchangeToken(context.Background(), "rt-1", "")
	require.NoError(t, err)
	require.Equal(t, "jwt-2", result.Token)
	require.Equal(t, "rt-2", result.RefreshToken)
	require.Equal(t, "u1", result.UserInfo.UserID)
	require.Equal(t, PublicClientID, gotBody["ClientID"])
	require.Equal(t, "rt-1", gotBody["RefreshToken"])
	require.Equal(t, "-", gotBody["ClientSecret"])
}

func TestExchangeTokenAuthError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"ResponseMetadata":{"Error":{"Message":"invalid refresh token"}}}`)
	}))
	defer server.Close()

	client := &Client{APIHost: server.URL}
	_, err := client.ExchangeToken(context.Background(), "stale-rt", "")
	require.Error(t, err)
	ue := AsUpstreamError(err)
	require.NotNil(t, ue)
	require.Equal(t, ErrorKindAuth, ue.Kind)
}

// 企业控制台（console.enterprise.trae.cn）的兑换响应是 {code,message,Data}
// 包装、时间为数字毫秒；CloudIDEHost 覆盖作用于 ExchangeToken/GetUserInfo。
func TestExchangeTokenEnterpriseWire(t *testing.T) {
	userInfoHit := false
	exchangeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case exchangeTokenPath:
			fmt.Fprint(w, `{"code":0,"message":"","Data":{"Token":"jwt-ent","RefreshToken":"rt-ent","RefreshExpireAt":1801054911755}}`)
		case getUserInfoPath:
			userInfoHit = true
			fmt.Fprint(w, `{"Result":{"UserID":"433084928","Region":"CN"}}`)
		default:
			t.Fatalf("unexpected path on cloud-ide host: %s", r.URL.Path)
		}
	}))
	defer exchangeServer.Close()
	apiServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("api host must not be hit by ExchangeToken/GetUserInfo: %s", r.URL.Path)
	}))
	defer apiServer.Close()

	client := &Client{APIHost: apiServer.URL, CloudIDEHost: exchangeServer.URL}
	result, err := client.ExchangeToken(context.Background(), "rt-ent-old", "")
	require.NoError(t, err)
	require.Equal(t, "jwt-ent", result.Token)
	require.Equal(t, "rt-ent", result.RefreshToken)
	// 数字毫秒时间戳归一化为字符串后仍可被 NormalizeExpireAt 解析。
	_, ok := NormalizeExpireAt(string(result.RefreshExpireAt))
	require.True(t, ok)

	info, err := client.GetUserInfo(context.Background(), "jwt-ent")
	require.NoError(t, err)
	require.True(t, userInfoHit)
	require.Equal(t, "433084928", info.UserID)
}

func TestExchangeTokenEnterpriseCodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":20101,"message":"Token 无效：refresh token is invalid.","Data":null}`)
	}))
	defer server.Close()

	client := &Client{CloudIDEHost: server.URL}
	_, err := client.ExchangeToken(context.Background(), "stale-rt", "")
	require.Error(t, err)
	ue := AsUpstreamError(err)
	require.NotNil(t, ue)
	require.Equal(t, ErrorKindAuth, ue.Kind)
	require.Contains(t, ue.Message, "20101")
}

func TestJWTExp(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"exp":1794488528}`))
	exp, ok := JWTExp("header." + payload + ".sig")
	require.True(t, ok)
	require.Equal(t, int64(1794488528), exp.Unix())

	_, ok = JWTExp("not-a-jwt")
	require.False(t, ok)
	noExp := base64.RawURLEncoding.EncodeToString([]byte(`{"no_exp":1}`))
	_, ok = JWTExp("header." + noExp + ".sig")
	require.False(t, ok)
}

// CreateSession 请求形状：agent_type/agent_id 钉住、manual 策略、custom_model
// 回带 descriptor、reasoning_effort 注入。
func TestCreateSessionWire(t *testing.T) {
	var gotBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, sessionsPath, r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&gotBody))
		fmt.Fprint(w, `{"data":{"chat_session_id":"s-1","message_id":"m-1"}}`)
	}))
	defer server.Close()

	client := &Client{RemoteBase: server.URL}
	handle, err := client.CreateSession(context.Background(), "jwt", SessionRequest{
		Query:           FlattenMessages([]ChatMessage{{Role: "user", Content: json.RawMessage(`"hi"`)}}),
		Model:           "GLM-5.3",
		ReasoningEffort: "high",
		CommonParams:    `{"web_id":"w1"}`,
		Descriptor: &ModelDescriptor{ConfigName: "GLM-5.3", Provider: "zhipu", Raw: map[string]any{
			"config_name": "GLM-5.3", "provider": "zhipu", "config_source": 1,
		}},
	})
	require.NoError(t, err)
	require.Equal(t, "s-1", handle.SessionID)
	require.Equal(t, "m-1", handle.MessageID)

	require.Equal(t, "code", gotBody["mode"])
	require.Equal(t, "remote", gotBody["env"])
	require.Equal(t, false, gotBody["auto_create_project"])
	require.Equal(t, "web", gotBody["origin"])
	initial := gotBody["initial_message"].(map[string]any)
	require.Equal(t, "solo_agent_remote", initial["agent_type"])
	require.Equal(t, "solo_agent_remote", initial["agent_id"])
	require.Equal(t, "manual", initial["model_selection_strategy"])
	require.Equal(t, "GLM-5.3", initial["model_name"])
	require.Equal(t, `{"web_id":"w1"}`, initial["common_params"])
	custom := initial["custom_model"].(map[string]any)
	require.Equal(t, "GLM-5.3", custom["config_name"])
	require.Equal(t, "zhipu", custom["provider"])
	require.Equal(t, "high", custom["reasoning_effort"])
}

// models 目录解析：分组条目 → descriptor；reasoning 档位提取。
func TestFetchModels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Contains(t, r.URL.RawQuery, "functions=solo_agent_remote")
		fmt.Fprint(w, `{"data":{"list":[
			{"function":"solo_agent_remote","models":[
				{"config_name":"GLM-5.3","provider":"zhipu","reasoning_effort_config":{"reasoning_effort_levels":["light","high","extra_high"]}},
				{"config_name":"Kimi-K2"}
			]},
			{"function":"solo_work_remote","models":[{"config_name":"GLM-5.3"}]}
		]}}`)
	}))
	defer server.Close()

	client := &Client{RemoteBase: server.URL}
	models, err := client.FetchModels(context.Background(), "jwt")
	require.NoError(t, err)
	require.Len(t, models, 2) // GLM-5.3 跨组去重
	require.Equal(t, "GLM-5.3", models[0].ConfigName)
	require.Equal(t, []string{"light", "high", "extra_high"}, models[0].ReasoningEfforts())
	require.Empty(t, models[1].ReasoningEfforts())
}

func TestEntitlementUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, entUsagePath, r.URL.Path)
		fmt.Fprint(w, `{"data":{"quota":{"solo_agent_parallel_limit":2},"user_entitlement_pack_list":[
			{"product_type":2,"credits_info":{"credits_limit":100,"credits_used":30}},
			{"product_type":0,"credits_info":{"credits_limit":9}}
		]}}`)
	}))
	defer server.Close()

	client := &Client{APIHost: server.URL}
	ent, err := client.FetchEntitlementUsage(context.Background(), "jwt")
	require.NoError(t, err)
	require.Equal(t, 2, ent.ParallelLimit)
	require.Equal(t, 100.0, ent.CreditsTotal)
	require.Equal(t, 30.0, ent.CreditsUsed)
}

var _ = time.Second
