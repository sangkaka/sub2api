package trae

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

// ErrorKind 是 Trae 上游错误的调度语义分类。
type ErrorKind string

const (
	// ErrorKindRateLimit 账号并发/配额限流（业务码 3004/4008/4011、HTTP 429
	// 及同义文本）：应触发账号冷却并换号。
	ErrorKindRateLimit ErrorKind = "rate_limit"
	// ErrorKindAuth token 失效（业务码 1001、HTTP 401）：强制刷新后单次重试。
	ErrorKindAuth ErrorKind = "auth"
	// ErrorKindUpstream 其它上游错误：不可重试，透出明确信息。
	ErrorKindUpstream ErrorKind = "upstream"
)

// UpstreamError 携带业务码与调度语义。业务码可能出现在 SSE error 事件或
// HTTP 状态层，两类来源统一走本类型。
type UpstreamError struct {
	Kind    ErrorKind
	Code    string
	Message string
	// ObservedModelOutput 为 true 表示错误发生前已观察到模型输出（正文/
	// 思考/usage）；此时不得重放请求（会话已产生计费）。
	ObservedModelOutput bool
}

func (e *UpstreamError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("trae upstream error [%s]: %s", e.Code, e.Message)
	}
	return "trae upstream error: " + e.Message
}

// AsUpstreamError 提取 *UpstreamError（非该类型返回 nil）。
func AsUpstreamError(err error) *UpstreamError {
	for err != nil {
		if ue, ok := err.(*UpstreamError); ok {
			return ue
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return nil
		}
		err = u.Unwrap()
	}
	return nil
}

// classifyHTTPError 按状态码给出调度语义。
func classifyHTTPError(status int) ErrorKind {
	switch {
	case status == http.StatusUnauthorized:
		return ErrorKindAuth
	case status == http.StatusTooManyRequests:
		return ErrorKindRateLimit
	default:
		return ErrorKindUpstream
	}
}

// rateLimitCodes 是账号并发/配额窗口的业务码族。4008 是硬配额，3004/4011
// 与 HTTP 429 是同一账号容量窗口在 native/remote 流上的不同表现。
var rateLimitCodes = map[string]bool{
	"3004": true,
	"4008": true,
	"4011": true,
	"429":  true,
}

// authCodes 是 token 失效业务码。
var authCodes = map[string]bool{
	"1001": true,
	"401":  true,
}

// ClassifyUpstreamError 把业务码/文本归类为带调度语义的 UpstreamError。
// observedModelOutput 由调用方传入（错误发生前是否已有模型输出）。
func ClassifyUpstreamError(code, message string, observedModelOutput bool) *UpstreamError {
	kind := ErrorKindUpstream
	normalizedCode := strings.TrimSpace(code)
	if rateLimitCodes[normalizedCode] {
		kind = ErrorKindRateLimit
	} else if authCodes[normalizedCode] {
		kind = ErrorKindAuth
	} else {
		text := strings.ToLower(message)
		switch {
		case strings.Contains(text, "exceeded the quota"),
			strings.Contains(text, "quota exceeded"),
			strings.Contains(text, "rate limit"),
			strings.Contains(text, "rate-limit"),
			strings.Contains(text, "too many requests"):
			kind = ErrorKindRateLimit
		}
	}
	return &UpstreamError{Kind: kind, Code: normalizedCode, Message: message, ObservedModelOutput: observedModelOutput}
}

// ExtractErrorDetails 从 SSE error 事件载荷中提取业务码与可读信息。
// 载荷形状：顶层或嵌套 error 对象上的 code/errorCode + message/detail/reason。
func ExtractErrorDetails(payload map[string]any) (code, message string) {
	nested, _ := payload["error"].(map[string]any)
	sources := []map[string]any{payload}
	if nested != nil {
		sources = append(sources, nested)
	}
	code = firstStringValue(sources, "code", "error_code", "errorCode")
	message = firstStringValue(sources, "message", "detail", "reason")
	if message == "" {
		if s, ok := payload["error"].(string); ok {
			message = s
		}
	}
	if message == "" {
		message = "Trae upstream returned an error event"
	}
	return code, message
}

// firstStringValue 依次在 sources 里查 keys，返回首个非空字符串；数字型
// code（JSON number 或 Go 整型）转为十进制字符串。
func firstStringValue(sources []map[string]any, keys ...string) string {
	for _, m := range sources {
		for _, key := range keys {
			switch v := m[key].(type) {
			case string:
				if s := strings.TrimSpace(v); s != "" {
					return s
				}
			case float64:
				return strings.TrimSuffix(fmt.Sprintf("%v", v), ".0")
			case int:
				return strconv.Itoa(v)
			case int64:
				return strconv.FormatInt(v, 10)
			case json.Number:
				if s := strings.TrimSpace(v.String()); s != "" {
					return s
				}
			}
		}
	}
	return ""
}
