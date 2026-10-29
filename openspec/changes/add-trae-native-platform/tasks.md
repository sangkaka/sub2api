# Tasks: add-trae-native-platform

## 1. PR-1 后端核心

- [x] 1.1 平台登记：`domain/constants.go` 加 `PlatformTrae = "trae"`；`domain/platforms.go`
      platformList 登记（DisplayName "TRAE"，Gateway=PlatformGatewayOpenAI）；补
      platforms/platform_catalog 测试。
- [x] 1.2 `pkg/trae/oauth.go`：ExchangeToken（ClientID 常量 `ono9krqynydwx5`、ClientSecret "-"
      约定）、GetUserInfo（`x-cloudide-token` 头）；refresh_token 轮换校验（响应落库前比对请求时
      的 rt 仍为当前值）；TokenExpireAt 秒/毫秒/ISO 归一化。
- [x] 1.3 `pkg/trae/catalog.go`：拉取 `/models?functions=solo_agent_remote,solo_work_remote,
      solo_design_remote&show_custom_model=true`，解析 config_name/descriptor
      （reasoning_effort_config、max_mode、上下文上限）；`defaultModelIDsForPlatform` 静态兜底表。
- [x] 1.4 `pkg/trae/client.go`：`POST /chat_sessions`（Cloud-IDE-JWT 头组、common_params JSON
      字符串、agent_type/agent_id 钉住、手动模型回带完整 descriptor）、
      `GET /chat_sessions/{id}/events` SSE、`POST /stop`；首事件超时（120s）与无 done 截断判定。
- [x] 1.5 `pkg/trae/translator.go`：OpenAI messages → 扁平化 query（[System]/[Assistant]/
      [Client Tool Result] 标记 + `[{"type":"text","data":{"content":...}}]` 包裹）；SSE 快照差分
      （plan_item 按 id 的 thought/content 累加器）；heartbeat 透传；token_usage → OpenAI usage；
      model_config 回填实际模型名；错误码表（4011/429/3004/4008/1001/401；4028 未在源码中验证，按不可重试处理）。
- [x] 1.6 `service/trae_token_provider.go` + `trae_token_refresher.go`：挂入
      `token_refresh_service.go` 注册表与 wire；热路径过期前刷新 + Redis 锁。
- [x] 1.7 `service/trae_runtime.go` + 网关分支：`openai_gateway_chat_completions.go` PlatformTrae
      分支；流式/非流式；reasoning_effort → custom_model（descriptor 钳制）；首模型事件前的换号
      重试与不重放原则；1001/401 强刷后单次重试。
- [x] 1.8 调度接入：建号/测试时抓 `quota.solo_agent_parallel_limit` 写账号 concurrency；
      4011/429 → SetRateLimited。
- [x] 1.9 `account_test_service.go` trae 分支：GetUserInfo + 最小会话探活。
- [x] 1.10 单测：以 Trae2api-cn `tests/`（test_trae_remote_client.py / test_sse.py /
      test_auth.py）的离线 fixture 改写 Go 用例：快照差分、心跳、错误映射、token 归一化、
      refresh_token 竞态。

## 2. PR-2 前端与用量

- [ ] 2.1 `CreateAccountModal.vue` trae 平台 tab：粘贴 refresh_token（+可选 user_id），
      提交即走后端兑换；ReAuthAccountModal 重授权（粘贴新 refresh_token）。
- [ ] 2.2 PlatformIcon / platformColors / i18n（zh+en）。
- [ ] 2.3 `api/admin/trae.ts` + composable；账号列表显示积分（ide_user_ent_usage 映射）。
- [ ] 2.4 模型目录同步按钮（sync-upstream 已有机制接 catalog 拉取）。

## 3. PR-3 增强（按需，逐项独立）

- [ ] 3.1 工具调用：Remote 文本桥接（ToolCallAccumulator 移植）或评估 IDE 原生
      `/api/ide/v2/llm_raw_chat`。
- [ ] 3.2 图片输入（media parts）。
- [ ] 3.3 1M Max 开关（strategy=max + context_window_size + mode_type）。
- [ ] 3.4 签到自动化（后台定时 + 9074 指数退避 + 设备换代）。

## 4. 内部迁移（账号 40 切流）

- [ ] 4.1 从 4.111 `data/accounts.json` 提取 refresh_token/user_id/client_id/provider_specific
      （手工单次跳板机操作，禁止脚本重试）。
- [ ] 4.2 建原生 trae 账号 + 分组接入（13→11→12→8 灰度序），观察 usage/错误率/思考强度生效。
- [ ] 4.3 watchdog：policy_drift=0；`-trae` 别名与账号 40 清理。
- [ ] 4.4 中转容器只读观察 1–2 周后下线 4.111 服务。

## 验收

- OpenAI Chat Completions 流式/非流式全通（zcode 实际流量形态），思考内容进
  reasoning_content，usage 正确计费。
- token 到期自动刷新无感；refresh_token 轮换落库正确；401 强刷自愈。
- 4011/429 触发账号冷却并换号；调度不超 solo_agent_parallel_limit。
- 账号 40 相关别名/借位全部退役，4.111 中转可下线。
