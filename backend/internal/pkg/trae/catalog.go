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
