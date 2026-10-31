//go:build probe

// 手动探测（不进 CI）：TRAE IDE llm_utils_chat / v2 llm_raw_chat 是否接受
// 图片内容块、多模态模型能否真读到图。运行：
//
//	TRAE_PROBE_TOKEN=... TRAE_PROBE_UID=... \
//	TRAE_PROBE_HOST=https://console.enterprise.trae.cn \
//	TRAE_PROBE_GATEWAY=https://trae-api-cn.mchost.guru \
//	TRAE_PROBE_IMG=<纯 base64 PNG（红色实心图）> \
//	go test -tags probe ./internal/pkg/trae/ -run TestProbeImageSupport -v -count=1
package trae

import (
	"bufio"
	"sort"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"
)

func probeSend(t *testing.T, host, path string, v2 bool, fn, model string, extra map[string]any, content []any) {
	c := &Client{CloudIDEHost: host}
	session := randomID()
	body := map[string]any{
		"stream":     true,
		"request_id": session,
		"session_id": session,
		"messages":   []map[string]any{{"role": "user", "content": content}},
	}
	if v2 {
		body["config_name"] = model
		body["model_name"] = model
		body["conversation_id"] = session
		body["raw_chat_function"] = fn
		body["config_source"] = 1
	} else {
		body["config_name"] = model
		body["model"] = model
		body["function"] = fn
	}
	for k, v := range extra {
		body[k] = v
	}
	raw, _ := jsonMarshalProbe(body)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, host+path, bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if v2 {
		c.setIDEHeadersV2(req, os.Getenv("TRAE_PROBE_TOKEN"), os.Getenv("TRAE_PROBE_UID"), fn)
	} else {
		c.setIDEHeaders(req, os.Getenv("TRAE_PROBE_TOKEN"), os.Getenv("TRAE_PROBE_UID"))
	}
	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		sc := bufio.NewScanner(resp.Body)
		var sb strings.Builder
		for sc.Scan() && sb.Len() < 400 {
			sb.WriteString(sc.Text())
		}
		t.Logf("HTTP %d: %s", resp.StatusCode, sb.String())
		return
	}
	// 收集 SSE data 行里的最终回答
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	deadline := time.Now().Add(120 * time.Second)
	final := ""
	for sc.Scan() && time.Now().Before(deadline) {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if i := strings.Index(payload, `"response":"`); i >= 0 {
			seg := payload[i+len(`"response":"`):]
			if j := strings.Index(seg, `"`); j >= 0 {
				final += seg[:j]
			}
		}
		if strings.Contains(payload, "token_usage") || strings.Contains(payload, "[DONE]") {
			break
		}
	}
	t.Logf("最终回答: %q", truncateProbe(final, 120))
}

func minProbe(a, b int) int { if a < b { return a }; return b }

func truncateProbe(s string, n int) string { if len(s) > n { return s[:n] + "…" }; return s }

func jsonMarshalProbe(v any) ([]byte, error) { return json.Marshal(v) }

func TestProbeImageRound2(t *testing.T) {
	token := os.Getenv("TRAE_PROBE_TOKEN")
	img := os.Getenv("TRAE_PROBE_IMG2")
	if token == "" || img == "" {
		t.Skip("需要 TRAE_PROBE_TOKEN / TRAE_PROBE_IMG2")
	}
	host := envOr("TRAE_PROBE_HOST", "https://console.enterprise.trae.cn")
	gw := envOr("TRAE_PROBE_GATEWAY", DefaultModelGatewayHost)
	textBlock := map[string]any{"type": "text", "text": "图片中央的大方块是什么颜色？只答一个词"}
	openaiImg := map[string]any{"type": "image_url", "image_url": map[string]any{
		"url": "data:image/png;base64," + img}}

	t.Run("ide-glm5.3-大图", func(t *testing.T) {
		probeSend(t, host, ideChatPath, false, ideFunction, "glm-5.3", nil, []any{openaiImg, textBlock})
	})
	t.Run("gw-deepseek-大图", func(t *testing.T) {
		probeSend(t, gw, ideChatPath, false, ideFunction, "deepseek-v4.1-flash", nil, []any{openaiImg, textBlock})
	})
	t.Run("ide-kimi-大图", func(t *testing.T) {
		probeSend(t, host, ideChatPath, false, ideFunction, "kimi-k3.1-pro", nil, []any{openaiImg, textBlock})
	})
	// v2：先取真实 binding
	c := &Client{CloudIDEHost: host, ModelGatewayHost: gw}
	bindings, err := c.FetchV2ModelBindings(context.Background(), token, os.Getenv("TRAE_PROBE_UID"))
	if err != nil {
		t.Logf("v2 bindings 拉取失败: %v", err)
	} else {
		for name, b := range bindings {
			t.Logf("v2 binding: %s -> %+v", name, b)
			t.Run("v2-"+name+"-大图", func(t *testing.T) {
				body := map[string]any{"config_name": b.ConfigName, "model_name": b.ModelName,
					"config_source": b.ConfigSource}
				_ = body
				probeSendV2(t, host, b, []any{openaiImg, textBlock})
			})
			break // 只测第一个
		}
	}
}

