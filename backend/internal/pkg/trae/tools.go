package trae

import (
	"encoding/json"
	"fmt"
	"strings"
)

// 工具调用文本桥（PR-3.1，Remote 版）：Remote 协议没有原生 tools 字段，
// 客户端工具经两条通路桥接：
//  1. 请求侧：把工具定义注入 query 的运行时提示，规定模型以
//     <opencode_tool_call>{"name":...,"input":{}}</opencode_tool_call> 文本块
//     发起调用（不带 id，由本网关分配）；
//  2. 响应侧：ToolCallScanner 从正文增量流中扫描该标记，转成 OpenAI
//     delta.tool_calls 并从可见文本中剔除。
// 历史侧：assistant.tool_calls 序列化为「已处理勿重复」清单，tool 角色消息
// 以 [Client Tool Result] 标记（FlattenMessages 已实现）。

const toolCallMarker = "<opencode_tool_call>"

// ToolDefinition 是入站 tools 数组条目的最小形状（function.name/description/
// parameters 保留原文）。
type ToolDefinition struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

// BuildToolRuntimePrompt 组装注入 query 的客户端工具运行时提示。
func BuildToolRuntimePrompt(tools []ToolDefinition, toolChoice string, parallelToolCalls *bool) string {
	if len(tools) == 0 {
		return ""
	}
	type functionShape struct {
		Name        string          `json:"name"`
		Description string          `json:"description,omitempty"`
		Parameters  json.RawMessage `json:"parameters,omitempty"`
	}
	definitions := make([]functionShape, 0, len(tools))
	for _, tool := range tools {
		var fn functionShape
		if len(tool.Function) > 0 {
			if err := json.Unmarshal(tool.Function, &fn); err != nil {
				continue
			}
		}
		if fn.Name == "" {
			continue
		}
		definitions = append(definitions, fn)
	}
	if len(definitions) == 0 {
		return ""
	}
	payload, err := json.Marshal(definitions)
	if err != nil {
		return ""
	}
	lines := []string{
		"Client tools run on the API caller's machine, not on the server.",
		"Client tools are NOT native/server functions: never invoke them through function calling or any built-in tool, and never fall back to server tools (LS, Glob, Read, terminal) to reach the caller's files.",
		fmt.Sprintf("To call a tool, emit one block per call in this format and no prose or final answer in the same turn: %s%s%s. Omit the id field (the client assigns it), use an exact declared tool name, and fill input according to that tool's schema.", toolCallMarker, `{"name":"tool_name","input":{}}`, "</opencode_tool_call>"),
		"After the client returns a tool result, continue from that result until the user's request is complete.",
		"Never claim that a client file was downloaded, created, edited or saved unless a later matching client tool result with the same call id confirms success.",
		"Never repeat a completed tool call.",
		"Available client tool definitions and input schemas (JSON):",
		string(payload),
	}
	switch strings.TrimSpace(toolChoice) {
	case "none":
		lines = append(lines, "Tool choice is none: do not request a tool.")
	case "required":
		lines = append(lines, "Tool choice is required: request an available client tool before answering.")
	}
	if parallelToolCalls != nil && !*parallelToolCalls {
		lines = append(lines, "Request at most one tool per turn.")
	}
	return strings.Join(lines, "\n")
}

// SerializeAssistantToolCalls 把 assistant 历史的 tool_calls 序列化为
// 「已处理勿重复」文本（调用方拼进 [Assistant] 段）。
func SerializeAssistantToolCalls(raw json.RawMessage) string {
	if len(raw) == 0 || strings.TrimSpace(string(raw)) == "null" {
		return ""
	}
	var calls []struct {
		ID         string `json:"id"`
		ToolCallID string `json:"tool_call_id"`
		Function   struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		} `json:"function"`
	}
	if err := json.Unmarshal(raw, &calls); err != nil || len(calls) == 0 {
		return ""
	}
	records := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		id := call.ID
		if id == "" {
			id = call.ToolCallID
		}
		input := call.Function.Arguments
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		var parsed any
		if err := json.Unmarshal(input, &parsed); err != nil {
			parsed = string(input)
		}
		records = append(records, map[string]any{
			"id": id, "name": call.Function.Name, "input": parsed,
		})
	}
	payload, err := json.Marshal(records)
	if err != nil {
		return ""
	}
	return "Client tool history (already handled; do not repeat):\n" + string(payload)
}

// ToolCall 是扫描出的单次调用（id 由本端分配）。
type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// ToolCallScanner 是有状态的正文流扫描器：透传标记外的文本，缓冲可能是
// 标记前缀的尾巴，闭合标记解析为 ToolCall。非并发安全，每会话一个。
type ToolCallScanner struct {
	pending   string // 尚未判定为「非标记文本」的尾部（可能含半截标记）
	callIndex int
	all       []ToolCall
}

// Calls 返回扫描器累计产出的全部调用（发射序）。
func (s *ToolCallScanner) Calls() []ToolCall {
	return s.all
}

// Feed 输入一段正文增量，返回（可安全透出的文本, 扫描完成的调用）。
func (s *ToolCallScanner) Feed(delta string) (string, []ToolCall) {
	buffer := s.pending + delta
	s.pending = ""
	var (
		out   strings.Builder
		calls []ToolCall
	)
	for {
		open := strings.Index(buffer, toolCallMarker)
		if open < 0 {
			// 无标记开头：保留可能是标记前缀的尾巴，其余透出。
			safe := len(buffer) - maxMarkerPrefixLen(buffer)
			if safe > 0 {
				out.WriteString(buffer[:safe])
				buffer = buffer[safe:]
			}
			s.pending = buffer
			return out.String(), calls
		}
		// 标记前的文本透出（同样要防半截前缀？标记已完整出现，前面全部安全）。
		out.WriteString(buffer[:open])
		rest := buffer[open+len(toolCallMarker):]
		close := strings.Index(rest, "</opencode_tool_call>")
		if close < 0 {
			// 标记未闭合：整段缓冲等待后续增量。
			s.pending = buffer[open:]
			return out.String(), calls
		}
		if call, ok := parseToolCall(rest[:close], &s.callIndex); ok {
			calls = append(calls, call)
			s.all = append(s.all, call)
		}
		buffer = rest[close+len("</opencode_tool_call>"):]
	}
}

// Flush 收尾：未闭合的标记按原文透出（视为普通文本）。
func (s *ToolCallScanner) Flush() string {
	pending := s.pending
	s.pending = ""
	return pending
}

func parseToolCall(payload string, index *int) (ToolCall, bool) {
	var parsed struct {
		Name  string          `json:"name"`
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload)), &parsed); err != nil || parsed.Name == "" {
		return ToolCall{}, false
	}
	arguments := strings.TrimSpace(string(parsed.Input))
	if arguments == "" || arguments == "null" {
		arguments = "{}"
	}
	*index++
	return ToolCall{
		ID:        fmt.Sprintf("call_trae_%d", *index),
		Name:      parsed.Name,
		Arguments: arguments,
	}, true
}

// maxMarkerPrefixLen 返回 buffer 尾部里可能是标记前缀的最长长度。
func maxMarkerPrefixLen(buffer string) int {
	max := 0
	for i := 1; i <= len(toolCallMarker) && i <= len(buffer); i++ {
		if strings.HasPrefix(toolCallMarker, buffer[len(buffer)-i:]) {
			max = i
		}
	}
	return max
}
