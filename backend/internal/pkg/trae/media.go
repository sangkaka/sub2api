package trae

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// 图片输入（PR-3.2）：Remote query 的 parts 数组除文本 part 外可携带 renderer
// media payload（{"type":"image_url","data":{"mimeType","data"}}），让多模态
// 模型直接读到附件而不是把图片描述成文字后丢弃。每个图片 part 后随一条
// 描述性文本 part，非多模态读取方仍知道有附件。

const defaultImageMIME = "image/png"

// imageRendererBlock 是从 OpenAI/Anthropic 图片 part 转换出的 renderer 块。
type imageRendererBlock struct {
	MimeType string
	Data     string
}

// MediaQueryParts 扫描消息里的图片 part，返回追加到 query parts 的媒体载荷。
func MediaQueryParts(messages []ChatMessage) []map[string]any {
	var parts []map[string]any
	for _, m := range messages {
		blocks := parseJSONBlocks(m.Content)
		for _, block := range blocks {
			blockType := strings.ToLower(anyToString(block["type"]))
			if blockType != "image_url" && blockType != "input_image" && blockType != "image" {
				continue
			}
			converted := toRendererBlock(blockType, block)
			if converted == nil {
				continue
			}
			parts = append(parts,
				map[string]any{
					"type": "image_url",
					"data": map[string]any{
						"mimeType": converted.MimeType,
						"data":     converted.Data,
					},
				},
				map[string]any{
					"type": "text",
					"data": map[string]any{"content": describeImage(converted)},
				},
			)
		}
	}
	return parts
}

func toRendererBlock(blockType string, block map[string]any) *imageRendererBlock {
	if blockType == "image" { // Anthropic 块：source.type = base64|url
		source, _ := block["source"].(map[string]any)
		if source == nil {
			return nil
		}
		switch strings.ToLower(anyToString(source["type"])) {
		case "base64":
			data := strings.Join(strings.Fields(anyToString(source["data"])), "")
			if data == "" {
				return nil
			}
			return &imageRendererBlock{MimeType: orDefault(anyToString(source["media_type"]), defaultImageMIME), Data: data}
		case "url":
			if url := anyToString(source["url"]); url != "" {
				return &imageRendererBlock{MimeType: "url", Data: url}
			}
		}
		return nil
	}
	// OpenAI 块：image_url.url（或顶层 url）。
	url := ""
	if image, ok := block["image_url"].(map[string]any); ok {
		url = anyToString(image["url"])
	} else if image, ok := block["image_url"].(string); ok {
		url = image
	}
	if url == "" {
		url = anyToString(block["url"])
	}
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		mime, data := splitDataURL(url)
		if data == "" {
			return nil
		}
		return &imageRendererBlock{MimeType: orDefault(mime, defaultImageMIME), Data: data}
	}
	// 远端 URL 无法内联，原样转发优于静默丢弃。
	return &imageRendererBlock{MimeType: "url", Data: url}
}

func describeImage(block *imageRendererBlock) string {
	if block.MimeType == "url" || strings.HasPrefix(block.Data, "http") {
		return "[image: " + truncate(block.Data, 120) + "]"
	}
	size := base64.StdEncoding.DecodedLen(len(block.Data))
	if size > 0 {
		return fmt.Sprintf("[image: %s, %d bytes]", block.MimeType, size)
	}
	return "[image: " + block.MimeType + "]"
}

func splitDataURL(url string) (mime, data string) {
	if !strings.HasPrefix(url, "data:") {
		return "", url
	}
	rest := strings.TrimPrefix(url, "data:")
	if idx := strings.Index(rest, ","); idx >= 0 {
		meta := rest[:idx]
		data = rest[idx+1:]
		if i := strings.Index(meta, ";"); i >= 0 {
			mime = meta[:i]
		} else {
			mime = meta
		}
	}
	return mime, data
}

func orDefault(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func parseJSONBlocks(raw json.RawMessage) []map[string]any {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var blocks []map[string]any
	if err := json.Unmarshal(raw, &blocks); err != nil {
		return nil
	}
	return blocks
}
