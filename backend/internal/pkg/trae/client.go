package trae

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// setRemoteHeaders 写 Remote 会话协议请求头组。鉴权 scheme 是非标准的
// Cloud-IDE-JWT（不是 Bearer）；Origin/Referer 按 Solo Web 客户端伪装。
func (c *Client) setRemoteHeaders(req *http.Request, token string, stream bool) {
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+strings.TrimSpace(token))
	req.Header.Set("X-Trae-Client-Type", "web")
	req.Header.Set("X-Preferenced-Language", "zh-CN")
	req.Header.Set("x-user-region", "CN")
	req.Header.Set("Origin", c.webOrigin())
	req.Header.Set("Referer", c.webOrigin()+"/")
	req.Header.Set("User-Agent", UserAgentChrome)
	if stream {
		req.Header.Set("Accept", "text/event-stream")
	} else {
		req.Header.Set("Accept", "application/json")
	}
	if req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
}

// SessionRequest 是建会话请求的翻译器侧输入。
type SessionRequest struct {
	// Query 是扁平化后的 query JSON 字符串（FlattenMessages 产物）。
	Query string
	// ExtraQueryParts 追加到 query parts 数组的额外载荷（图片 media parts）。
	ExtraQueryParts []map[string]any
	// Model 是上游 config_name；Descriptor 必须随请求回带（custom_model）。
	Model      string
	Descriptor *ModelDescriptor
	// ReasoningEffort 是钳制后的思考强度（light/high/extra_high），空则不传。
	ReasoningEffort string
	// CommonParams 是身份/设备字段的 JSON 字符串（BuildCommonParams 产物）。
	CommonParams string
	// BizSessionID 是稳定会话 id（账号+模型派生），上游按它复用会话上下文。
	BizSessionID string
	// MaxModeFields 非 nil 时（descriptor.MaxModeFields 产物）合并进
	// initial_message，把会话钉在 1M Max 档。
	MaxModeFields map[string]any
}

type initialMessage struct {
	ChatSessionID          string         `json:"chat_session_id"`
	Content                []any          `json:"content"`
	Query                  string         `json:"query"`
	ModelName              string         `json:"model_name"`
	AgentType              string         `json:"agent_type"`
	AgentID                string         `json:"agent_id"`
	ModelSelectionStrategy string         `json:"model_selection_strategy"`
	CommonParams           string         `json:"common_params"`
	ModelConfigSource      int            `json:"model_config_source,omitempty"`
	ModelIsPreset          bool           `json:"model_is_preset,omitempty"`
	ModelProvider          string         `json:"model_provider,omitempty"`
	CustomModel            map[string]any `json:"custom_model,omitempty"`
	// ExtraFields 携带 1M Max 等档位字段（context_window_size、
	// prompt_max_tokens、max_tokens、mode_type），经 map 合并输出。
	ExtraFields map[string]any `json:"-"`
}

// MarshalJSON 把 ExtraFields 平铺进顶层（避免嵌套结构体）。
func (m initialMessage) MarshalJSON() ([]byte, error) {
	type alias initialMessage
	base, err := json.Marshal(alias(m))
	if err != nil {
		return nil, err
	}
	if len(m.ExtraFields) == 0 {
		return base, nil
	}
	merged := map[string]any{}
	if err := json.Unmarshal(base, &merged); err != nil {
		return nil, err
	}
	for key, value := range m.ExtraFields {
		merged[key] = value
	}
	return json.Marshal(merged)
}

// appendQueryParts 把额外 parts（图片媒体载荷）追加到 query parts 数组。
func appendQueryParts(query string, extra []map[string]any) string {
	var parts []map[string]any
	if err := json.Unmarshal([]byte(query), &parts); err != nil || parts == nil {
		parts = []map[string]any{}
	}
	return string(mustJSON(append(parts, extra...)))
}

func mustJSON(v any) []byte {
	payload, err := json.Marshal(v)
	if err != nil {
		return []byte("[]")
	}
	return payload
}

type createSessionBody struct {
	Mode              string         `json:"mode"`
	EnvironmentID     string         `json:"environment_id"`
	InitialMessage    initialMessage `json:"initial_message"`
	Env               string         `json:"env"`
	AutoCreateProject bool           `json:"auto_create_project"`
	Origin            string         `json:"origin"`
}

