// TraeCode v2 裸通道：POST /api/ide/v2/llm_raw_chat（function=chat_v3）。
//
// 与 llm_utils_chat（TraeWork 0.1 旧栈）的差异（2026-10-30 企业账号实测）：
//   - 模型绑定必须用 get_detail_param(function=chat_v3) 目录里
//     model_detail_list[].model_name（__dev/__max 后缀 provider id），裸模型
//     名一律 4023 model unknown；config_source 同目录。
//   - 头部需 X-App-Function/X-Ide-Function 携带函数名 + X-Ide-Token +
//     x-plugin-channel: desktop + 3.4.x 版本头，缺失即 400 空体。
//   - 工具 parameters 同为 SOLO 字符串化编码；不能带 tool_choice（400）；
//     不下发 max_tokens。
//   - 事件流与旧端点同族 DTO（output/token_usage/done，帧内
//     response/reasoning_content/tool_calls），IDETranslator 直接复用；
//     历史消息 function_call 键与原生 function 键均被接受。
//   - 长参数在 v2 完整（实测 4662 字符 / 443 帧分片流），旧端点会被上游
//     格式化器捞成 "{}"。
package trae

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const (
	rawChatV2Path   = "/api/ide/v2/llm_raw_chat"
	detailParamPath = "/api/ide/v1/get_detail_param"
	// ChatV3Function 是 chat_v3 目录模型（native_function_call:true）的执行器。
	ChatV3Function = "chat_v3"
	// v2 版本头：当前桌面 IDE（TraeCode）版本族。
	v2IDEVersion  = "3.4.4"
	v2VersionCode = "20260212"
)

// RawChatV2Request 是一次 llm_raw_chat 回合的输入。Messages/Tools 与
// llm_utils_chat 共用编码；ConfigName/ModelName/ConfigSource 来自目录绑定。
type RawChatV2Request struct {
	Messages        []ChatMessage
	ConfigName      string
	ModelName       string
	ConfigSource    int
	UserID          string
	Tools           []ToolDefinition
	ReasoningEffort string
}

// V2ModelBinding 是 chat_v3 目录解析出的模型绑定。
type V2ModelBinding struct {
	ConfigName   string
	ModelName    string
	ConfigSource int
}

