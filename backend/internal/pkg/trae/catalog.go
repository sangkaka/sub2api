package trae

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
)

// ModelDescriptor 是 /models 目录中的模型条目。config_name 是请求用的模型
// ID；手动指定模型时必须把整个 descriptor 作为 custom_model 回带，否则上游
// 会静默选用其默认模型。
type ModelDescriptor struct {
	ConfigName string `json:"config_name"`
	// RawChatFunction 标识模型所属执行器（solo_agent_remote / solo_work_remote）。
	RawChatFunction string `json:"raw_chat_function"`
	Provider        string `json:"provider"`
	ConfigSource    int    `json:"config_source"`
	IsPreset        bool   `json:"is_preset"`
	// Raw 保留完整原始条目，回带 custom_model 时原样使用（descriptor 字段
	// 上游会随版本增减，白名单字段仅服务本包逻辑）。
	Raw map[string]any `json:"-"`
}

// ReasoningEfforts 返回 descriptor 声明支持的思考强度档位；空表示该模型
// 不暴露思考强度配置。
func (m *ModelDescriptor) ReasoningEfforts() []string {
	config, _ := m.Raw["reasoning_effort_config"].(map[string]any)
	if config == nil {
		return nil
	}
	levels, _ := config["reasoning_effort_levels"].([]any)
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		if s, ok := level.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

// catalogResponse 是 /models 响应：data.list[] 按 function 分组，每组 models[]
// 是模型 descriptor。
type catalogResponse struct {
	Data struct {
		List []struct {
			Function string           `json:"function"`
			Models   []map[string]any `json:"models"`
		} `json:"list"`
	} `json:"data"`
}

// FetchModels 拉取 Solo 系（agent/work/design）模型目录。仅保留
// solo_agent_remote 档位的模型（v1 只服务 Agent 会话）。
func (c *Client) FetchModels(ctx context.Context, token string) ([]*ModelDescriptor, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("trae: token is empty")
	}
	url := c.remoteBase() + modelsPath + "?functions=solo_agent_remote%2Csolo_work_remote%2Csolo_design_remote&show_custom_model=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(req, token, false)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: fetch models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("trae: fetch models read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			Kind:    classifyHTTPError(resp.StatusCode),
			Code:    fmt.Sprintf("%d", resp.StatusCode),
			Message: fmt.Sprintf("models [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var parsed catalogResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("trae: fetch models parse response: %w", err)
	}
	out := make([]*ModelDescriptor, 0, 16)
	seen := map[string]bool{}
	for _, group := range parsed.Data.List {
		for _, entry := range group.Models {
			descriptor := parseModelDescriptor(entry)
			if descriptor == nil || seen[descriptor.ConfigName] {
				continue
			}
			seen[descriptor.ConfigName] = true
			out = append(out, descriptor)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ConfigName < out[j].ConfigName })
	return out, nil
}

// parseModelDescriptor 解析单个模型条目。config_name 兼容 name/configName/
// config_name 三种键（不同上游版本的目录形状有差异）。
func parseModelDescriptor(entry map[string]any) *ModelDescriptor {
	if entry == nil {
		return nil
	}
	configName := firstStringValue([]map[string]any{entry}, "config_name", "configName", "name")
	if strings.TrimSpace(configName) == "" {
		return nil
	}
	function, _ := entry["raw_chat_function"].(string)
	if function == "" {
		function, _ = entry["rawChatFunction"].(string)
	}
	provider, _ := entry["provider"].(string)
	configSource := 1
	if v, ok := entry["config_source"].(float64); ok {
		configSource = int(v)
	} else if v, ok := entry["configSource"].(float64); ok {
		configSource = int(v)
	}
	isPreset := true
	if v, ok := entry["is_preset"].(bool); ok {
		isPreset = v
	} else if v, ok := entry["isPreset"].(bool); ok {
		isPreset = v
	}
	return &ModelDescriptor{
		ConfigName:      strings.TrimSpace(configName),
		RawChatFunction: strings.TrimSpace(function),
		Provider:        strings.TrimSpace(provider),
		ConfigSource:    configSource,
		IsPreset:        isPreset,
		Raw:             entry,
	}
}

// StaticFallbackModelIDs 是模型目录不可用时的静态兜底清单（来自
// Trae2api-cn 生产观察的 config_name 集合）。仅用于展示与默认值，绑定校验
// 仍以上游目录为准。
var StaticFallbackModelIDs = []string{
	"GLM-5.3",
	"GLM-5.3-Air",
	"GLM-5.3-FlashX",
	"DeepSeek-V3.2-Exp",
	"DeepSeek-Chat",
	"Kimi-K2-Instruct",
	"claude-sonnet-4-5",
	"claude-opus-4-8",
	"gpt-5.2",
	"gpt-5.2-codex",
	"gemini-3-pro",
	"gemini-3-flash",
	"doubao-seed-1.6",
	"qwen3-coder-plus",
}

