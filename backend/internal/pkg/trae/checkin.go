package trae

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
)

// 每日签到（PR-3.4）：领取端点按设备维度限流（业务码 9074）。设备 id 从
// 账号身份派生（16 位数字，跨 token 刷新稳定）；撞 9074 后不可立即重试
// （会拉长风控窗口），需退避 + 设备「换代」（generation+1 重新派生）。

const (
	checkinStatusPath = "/trae/api/v2/ug/checkin_credits/status"
	checkinClaimPath  = "/trae/api/v2/ug/checkin_credits/claim"

	checkinDeviceBrand     = "ASUS TUF Gaming A15 FA507RM_FA507RM"
	checkinDeviceType      = "windows"
	checkinCodeRateLimited = "9074"
)

// CheckinStatus 是签到状态查询结果。
type CheckinStatus struct {
	Enabled   bool    `json:"enable"`
	CheckedIn bool    `json:"checked_in"`
	Credits   float64 `json:"credits"`
}

// CheckinResult 是领取结果；RateLimited=true 表示 9074（需退避 + 换代）。
type CheckinResult struct {
	Claimed     bool
	Credits     float64
	RateLimited bool
	AlreadyDone bool
	RawMessage  string
}

// CheckinDeviceID 派生账号稳定（按 generation）的 16 位数字设备 id。
// generation>0 时换派生材料，实现「换代」绕开设备维度风控。
func CheckinDeviceID(identity string, generation int) string {
	if identity == "" {
		return ""
	}
	material := identity
	if generation > 0 {
		material = fmt.Sprintf("%s#gen%d", identity, generation)
	}
	digest := sha256Hex(material)
	// 64 位十六进制 = 256 bit，uint64 放不下；按 trae2api 语义用大数取模。
	value, ok := new(big.Int).SetString(digest, 16)
	if !ok {
		return "0000000000000000"
	}
	mod := new(big.Int).Mod(value, big.NewInt(1e16))
	return fmt.Sprintf("%016s", mod.String())
}

func (c *Client) checkinHeaders(req *http.Request, token, deviceID string) {
	req.Header.Set("Authorization", "Cloud-IDE-JWT "+strings.TrimSpace(token))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-device-id", deviceID)
	req.Header.Set("x-device-brand", checkinDeviceBrand)
	req.Header.Set("x-device-type", checkinDeviceType)
}

// CheckinStatus 查询当日签到状态。
func (c *Client) CheckinStatus(ctx context.Context, token, identity string, generation int) (*CheckinStatus, error) {
	var status CheckinStatus
	if err := c.postCheckin(ctx, checkinStatusPath, token, identity, generation, &status); err != nil {
		return nil, err
	}
	return &status, nil
}

// ClaimCheckin 领取当日积分。9074 映射为 RateLimited=true（不自动重试）。
func (c *Client) ClaimCheckin(ctx context.Context, token, identity string, generation int) (*CheckinResult, error) {
	var payload struct {
		Enable    bool    `json:"enable"`
		Code      any     `json:"code"`
		Message   string  `json:"message"`
		Credits   float64 `json:"credits"`
		CheckedIn bool    `json:"checked_in"`
	}
	if err := c.postCheckin(ctx, checkinClaimPath, token, identity, generation, &payload); err != nil {
		if ue, ok := err.(*UpstreamError); ok && ue.Code == checkinCodeRateLimited {
			return &CheckinResult{RateLimited: true, RawMessage: ue.Message}, nil
		}
		return nil, err
	}
	if code := anyToString(payload.Code); code != "" && code != "0" {
		return &CheckinResult{
			AlreadyDone: code == "9075" || strings.Contains(payload.Message, "already"),
			RawMessage:  fmt.Sprintf("[%s] %s", code, payload.Message),
		}, nil
	}
	return &CheckinResult{Claimed: true, Credits: payload.Credits}, nil
}

func (c *Client) postCheckin(ctx context.Context, path, token, identity string, generation int, out any) error {
	deviceID := CheckinDeviceID(identity, generation)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.apiHost()+path, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	c.checkinHeaders(req, token, deviceID)
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("trae: checkin %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("trae: checkin read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return &UpstreamError{
			Kind:    classifyHTTPError(resp.StatusCode),
			Code:    strconv.Itoa(resp.StatusCode),
			Message: fmt.Sprintf("checkin [%d]: %s", resp.StatusCode, truncate(string(raw), 300)),
		}
	}
	var envelope struct {
		Code    any    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("trae: checkin parse response: %w", err)
	}
	if code := anyToString(envelope.Code); code != "" && code != "0" {
		return &UpstreamError{Kind: ErrorKindUpstream, Code: code, Message: envelope.Message}
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("trae: checkin decode payload: %w", err)
	}
	if status, ok := out.(*CheckinStatus); ok {
		if math.IsNaN(status.Credits) || math.IsInf(status.Credits, 0) || status.Credits <= 0 {
			status.Credits = 0
		}
	}
	return nil
}

func sha256Hex(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
