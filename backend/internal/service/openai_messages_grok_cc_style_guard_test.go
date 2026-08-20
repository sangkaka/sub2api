package service

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAppendGrokClaudeCodeStyleGuard_InsertsAfterDeveloperPrefix(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal([]apicompat.ResponsesInputItem{
		{
			Type:    "message",
			Role:    "developer",
			Content: mustMarshalGrokStyleContentParts(t, "project instructions"),
		},
		{
			Type:    "message",
			Role:    "user",
			Content: mustMarshalGrokStyleContentParts(t, "quote the util"),
		},
	})
	require.NoError(t, err)

	req := &apicompat.ResponsesRequest{Input: input}
	require.True(t, appendGrokClaudeCodeStyleGuard(req))

	var items []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	require.Len(t, items, 3)
	require.Equal(t, "developer", items[0].Role)
	require.Equal(t, "developer", items[1].Role)
	require.Equal(t, "user", items[2].Role)
	require.Contains(t, string(items[1].Content), "sub2api-grok-cc-style-guard")
	require.Contains(t, string(items[1].Content), "startLine:endLine:relative/path")
	require.Contains(t, string(items[1].Content), `\begin{cases}`)

	// Idempotent: second call must not insert again.
	require.False(t, appendGrokClaudeCodeStyleGuard(req))
	require.NoError(t, json.Unmarshal(req.Input, &items))
	require.Len(t, items, 3)
}

func TestAppendGrokClaudeCodeStyleGuard_EmptyInput(t *testing.T) {
	t.Parallel()

	require.False(t, appendGrokClaudeCodeStyleGuard(nil))
	require.False(t, appendGrokClaudeCodeStyleGuard(&apicompat.ResponsesRequest{}))
	require.False(t, appendGrokClaudeCodeStyleGuard(&apicompat.ResponsesRequest{Input: []byte(`[]`)}))
}

func TestAppendGrokClaudeCodeStyleGuard_UserOnly(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal([]apicompat.ResponsesInputItem{
		{
			Type:    "message",
			Role:    "user",
			Content: mustMarshalGrokStyleContentParts(t, "hi"),
		},
	})
	require.NoError(t, err)

	req := &apicompat.ResponsesRequest{Input: input}
	require.True(t, appendGrokClaudeCodeStyleGuard(req))

	var items []apicompat.ResponsesInputItem
	require.NoError(t, json.Unmarshal(req.Input, &items))
	require.Len(t, items, 2)
	require.Equal(t, "developer", items[0].Role)
	require.Equal(t, "user", items[1].Role)
	require.Contains(t, string(items[0].Content), "sub2api-grok-cc-style-guard")
}

func TestShouldInjectGrokClaudeCodeStyleGuard(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)

	sessionBody := []byte(`{"model":"claude-sonnet-4-5","metadata":{"user_id":"user_session_aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"},"messages":[{"role":"user","content":"hi"}]}`)
	plainBody := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hi"}]}`)

	t.Run("non-grok", func(t *testing.T) {
		t.Parallel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(plainBody))
		c.Request.Header.Set("User-Agent", "claude-cli/2.1.0")
		require.False(t, shouldInjectGrokClaudeCodeStyleGuard(&Account{Platform: PlatformOpenAI}, c, plainBody))
	})

	t.Run("nil account", func(t *testing.T) {
		t.Parallel()
		require.False(t, shouldInjectGrokClaudeCodeStyleGuard(nil, nil, plainBody))
	})

	t.Run("grok with claude-cli UA", func(t *testing.T) {
		t.Parallel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(plainBody))
		c.Request.Header.Set("User-Agent", "claude-cli/2.1.0")
		require.True(t, shouldInjectGrokClaudeCodeStyleGuard(&Account{Platform: PlatformGrok}, c, plainBody))
	})

	t.Run("grok with session metadata", func(t *testing.T) {
		t.Parallel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(sessionBody))
		require.True(t, shouldInjectGrokClaudeCodeStyleGuard(&Account{Platform: PlatformGrok}, c, sessionBody))
	})

	t.Run("grok with context flag", func(t *testing.T) {
		t.Parallel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(plainBody))
		c.Request = c.Request.WithContext(SetClaudeCodeClient(context.Background(), true))
		require.True(t, shouldInjectGrokClaudeCodeStyleGuard(&Account{Platform: PlatformGrok}, c, plainBody))
	})

	t.Run("grok plain sdk", func(t *testing.T) {
		t.Parallel()
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(plainBody))
		c.Request.Header.Set("User-Agent", "python-httpx/0.27")
		require.False(t, shouldInjectGrokClaudeCodeStyleGuard(&Account{Platform: PlatformGrok}, c, plainBody))
	})
}

func TestAppendGrokClaudeCodeStyleGuard_SurvivesPatchGrokResponsesBody(t *testing.T) {
	t.Parallel()

	input, err := json.Marshal([]apicompat.ResponsesInputItem{
		{
			Type:    "message",
			Role:    "user",
			Content: mustMarshalGrokStyleContentParts(t, "hi"),
		},
	})
	require.NoError(t, err)

	req := &apicompat.ResponsesRequest{
		Model:  "grok-4.5",
		Stream: true,
		Input:  input,
	}
	require.True(t, appendGrokClaudeCodeStyleGuard(req))

	body, err := json.Marshal(req)
	require.NoError(t, err)

	patched, err := patchGrokResponsesBody(body, "grok-4.5")
	require.NoError(t, err)
	// After outer json.Marshal/sjson, angle brackets may be HTML-escaped in
	// the wire form; the decoded string (and the model) still see the marker.
	require.Contains(t, gjson.GetBytes(patched, "input").Raw, "sub2api-grok-cc-style-guard")
	require.Equal(t, "developer", gjson.GetBytes(patched, "input.0.role").String())
	require.Contains(t, gjson.GetBytes(patched, "input.0.content.0.text").String(), "sub2api-grok-cc-style-guard")
	require.Contains(t, gjson.GetBytes(patched, "input.0.content.0.text").String(), "startLine:endLine:relative/path")
}

func mustMarshalGrokStyleContentParts(t *testing.T, text string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal([]apicompat.ResponsesContentPart{{
		Type: "input_text",
		Text: text,
	}})
	require.NoError(t, err)
	return b
}