// FetchV2ModelBindings 拉取 chat_v3 目录并解析每个 config 的 provider 绑定
// （model_detail_list 优先 __dev 后缀项）。键为 config_name（小写）。
func (c *Client) FetchV2ModelBindings(ctx context.Context, token, userID string) (map[string]V2ModelBinding, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("trae: token is empty")
	}
	body := map[string]any{
		"function":            ChatV3Function,
		"config_names":        nil,
		"need_prompt":         false,
		"current_config_info": nil,
		"poly_prompt":         true,
		"mode_type":           nil,
		"agent_type":          nil,
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cloudIDEHost()+detailParamPath, bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	c.setIDEHeadersV2(req, token, userID, ChatV3Function)
	req.Header.Set("Accept", "application/json")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: fetch v2 catalog: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	payload, err := readLimited(resp.Body, 4<<20)
	if err != nil {
		return nil, fmt.Errorf("trae: fetch v2 catalog read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			Kind:    classifyHTTPError(resp.StatusCode),
			Code:    fmt.Sprintf("%d", resp.StatusCode),
			Message: fmt.Sprintf("get_detail_param [%d]: %s", resp.StatusCode, truncate(payload, 300)),
		}
	}
	var parsed struct {
		ConfigInfoList []map[string]any `json:"config_info_list"`
		Data           struct {
			ConfigInfoList []map[string]any `json:"config_info_list"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(payload), &parsed); err != nil {
		return nil, fmt.Errorf("trae: fetch v2 catalog parse response: %w", err)
	}
	entries := parsed.ConfigInfoList
	if len(entries) == 0 {
		entries = parsed.Data.ConfigInfoList
	}
	out := make(map[string]V2ModelBinding, len(entries))
	for _, entry := range entries {
		configName, _ := entry["config_name"].(string)
		if configName == "" {
			continue
		}
		source := 1
		if v, ok := entry["config_source"].(float64); ok && int(v) > 0 {
			source = int(v)
		}
		details, _ := entry["model_detail_list"].([]any)
		modelName := pickV2DetailModel(details)
		if modelName == "" {
			continue
		}
		out[strings.ToLower(configName)] = V2ModelBinding{
			ConfigName:   configName,
			ModelName:    modelName,
			ConfigSource: source,
		}
	}
	return out, nil
}

// pickV2DetailModel 从 model_detail_list 选 provider id：优先非 __max 的
// __dev 项，退化取首个带 model_name 的项。
func pickV2DetailModel(details []any) string {
	first := ""
	for _, item := range details {
		entry, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name, _ := entry["model_name"].(string)
		if name == "" {
			continue
		}
		if first == "" {
			first = name
		}
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, "__dev") {
			return name
		}
	}
	return first
}

// RawChatV2 发起一次 llm_raw_chat 回合并返回归一化事件流（与 IDEChat 同族，
// 复用 readSSEEvents 与 IDETranslator）。
func (c *Client) RawChatV2(ctx context.Context, token string, req RawChatV2Request) (<-chan Event, <-chan error) {
	events := make(chan Event, 8)
	errs := make(chan error, 1)
	go func() {
		defer close(events)
		defer close(errs)
		body, err := buildRawChatV2Body(req)
		if err != nil {
			errs <- err
			return
		}
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.cloudIDEHost()+rawChatV2Path, bytes.NewReader(body))
		if err != nil {
			errs <- err
			return
		}
		c.setIDEHeadersV2(httpReq, token, req.UserID, ChatV3Function)
		streamClient := &http.Client{Transport: c.httpClient().Transport, Timeout: 0}
		resp, err := streamClient.Do(httpReq)
		if err != nil {
			errs <- fmt.Errorf("trae: raw chat v2: %w", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode >= 400 {
			raw, _ := readLimited(resp.Body, 1<<20)
			errs <- ClassifyUpstreamError(
				fmt.Sprintf("%d", resp.StatusCode),
				fmt.Sprintf("llm_raw_chat [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
				false,
			)
			return
		}
		readSSEEvents(ctx, resp.Body, events)
	}()
	return events, errs
}

func buildRawChatV2Body(req RawChatV2Request) ([]byte, error) {
	if len(req.Messages) == 0 {
		return nil, fmt.Errorf("trae: raw chat v2 messages cannot be empty")
	}
	if strings.TrimSpace(req.ConfigName) == "" || strings.TrimSpace(req.ModelName) == "" {
		return nil, fmt.Errorf("trae: raw chat v2 model binding is empty")
	}
	session := randomID()
	body := map[string]any{
		"config_name":       req.ConfigName,
		"model_name":        req.ModelName,
		"conversation_id":   session,
		"session_id":        session,
		"stream":            true,
		"raw_chat_function": ChatV3Function,
		"config_source":     req.ConfigSource,
		"messages":          toIDEWireMessages(req.Messages),
	}
	if req.ReasoningEffort != "" {
		body["reasoning_effort"] = req.ReasoningEffort
	}
	if len(req.Tools) > 0 {
		// parameters 保持 SOLO 字符串化（对象形实测 400）。
		body["tools"] = ideToolDefinitions(req.Tools)
	}
	return json.Marshal(body)
}

// setIDEHeadersV2 在 TraeWork 头部基础上叠加 v2 必需项：函数名头、
// X-Ide-Token、desktop 通道与 3.4.x 版本族。
func (c *Client) setIDEHeadersV2(req *http.Request, token, userID, fn string) {
	c.setIDEHeaders(req, token, userID)
	h := req.Header
	h.Set("X-Ide-Token", token)
	h.Set("X-App-Function", fn)
	h.Set("X-Ide-Function", fn)
	h.Set("x-ide-version", v2IDEVersion)
	h.Set("x-ide-version-code", v2VersionCode)
	h.Set("x-app-version-code", v2VersionCode)
	h.Set("x-version-code", v2VersionCode)
	h.Set("app-version", v2IDEVersion)
	h.Set("x-plugin-channel", "desktop")
	h.Set("User-Agent", "Trae/"+v2IDEVersion)
}
