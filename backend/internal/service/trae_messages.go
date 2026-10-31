package service

// trae 平台的 /v1/messages（Anthropic Messages 入站）支持：
// 请求侧复用 CC 直转链的 Anthropic→ChatCompletions 转换，转发走
// forwardTraeChatCompletions（Remote/IDE 双通道与 1005 回落全部继承）；
// 响应侧经拦截 writer 把 runtime 写出的 OpenAI 形输出增量转译为
// Anthropic Messages 形（message_start / content_block_* / message_delta /
// message_stop），Claude Code 客户端可直接使用。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/apicompat"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"go.uber.org/zap"
)

// anthropicBodyHasImageBlocks 报告 Anthropic Messages 请求里是否携带 image
// 内容块（content 为块数组且任一块 type=image）。system 与字符串 content
// 不含图，无需扫描。
func anthropicBodyHasImageBlocks(body []byte) bool {
	for _, msg := range gjson.GetBytes(body, "messages").Array() {
		content := msg.Get("content")
		if !content.IsArray() {
			continue
		}
		for _, block := range content.Array() {
			if block.Get("type").String() == "image" {
				return true
			}
		}
	}
	return false
}

// forwardAnthropicViaTrae 是 ForwardAsAnthropic 的 trae 平台分支。
func (s *OpenAIGatewayService) forwardAnthropicViaTrae(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	defaultMappedModel string,
) (*OpenAIForwardResult, error) {
	var anthropicReq apicompat.AnthropicRequest
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "Failed to parse request body")
		return nil, fmt.Errorf("parse anthropic request: %w", err)
	}
	originalModel := anthropicReq.Model
	if strings.TrimSpace(originalModel) == "" {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", "model is required")
		return nil, fmt.Errorf("missing model in request")
	}
	applyOpenAICompatModelNormalization(&anthropicReq)
	clientStream := anthropicReq.Stream

	chatReq, err := apicompat.AnthropicToChatCompletionsRequestWithMinOutputTokens(
		&anthropicReq,
		configuredMinOutputTokens(s.cfg),
	)
	if err != nil {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error", err.Error())
		return nil, fmt.Errorf("convert anthropic to chat completions: %w", err)
	}
	billingModel := resolveOpenAIForwardModel(account, anthropicReq.Model, defaultMappedModel)
	upstreamModel := normalizeOpenAIModelForUpstream(account, billingModel)
	// 图片放行判定（2026-10-31）：csv 覆盖 OR agent 档 descriptor 官方
	// multimodal 标志。必须在 upstreamModel 解析之后——目录按上游名索引，
	// 用对外名（如 deepseek-flash-trae）查会 miss 而误拒；且必须先于
	// interposer 包 writer 写出 Anthropic 形 400（runtime 层的 OpenAI 形
	// 错误体会被拦截 writer 吞掉）。
	if anthropicBodyHasImageBlocks(body) && !s.traeImageAllowed(ctx, account, originalModel, upstreamModel) {
		writeAnthropicError(c, http.StatusBadRequest, "invalid_request_error",
			"image inputs are not supported for model "+originalModel+" on the trAE upstream: only multimodal-capable models (per the upstream catalog) or ide_image_models overrides accept images; remove image blocks or switch models")
		return nil, fmt.Errorf("trAE upstream does not support image inputs (model %s)", originalModel)
	}
	chatReq.Model = upstreamModel
	chatReq.ReasoningEffort = openAICompatAnthropicReasoningEffort(&anthropicReq, upstreamModel, chatReq.ReasoningEffort)
	chatReq.Stream = clientStream
	if clientStream {
		chatReq.StreamOptions = &apicompat.ChatStreamOptions{IncludeUsage: true}
	}
	chatBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat completions request: %w", err)
	}

	interposer := newTraeAnthropicWriter(c.Writer, originalModel, clientStream)
	c.Writer = interposer
	result, fwdErr := s.forwardTraeChatCompletions(ctx, c, account, chatBody, defaultMappedModel)
	c.Writer = interposer.ResponseWriter
	if fwdErr != nil {
		// 错误路径：拦截 writer 吞掉了 runtime 写出的 OpenAI 形错误体，
		// gin 视角响应未写过，上层（handler 回退/failover）会以 Anthropic
		// 形状写错误。
		return result, fwdErr
	}
	if finishErr := interposer.Finish(); finishErr != nil {
		return result, finishErr
	}
	return result, nil
}

// traeAnthropicWriter 拦截 trae runtime 的 OpenAI 形输出并转译为 Anthropic
// Messages 形。流式按 SSE 帧增量转译；非流式缓冲后整体转换（并修正
// Content-Length）。非 200 的中途写出被吞掉，交由上层统一写 Anthropic 错误。
type traeAnthropicWriter struct {
	gin.ResponseWriter
	model      string
	stream     bool
	state      *apicompat.ChatCompletionsToAnthropicStreamState
	clientGone bool

	headWritten bool
	errMode     bool
	frame       []byte // 未完结的 SSE 帧（流式）
	jsonBuf     []byte // 非流式成功缓冲
}

func newTraeAnthropicWriter(orig gin.ResponseWriter, model string, stream bool) *traeAnthropicWriter {
	return &traeAnthropicWriter{
		ResponseWriter: orig,
		model:          model,
		stream:         stream,
		state:          apicompat.NewChatCompletionsToAnthropicStreamState(model),
	}
}

