# OpenAI 账号限流不随用量恢复自动解除

状态：**未修复**（2026-09-28 线上发现，暂以手动清除限流处理）

## 现象

后台账号列表中，OpenAI OAuth 账号显示「限流中 · Nd 自动恢复」，但右侧 codex 用量条显示 5h / 7d 窗口都有余量，或 5h 窗口几分钟后就重置。账号在这段时间内不参与调度。

2026-09-28 线上实例：

| 账号 | 限流标记到 | 实时用量 | 情况 |
|---|---|---|---|
| #24、#25 | 10-02 | 7d 0% | 09-26 真实触发 7d 限流，上游之后提前重置了周额度 |
| #16 | 09-29 | 7d 0% | 同上 |
| #22 | 09-29 14:44 | 5h 100%（14:54 重置），7d 16% | 当初耗尽的窗口已恢复，现在只剩 5h 耗尽，被多封约 1 天 |
| #1 | 10-12 | 30 天窗口 100% | 真实耗尽，限流正确 |

## 根因

1. 收到 429 时，`backend/internal/service/ratelimit_service.go` 的 `handle429` 按当时窗口的重置点写入 `accounts.rate_limit_reset_at`（日志关键字 `openai_429_7d_limit_exhausted`、`openai_429_5h_limit_exhausted`、`openai_account_rate_limited`）。
2. 之后用量探针 `backend/internal/service/account_usage_service.go` 中的 `probeOpenAICodexSnapshot` 调用 `persistOpenAICodexProbeSnapshot`，只把最新用量写入 `extra.codex_*`，不回头校正或清除 `rate_limit_reset_at`。
3. 调度器（`openai_account_runtime_block_fastpath.go`）只看 `rate_limit_reset_at`。被封的账号不参与调度，也就没有成功请求来解除它，只能等到旧时间点。
4. 对照：Anthropic 在响应状态为 `allowed` 时会自动清除限流（`ratelimit_service.go` 的 session window 更新逻辑），OpenAI 没有对应机制。

会触发的两种情况：

- 上游提前重置额度。09-28 10:53 左右，#16、#24、#25 的 7d 窗口同时归零。
- 当初耗尽的是长窗口，后来只剩短窗口耗尽，限流时间仍停在长窗口的重置点。

## 临时处理

1. 确认账号 `extra.codex_5h_used_percent`、`extra.codex_7d_used_percent` 都低于 100，或已过 `codex_*_reset_at`。
2. 在后台点「清除限流」，或调用 `POST /api/v1/admin/accounts/:id/clear-rate-limit`。
3. 清除后观察一次请求。限流可能掩盖了其他问题：09-28 的 #16 清除限流后立刻返回 `401 Token revoked`，需要重新 OAuth 授权。

## 修复思路（待评审）

探针拿到 codex 快照后，如果账号处于限流中：

- 所有窗口都低于 100%，且探针返回 2xx：清除账号级限流。
- 有窗口为 100%，且它的重置点早于已记录的时间：把 `rate_limit_reset_at` 缩短到该重置点。
- 只缩短或解除，不新增、不延长限流。新增限流仍然只由真实请求的 429 触发。判断耗尽时直接用 primary / secondary 原始窗口，兼容 30 天等非 5h / 7d 的窗口。