// SupportsMaxMode 仅在 descriptor 显式声明 max_mode=true 且三项线上限
// （max context ≥ 1M、prompt_max_tokens、max_tokens）齐备时为真——绝不
// 凭模型名合成 1M/936K/64K 配置（上游服务端默认会重建被禁止的合成档）。
func (m *ModelDescriptor) SupportsMaxMode() bool {
	if m == nil || m.Raw == nil {
		return false
	}
	enabled, _ := m.Raw["max_mode"].(bool)
	if !enabled {
		return false
	}
	maxContext := m.maxContextTokens()
	return maxContext >= 1_000_000 && m.positiveLimit("prompt_max_tokens") > 0 && m.positiveLimit("max_tokens") > 0
}

// MaxModeFields 返回把会话钉在 1M Max 档的线上字段（initial_message 层）。
// 无有效档位时返回 nil。
func (m *ModelDescriptor) MaxModeFields() map[string]any {
	if !m.SupportsMaxMode() {
		return nil
	}
	fields := map[string]any{
		"model_selection_strategy": "max",
	}
	if maxContext := m.maxContextTokens(); maxContext > 0 {
		fields["context_window_size"] = maxContext
	}
	if v := m.positiveLimit("prompt_max_tokens"); v > 0 {
		fields["prompt_max_tokens"] = v
	}
	if v := m.positiveLimit("max_tokens"); v > 0 {
		fields["max_tokens"] = v
	}
	fields["mode_type"] = 1
	return fields
}

func (m *ModelDescriptor) maxContextTokens() int {
	if m == nil || m.Raw == nil {
		return 0
	}
	candidates := []int{}
	if v := m.positiveLimit("context_window_size"); v > 0 {
		candidates = append(candidates, v)
	}
	if v := m.positiveLimit("max_context"); v > 0 {
		candidates = append(candidates, v)
	}
	if tokens, ok := m.Raw["context_window_tokens"].(map[string]any); ok {
		if v := positiveIntFrom(tokens["max"]); v > 0 {
			candidates = append(candidates, v)
		}
	}
	max := 0
	for _, v := range candidates {
		if v > max {
			max = v
		}
	}
	return max
}

func (m *ModelDescriptor) positiveLimit(key string) int {
	if m == nil || m.Raw == nil {
		return 0
	}
	return positiveIntFrom(m.Raw[key])
}

func positiveIntFrom(v any) int {
	switch n := v.(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	case string:
		var parsed int
		if _, err := fmt.Sscanf(strings.TrimSpace(n), "%d", &parsed); err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

// SupportsImage 报告 descriptor 声明的图片输入能力。语义与 Trae2api-cn
// 对齐（2026-10-31）：顶层 support_read_image / multimodal 显式布尔优先
// （false 胜出），其次 features（上游可能给 JSON 字符串或对象）里的
// multimodal.enable。注意能力按 (function, model) 声明且档位不一致——
// 调用方应使用 agent 档（solo_agent_remote）的 descriptor。
func (m *ModelDescriptor) SupportsImage() bool {
	if m == nil || m.Raw == nil {
		return false
	}
	for _, key := range []string{"support_read_image", "multimodal"} {
		if v, ok := m.Raw[key].(bool); ok {
			return v
		}
	}
	var features map[string]any
	switch raw := m.Raw["features"].(type) {
	case map[string]any:
		features = raw
	case string:
		var parsed map[string]any
		if json.Unmarshal([]byte(raw), &parsed) == nil {
			features = parsed
		}
	}
	if features == nil {
		return false
	}
	switch multimodal := features["multimodal"].(type) {
	case map[string]any:
		if v, ok := multimodal["enable"].(bool); ok {
			return v
		}
	case bool:
		return multimodal
	}
	return false
}

// FetchAgentModels 只拉 agent 档（solo_agent_remote）目录。图片能力判定
// 必须用这一档：上游按档位声明 multimodal，work 档普遍比 agent 档保守
// （glm-5.3-flash work=false / agent=true，2026-10-31 实测），多档合并
// 首见优先会拿 work 档的 false 误杀。
func (c *Client) FetchAgentModels(ctx context.Context, token string) ([]*ModelDescriptor, error) {
	url := c.remoteBase() + modelsPath + "?functions=solo_agent_remote&show_custom_model=true"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	c.setRemoteHeaders(req, token, false)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("trae: fetch agent models: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("trae: fetch agent models read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &UpstreamError{
			Kind:    classifyHTTPError(resp.StatusCode),
			Code:    fmt.Sprintf("%d", resp.StatusCode),
			Message: fmt.Sprintf("agent models [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var parsed catalogResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil, fmt.Errorf("trae: fetch agent models parse: %w", err)
	}
	out := make([]*ModelDescriptor, 0, 16)
	for _, group := range parsed.Data.List {
		for _, entry := range group.Models {
			if descriptor := parseModelDescriptor(entry); descriptor != nil {
				out = append(out, descriptor)
			}
		}
	}
	return out, nil
}
