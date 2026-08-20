package service

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/gin-gonic/gin"
)

// Grok → Claude Code 输出风格约束：补全 citation fence，避免裸 LaTeX。
// 形态对齐 todo-guard（developer 消息 + marker 幂等），仅挂在 ForwardAsAnthropic
// 的 Responses 主路径；不改写响应流。
const (
	grokClaudeCodeStyleGuardMarker = "<sub2api-grok-cc-style-guard>"
	// Raw text must stay free of source-indent tabs; models read it verbatim.
	grokClaudeCodeStyleGuardText = grokClaudeCodeStyleGuardMarker + "\n" +
		"When answering in Claude Code:\n" +
		"\n" +
		"Code citations (when quoting existing workspace files):\n" +
		"- Use a full fenced code block whose info string is exactly startLine:endLine:relative/path\n" +
		"  (example info string: 71:88:src/util/StockPriceUtil.java).\n" +
		"- Put the cited source lines inside the fence. Never emit a bare startLine:endLine:path line without the fence.\n" +
		"- Prefer workspace-relative paths. Do not invent paths.\n" +
		"\n" +
		"Math / formulas:\n" +
		"- Do not use LaTeX delimiters \\[ \\] \\( \\) or environments like \\begin{cases}.\n" +
		"- Claude Code renders GitHub-flavored Markdown only; use plain text, a markdown table, or a normal text fence.\n" +
		"\n" +
		"Inline file pointers may use path:line when a full citation fence is unnecessary.\n" +
		"</sub2api-grok-cc-style-guard>"
)

// shouldInjectGrokClaudeCodeStyleGuard decides whether ForwardAsAnthropic should
// attach the Grok→Claude Code style guard for this request.
//
// OpenAIGatewayHandler.Messages does not always call SetClaudeCodeClientContext,
// so detection is OR of: context flag, claude-cli User-Agent, or Claude Code session.
func shouldInjectGrokClaudeCodeStyleGuard(account *Account, c *gin.Context, body []byte) bool {
	if account == nil || account.Platform != PlatformGrok {
		return false
	}
	return isGrokClaudeCodeStyleClient(c, body)
}

func isGrokClaudeCodeStyleClient(c *gin.Context, body []byte) bool {
	if c != nil && c.Request != nil && IsClaudeCodeClient(c.Request.Context()) {
		return true
	}
	if c != nil {
		ua := strings.TrimSpace(c.GetHeader("User-Agent"))
		if claudeCodeUAPattern.MatchString(ua) {
			return true
		}
	}
	return extractClaudeCodeSessionID(c, body) != ""
}

// appendGrokClaudeCodeStyleGuard inserts a stable developer block into Responses
// input. Returns true when a new block was inserted; false when skipped (nil,
// empty input, or marker already present).
func appendGrokClaudeCodeStyleGuard(req *apicompat.ResponsesRequest) bool {
	if req == nil || len(req.Input) == 0 {
		return false
	}

	var items []apicompat.ResponsesInputItem
	if err := json.Unmarshal(req.Input, &items); err != nil {
		return false
	}
	// Match the bare token so HTML-escaped forms (<...>) still count
	// as already injected after a later encoding/json or sjson re-marshal.
	if len(items) == 0 || responsesInputItemsContainText(items, "sub2api-grok-cc-style-guard") {
		return false
	}

	// Keep literal <marker> (and LaTeX backslashes) so idempotency checks and
	// upstream models see the same text; encoding/json defaults escape HTML.
	content, err := marshalJSONNoHTMLEscape([]apicompat.ResponsesContentPart{{
		Type: "input_text",
		Text: grokClaudeCodeStyleGuardText,
	}})
	if err != nil {
		return false
	}

	guard := apicompat.ResponsesInputItem{
		Type:    "message",
		Role:    "developer",
		Content: content,
	}

	insertAt := 0
	for insertAt < len(items) && items[insertAt].Type == "message" && items[insertAt].Role == "developer" {
		insertAt++
	}

	items = append(items, apicompat.ResponsesInputItem{})
	copy(items[insertAt+1:], items[insertAt:])
	items[insertAt] = guard

	input, err := marshalJSONNoHTMLEscape(items)
	if err != nil {
		return false
	}
	req.Input = input
	return true
}

func marshalJSONNoHTMLEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
