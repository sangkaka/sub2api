//go:build unit || probe

package trae

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// 图片块 wire 透传（2026-10-31 实证：上游只收 OpenAI 形 image_url 对象块）。
func TestIDEContentBlocks_ImagePassthrough(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"字符串保持单text块", `"你好"`, `[{"type":"text","text":"你好"}]`},
		{"纯文本块压平兼容", `[{"type":"text","text":"a"},{"type":"text","text":"b"}]`, `[{"type":"text","text":"ab"}]`},
		{"OpenAI对象形透传", `[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]`,
			`[{"type":"text","text":"看图"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AA"}}]`},
		{"OpenAI字符串形归一", `[{"type":"image_url","image_url":"data:image/png;base64,BB"}]`,
			`[{"type":"image_url","image_url":{"url":"data:image/png;base64,BB"}}]`},
		{"Anthropic base64转dataURL", `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"CC"}}]`,
			`[{"type":"image_url","image_url":{"url":"data:image/png;base64,CC"}}]`},
		{"Anthropic url透传", `[{"type":"image","source":{"type":"url","url":"https://x/a.png"}}]`,
			`[{"type":"image_url","image_url":{"url":"https://x/a.png"}}]`},
		{"Responses input_image转换", `[{"type":"input_image","image_url":"data:image/png;base64,DD"}]`,
			`[{"type":"image_url","image_url":{"url":"data:image/png;base64,DD"}}]`},
		{"图文顺序保留", `[{"type":"image_url","image_url":{"url":"data:image/png;base64,E"}},{"type":"text","text":"尾注"}]`,
			`[{"type":"image_url","image_url":{"url":"data:image/png;base64,E"}},{"type":"text","text":"尾注"}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := json.Marshal(ideContentBlocks(json.RawMessage(tc.in)))
			require.NoError(t, err)
			require.JSONEq(t, tc.want, string(got))
		})
	}
}

func TestIDEContentBlocks_EmptyAndInvalid(t *testing.T) {
	require.Nil(t, ideContentBlocks(nil))
	require.Equal(t, []ideContentBlock{{Type: "text", Text: ""}}, ideContentBlocks(json.RawMessage(`""`)))
	// 非法 JSON 走 contentText 兜底（空串单块）
	require.Len(t, ideContentBlocks(json.RawMessage(`{oops`)), 1)
}

func TestModelDescriptorSupportsImage(t *testing.T) {
	cases := []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{"顶层multimodal真", map[string]any{"multimodal": true}, true},
		{"顶层support_read_image假胜出", map[string]any{"support_read_image": false, "multimodal": true}, false},
		{"features为JSON字符串", map[string]any{"features": `{"multimodal":{"enable":true}}`}, true},
		{"features为对象", map[string]any{"features": map[string]any{"multimodal": map[string]any{"enable": false}}}, false},
		{"无标志默认假", map[string]any{"name": "x"}, false},
		{"nil安全", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := &ModelDescriptor{Raw: tc.raw}
			require.Equal(t, tc.want, d.SupportsImage())
		})
	}
}