// SessionHandle 是建会话成功的返回。
type SessionHandle struct {
	SessionID string
	MessageID string
}

// CreateSession 建立 Remote 会话（两步协议第一步）。显式钉住 agent_type/
// agent_id 并回带完整 descriptor，防止上游静默换默认模型。
func (c *Client) CreateSession(ctx context.Context, token string, sr SessionRequest) (*SessionHandle, error) {
	if sr.Descriptor == nil || sr.Descriptor.Raw == nil {
		return nil, fmt.Errorf("trae: model descriptor is required (model=%s)", sr.Model)
	}
	customModel := copyMap(sr.Descriptor.Raw)
	// 回带的 config_name 以请求模型为准（绑定校验按它精确匹配，大小写敏感）。
	customModel["config_name"] = sr.Model
	if sr.ReasoningEffort != "" {
		if _, hasLevel := customModel["reasoning_effort_level"]; hasLevel {
			customModel["reasoning_effort_level"] = sr.ReasoningEffort
		} else {
			customModel["reasoning_effort"] = sr.ReasoningEffort
		}
	}
	query := sr.Query
	if len(sr.ExtraQueryParts) > 0 {
		query = appendQueryParts(query, sr.ExtraQueryParts)
	}
	message := initialMessage{
		ChatSessionID:          "",
		Content:                []any{},
		Query:                  query,
		ModelName:              sr.Model,
		AgentType:              agentTypeAgent,
		AgentID:                agentTypeAgent,
		ModelSelectionStrategy: "manual",
		CommonParams:           sr.CommonParams,
		ModelConfigSource:      sr.Descriptor.ConfigSource,
		ModelIsPreset:          sr.Descriptor.IsPreset,
		ModelProvider:          sr.Descriptor.Provider,
		CustomModel:            customModel,
	}
	if len(sr.MaxModeFields) > 0 {
		message.ExtraFields = map[string]any{}
		for key, value := range sr.MaxModeFields {
			switch key {
			case "model_selection_strategy":
				message.ModelSelectionStrategy = value.(string)
			default:
				message.ExtraFields[key] = value
			}
		}
	}
	body := createSessionBody{
		Mode:              RemoteSessionModeAgent,
		EnvironmentID:     "default",
		InitialMessage:    message,
		Env:               "remote",
		AutoCreateProject: false,
		Origin:            "web",
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.remoteBase()+sessionsPath, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(req, token, false)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: create session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := readLimited(resp.Body, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("trae: create session read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return nil, ClassifyUpstreamError(
			strconv.Itoa(resp.StatusCode),
			fmt.Sprintf("create_session [%d]: %s", resp.StatusCode, truncate(raw, 300)),
			false,
		)
	}
	var parsed struct {
		Code any `json:"code"`
		Data struct {
			ChatSessionID any `json:"chat_session_id"`
			MessageID     any `json:"message_id"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return nil, fmt.Errorf("trae: create session parse response: %w", err)
	}
	if parsed.Code != nil && !isEmptyCode(parsed.Code) && (parsed.Data.ChatSessionID == nil || parsed.Data.MessageID == nil) {
		code, message := fmt.Sprintf("%v", parsed.Code), raw
		return nil, ClassifyUpstreamError(code, fmt.Sprintf("create_session: %s", truncate(message, 300)), false)
	}
	sessionID := anyToString(parsed.Data.ChatSessionID)
	messageID := anyToString(parsed.Data.MessageID)
	if sessionID == "" || messageID == "" {
		return nil, fmt.Errorf("trae: create session missing ids: %s", truncate(raw, 300))
	}
	return &SessionHandle{SessionID: sessionID, MessageID: messageID}, nil
}

// Event 是归一化后的 Remote SSE 事件（name 已按别名表归一）。
type Event struct {
	Name string
	Data map[string]any
}

// StreamEvents 读取会话事件流（两步协议第二步）。返回的事件 channel 由
// 调用方消费；errC 收到终态错误后两个 channel 都关闭。SSE 解析规则：
// event:/data: 行、data: [DONE] 即 done、注释行忽略、遗留 buffer 的尾帧
// 不丢弃（上游可能不发结尾空行）。
func (c *Client) StreamEvents(ctx context.Context, token, sessionID, messageID string) (<-chan Event, <-chan error) {
	events := make(chan Event, 16)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		url := c.remoteBase() + fmt.Sprintf(eventsPathFmt, sessionID) + "?reply_to_message_id=" + messageID
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			errs <- err
			return
		}
		c.setRemoteHeaders(req, token, true)
		// 事件流不能吃公共 60s 总超时：复用 transport 但去掉 Timeout，
		// 生命周期由 ctx 控制。
		streamClient := &http.Client{Transport: c.httpClient().Transport, Timeout: 0}
		resp, err := streamClient.Do(req)
		if err != nil {
			errs <- fmt.Errorf("trae: stream events: %w", err)
			return
		}
		defer func() {
			_ = resp.Body.Close()
		}()
		if resp.StatusCode >= 400 {
			raw, _ := readLimited(resp.Body, 1<<20)
			errs <- ClassifyUpstreamError(
				strconv.Itoa(resp.StatusCode),
				fmt.Sprintf("events [%d]: %s", resp.StatusCode, truncate(raw, 300)),
				false,
			)
			return
		}
		reader := bufio.NewReader(resp.Body)
		var (
			eventName string
			dataLines []string
		)
		flush := func(final bool) {
			defer func() {
				eventName = ""
				dataLines = nil
			}()
			payload := strings.Join(dataLines, "\n")
			payload = strings.TrimSpace(payload)
			if payload == "[DONE]" {
				select {
				case events <- Event{Name: EventDone, Data: map[string]any{}}:
				case <-ctx.Done():
				}
				return
			}
			if payload == "" && eventName == "" {
				return
			}
			data := map[string]any{}
			if payload != "" {
				if err := json.Unmarshal([]byte(payload), &data); err != nil {
					data = map[string]any{"_raw": payload}
				}
			}
			name := eventName
			if name == "" {
				name = firstStringValue([]map[string]any{data}, "event", "event_type", "eventType", "type")
			}
			if name == "" {
				if !final {
					return
				}
				name = EventMessage
			}
			select {
			case events <- Event{Name: NormalizeEventName(name), Data: data}:
			case <-ctx.Done():
			}
		}
		for {
			line, err := reader.ReadString('\n')
			if len(line) > 0 || err == nil {
				trimmed := strings.TrimRight(line, "\r\n")
				switch {
				case strings.HasPrefix(trimmed, ":"):
					// SSE 注释（心跳）忽略；协议心跳走显式 heartbeat 事件。
				case strings.HasPrefix(trimmed, "event:"):
					eventName = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
				case strings.HasPrefix(trimmed, "data:"):
					dataLines = append(dataLines, strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
				case trimmed == "":
					flush(false)
				}
			}
			if err != nil {
				// EOF：遗留 buffer 的尾帧按 final 处理（上游可能省略结尾空行）。
				if len(dataLines) > 0 || eventName != "" {
					flush(true)
				}
				return
			}
		}
	}()
	return events, errs
}

// StopSession 终止会话（两步协议收尾）。尽力而为：失败仅记录，不影响主流程。
func (c *Client) StopSession(ctx context.Context, token, sessionID string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.remoteBase()+fmt.Sprintf(stopPathFmt, sessionID), bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	c.setRemoteHeaders(req, token, false)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("trae: stop session: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = readLimited(resp.Body, 1<<16)
	if resp.StatusCode >= 400 {
		return fmt.Errorf("trae: stop session [%d]", resp.StatusCode)
	}
	return nil
}

func readLimited(body io.Reader, limit int64) (string, error) {
	raw, err := io.ReadAll(io.LimitReader(body, limit))
	return string(raw), err
}

func copyMap(src map[string]any) map[string]any {
	out := make(map[string]any, len(src))
	for k, v := range src {
		out[k] = v
	}
	return out
}

func isEmptyCode(code any) bool {
	switch v := code.(type) {
	case nil:
		return true
	case float64:
		return v == 0
	case string:
		return v == "" || v == "0"
	default:
		return false
	}
}

func anyToString(v any) string {
	switch value := v.(type) {
	case nil:
		return ""
	case string:
		return value
	case float64:
		if value == float64(int64(value)) {
			return strconv.FormatInt(int64(value), 10)
		}
		return strconv.FormatFloat(value, 'f', -1, 64)
	default:
		return fmt.Sprintf("%v", value)
	}
}