func probeSendV2(t *testing.T, host string, b V2ModelBinding, content []any) {
	c := &Client{CloudIDEHost: host}
	session := randomID()
	body := map[string]any{
		"config_name": b.ConfigName, "model_name": b.ModelName,
		"conversation_id": session, "session_id": session,
		"stream": true, "raw_chat_function": ChatV3Function, "config_source": b.ConfigSource,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	raw, _ := jsonMarshalProbe(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, host+rawChatV2Path, bytes.NewReader(raw))
	c.setIDEHeadersV2(req, os.Getenv("TRAE_PROBE_TOKEN"), os.Getenv("TRAE_PROBE_UID"), ChatV3Function)
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	var out strings.Builder
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		for _, key := range []string{`"response"`, `"content"`, `"reasoning_content"`, `"message"`, `"error"`} {
			if i := strings.Index(payload, key); i >= 0 {
				out.WriteString(payload[i:minProbe(i+140, len(payload))])
				out.WriteString(" │ ")
				break
			}
		}
		if strings.Contains(payload, "token_usage") || strings.Contains(payload, "[DONE]") {
			break
		}
	}
	t.Logf("HTTP %d 输出: %s", resp.StatusCode, truncateProbe(out.String(), 600))
}

func TestProbeImageRound3(t *testing.T) {
	token := os.Getenv("TRAE_PROBE_TOKEN")
	img := os.Getenv("TRAE_PROBE_IMG2")
	if token == "" || img == "" {
		t.Skip("需要 TRAE_PROBE_TOKEN / TRAE_PROBE_IMG2")
	}
	host := envOr("TRAE_PROBE_HOST", "https://console.enterprise.trae.cn")
	c := &Client{CloudIDEHost: host}
	bindings, err := c.FetchV2ModelBindings(context.Background(), token, os.Getenv("TRAE_PROBE_UID"))
	if err != nil {
		t.Fatalf("bindings: %v", err)
	}
	textBlock := map[string]any{"type": "text", "text": "图片中央的大方块是什么颜色？只答一个词"}
	openaiImg := map[string]any{"type": "image_url", "image_url": map[string]any{
		"url": "data:image/png;base64," + img}}
	names := make([]string, 0, len(bindings))
	for n := range bindings {
		names = append(names, n)
	}
	sort.Strings(names)
	t.Logf("v2 目录共 %d 个绑定: %v", len(names), names)
	for _, n := range names {
		b := bindings[n]
		t.Run("v2-"+n+"-"+b.ModelName, func(t *testing.T) {
			probeSendV2Full(t, host, b, []any{openaiImg, textBlock})
		})
	}
}