func (w *traeAnthropicWriter) WriteHeader(code int) {
	if code != http.StatusOK {
		// 头已写（流式中途错误）：吞掉避免污染已开始的 SSE 流；
		// 头未写：进入错误模式，上层重写 Anthropic 错误。
		w.errMode = true
		return
	}
	if w.stream {
		if !w.headWritten {
			w.headWritten = true
			w.ResponseWriter.WriteHeader(code)
		}
		return
	}
	// 非流式成功：延迟到 Finish（Content-Length 需按转换后长度修正）。
}

func (w *traeAnthropicWriter) WriteHeaderNow() {}

func (w *traeAnthropicWriter) Written() bool { return w.headWritten }

func (w *traeAnthropicWriter) Write(p []byte) (int, error) {
	if w.errMode {
		return len(p), nil
	}
	if !w.stream {
		w.jsonBuf = append(w.jsonBuf, p...)
		return len(p), nil
	}
	w.frame = append(w.frame, p...)
	for {
		idx := bytes.Index(w.frame, []byte("\n\n"))
		if idx < 0 {
			break
		}
		frame := string(w.frame[:idx])
		w.frame = w.frame[idx+2:]
		if !w.headWritten {
			// runtime 惰性头若尚未写（首帧即数据），先补头。
			w.headWritten = true
			w.ResponseWriter.WriteHeader(http.StatusOK)
		}
		w.emitFrame(frame)
	}
	return len(p), nil
}

func (w *traeAnthropicWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *traeAnthropicWriter) Flush() {
	if w.errMode || !w.headWritten {
		return
	}
	w.ResponseWriter.Flush()
}

// emitFrame 处理一帧 OpenAI SSE：data: chunk → Anthropic 事件；注释心跳
// 原样透传；[DONE] 吞掉（Finish 统一收尾）。
func (w *traeAnthropicWriter) emitFrame(frame string) {
	for _, line := range strings.Split(frame, "\n") {
		line = strings.TrimRight(line, "\r")
		switch {
		case strings.HasPrefix(line, ":"):
			if _, err := fmt.Fprintf(w.ResponseWriter, "%s\n\n", line); err != nil {
				w.clientGone = true
				return
			}
			w.ResponseWriter.Flush()
		case strings.HasPrefix(line, "data:"):
			payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if payload == "[DONE]" {
				return
			}
			var chunk apicompat.ChatCompletionsChunk
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				continue
			}
			for _, evt := range apicompat.ChatCompletionsChunkToAnthropicEvents(&chunk, w.state) {
				sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
				if err != nil {
					continue
				}
				if _, werr := fmt.Fprint(w.ResponseWriter, sse); werr != nil {
					w.clientGone = true
					return
				}
			}
			w.ResponseWriter.Flush()
		}
	}
}

// Finish 收尾：流式补 Finalize 事件（message_delta/message_stop）；非流式
// 整体转换成功响应（含 Content-Length 修正）。错误模式无操作。
func (w *traeAnthropicWriter) Finish() error {
	if w.errMode {
		return nil
	}
	if w.stream {
		if w.clientGone {
			return nil
		}
		for _, evt := range apicompat.FinalizeChatCompletionsAnthropicStream(w.state) {
			sse, err := apicompat.ResponsesAnthropicEventToSSE(evt)
			if err != nil {
				continue
			}
			if _, werr := fmt.Fprint(w.ResponseWriter, sse); werr != nil {
				break
			}
		}
		w.ResponseWriter.Flush()
		if w.state.LateToolArgFragmentsDropped > 0 {
			// 迟到参数分片被丢弃（工具块已被后续块关闭，协议禁止补 delta）。
			// 丢弃保住了流合法性（客户端不再判 malformed），代价是该工具参数
			// 不完整——采样留证，供上游形态分析。
			logger.L().Warn("trae anthropic bridge dropped late tool arg fragments",
				zap.Int("fragments", w.state.LateToolArgFragmentsDropped),
				zap.String("sample", w.state.LateToolArgSample),
				zap.String("model", w.model))
		}
		return nil
	}
	var ccResp apicompat.ChatCompletionsResponse
	if err := json.Unmarshal(w.jsonBuf, &ccResp); err != nil {
		logger.L().Warn("trae messages: convert buffered chat completions failed",
			zap.Error(err), zap.Int("body_bytes", len(w.jsonBuf)))
		writeAnthropicErrorBody(w.ResponseWriter, http.StatusBadGateway, "api_error", "trae upstream response conversion failed")
		return fmt.Errorf("trae messages: parse chat completions response: %w", err)
	}
	anthropicResp := apicompat.ChatCompletionsResponseToAnthropic(&ccResp, w.model)
	payload, err := json.Marshal(anthropicResp)
	if err != nil {
		return fmt.Errorf("trae messages: marshal anthropic response: %w", err)
	}
	w.Header().Del("Content-Length")
	w.ResponseWriter.WriteHeader(http.StatusOK)
	if _, werr := w.ResponseWriter.Write(payload); werr != nil {
		w.clientGone = true
	}
	return nil
}

// writeAnthropicErrorBody 直接在裸 writer 上写 Anthropic 形错误（拦截态下
// 不能再借 gin.Context，避免经过被替换的 writer）。
func writeAnthropicErrorBody(w gin.ResponseWriter, status int, errType, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	payload, _ := json.Marshal(gin.H{
		"type":  "error",
		"error": gin.H{"type": errType, "message": message},
	})
	_, _ = w.Write(payload)
}
