//go:build unit

package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
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