func probeSendV2Full(t *testing.T, host string, b V2ModelBinding, content []any) {
	c := &Client{CloudIDEHost: host}
	session := randomID()
	body := map[string]any{
		"config_name": b.ConfigName, "model_name": b.ModelName,
		"conversation_id": session, "session_id": session,
		"stream": true, "raw_chat_function": ChatV3Function, "config_source": b.ConfigSource,
		"messages": []map[string]any{{"role": "user", "content": content}},
	}
	raw, _ := jsonMarshalProbe(body)
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, host+rawChatV2Path, bytes.NewReader(raw))
	c.setIDEHeadersV2(req, os.Getenv("TRAE_PROBE_TOKEN"), os.Getenv("TRAE_PROBE_UID"), ChatV3Function)
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	final := ""
	for sc.Scan() {
		payload := strings.TrimSpace(strings.TrimPrefix(sc.Text(), "data:"))
		if !strings.HasPrefix(sc.Text(), "data:") {
			continue
		}
		if i := strings.Index(payload, `"response":"`); i >= 0 {
			seg := payload[i+len(`"response":"`):]
			if j := strings.Index(seg, `"`); j >= 0 {
				final += seg[:j]
			}
		}
		if strings.Contains(payload, "token_usage") || strings.Contains(payload, "[DONE]") {
			break
		}
	}
	t.Logf("HTTP %d 最终回答: %q", resp.StatusCode, truncateProbe(final, 100))
}

func TestProbeImageSupport(t *testing.T) {
	token := os.Getenv("TRAE_PROBE_TOKEN")
	img := os.Getenv("TRAE_PROBE_IMG")
	if token == "" || img == "" {
		t.Skip("需要 TRAE_PROBE_TOKEN / TRAE_PROBE_IMG")
	}
	host := envOr("TRAE_PROBE_HOST", "https://console.enterprise.trae.cn")
	gw := envOr("TRAE_PROBE_GATEWAY", DefaultModelGatewayHost)

	textBlock := map[string]any{"type": "text", "text": "这张图片是什么颜色？只答一个词"}
	openaiImg := map[string]any{"type": "image_url", "image_url": map[string]any{
		"url": "data:image/png;base64," + img}}
	rendererImg := map[string]any{"type": "image_url", "data": map[string]any{
		"mimeType": "image/png", "data": img}}
	inputImg := map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + img}

	cases := []struct {
		name string
		host string
		v2   bool
		fn   string
		model string
		content []any
	}{
		{"ide-glm5.3-文本对照", host, false, ideFunction, "glm-5.3", []any{textBlock}},
		{"ide-glm5.3-openai图", host, false, ideFunction, "glm-5.3", []any{openaiImg, textBlock}},
		{"ide-glm5.3-renderer图", host, false, ideFunction, "glm-5.3", []any{rendererImg, textBlock}},
		{"ide-glm5.3-input_image图", host, false, ideFunction, "glm-5.3", []any{inputImg, textBlock}},
		{"v2-glm5.2-文本对照", host, true, ChatV3Function, "glm-5.2", []any{textBlock}},
		{"v2-glm5.2-openai图", host, true, ChatV3Function, "glm-5.2", []any{openaiImg, textBlock}},
		{"gw-deepseek-openai图", gw, false, ideFunction, "deepseek-v4.1-flash", []any{openaiImg, textBlock}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := ideChatPath
			if tc.v2 {
				path = rawChatV2Path
			}
			probeSend(t, tc.host, path, tc.v2, tc.fn, tc.model, nil, tc.content)
		})
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}


func TestProbeDeepseekVision(t *testing.T) {
	token := os.Getenv("TRAE_PROBE_TOKEN")
	img := os.Getenv("TRAE_PROBE_IMG2")
	if token == "" || img == "" {
		t.Skip("需要 TRAE_PROBE_TOKEN / TRAE_PROBE_IMG2")
	}
	gw := envOr("TRAE_PROBE_GATEWAY", DefaultModelGatewayHost)
	textBlock := map[string]any{"type": "text", "text": "图片中央的大方块是什么颜色？只答一个词，不许猜"}
	openaiImg := map[string]any{"type": "image_url", "image_url": map[string]any{
		"url": "data:image/png;base64," + img}}
	t.Run("gw-deepseek-终局", func(t *testing.T) {
		probeSend(t, gw, ideChatPath, false, ideFunction, "deepseek-v4.1-flash", nil, []any{openaiImg, textBlock})
	})
}
