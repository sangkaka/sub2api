# Proposal: 新增原生 TRAE 平台（OAuth 账号 + Remote 私有协议，摆脱 trae2api 中转）

## Why

当前 TRAE（Trae CN）能力是借 4.111 上的 Trae2api-cn 中转接入的：sub2api 账号 40 挂
`platform=deepseek` + `type=apikey` + `base_url` 指向中转的 OpenAI 兼容端点。这条链路有三层结构性成本：

1. **语义错位**：借用 deepseek 平台导致命名空间冲突（`-trae` 别名 hack）、DeepSeek 余额探测误打
   中转端点、目录/白名单/定价全部按 deepseek 语义走。
2. **多一跳与单点**：tw-01 → IDC 公网 NAT(117.160.218.10:8100) → 4.111 容器 → Trae 上游，链路上
   每个环节（NAT 源过滤、容器重启、中转升级）都是独立故障域；中转本身 4 万行 Python，运维面大。
3. **能力割裂**：TRAE 的多账号轮询、并发槽位（entitlement `solo_agent_parallel_limit`）、积分、
   签到、令牌刷新都活在 sub2api 之外，无法参与 sub2api 的调度、冷却、配额与计费体系。

Trae2api-cn 的授权、刷新、调用逻辑已经完整逆向并在生产验证（源码通读结论见 design.md），
协议本身不复杂：一个 `ExchangeToken` 刷新端点 + 一个两步会话 SSE 端点 + 两个目录/积分端点。
sub2api 已有两类成熟样板（Kiro：OAuth + 私有 event-stream 协议 + 429 冷却；CN 供应商：OpenAI
网关族多协议转发）。把 TRAE 协议原生搬进 sub2api 是可行且划算的。

## What Changes

- 登记新平台 `trae`（`PlatformGatewayOpenAI` 网关族，对用户暴露 Chat Completions，与账号 40
  现消费形态一致；无 DB 迁移）。
- 新增 `backend/internal/pkg/trae/`：OAuth ExchangeToken 客户端、模型目录拉取、Remote
  `chat_sessions` 两步会话客户端、SSE 快照→增量翻译器、错误码映射。
- 新增 `service/trae_token_provider.go` / `trae_token_refresher.go`（挂入 TokenRefreshService
  双轨刷新）与 `service/trae_runtime.go` 转发分支（样板：`kiro_runtime.go`、
  `openai_gateway_chat_completions.go` 的 Grok 平台分支）。
- 账号形态：`type=oauth`，凭据 = `{refresh_token, user_id, client_id, token, expires_at,
  provider_specific{...}}`（JSONB）。v1 接入方式为"粘贴 refresh_token"——TRAE 老授权流程证明仅凭
  refresh_token 即可通过 ExchangeToken 兑换完整凭据；完整网页授权助手为后续增强。
- 并发槽位映射：账号 `concurrency` ← entitlement `quota.solo_agent_parallel_limit`（账号测试/
  刷新时抓取）；上游业务码 4011/429 → 现有 `SetRateLimited` 冷却，1001/401 → 强制刷新 token。
- 模型目录：从上游 `/models` 拉取（样板 `FetchUpstreamSupportedModels`），内置静态兜底表。
- 前端：平台目录自动出现（platformCatalog 由后端生成），补 CreateAccountModal 的 trae 表单、
  图标、配色、i18n。
- 迁移：从 4.111 中转的 `data/accounts.json` 提取 `refresh_token/user_id/client_id` 建原生账号，
  灰度切流后下线 trae2api 依赖（账号 40 退役）。

## Capabilities

### 新增 capability：`trae-native-platform`

覆盖：平台登记与网关归属；OAuth 凭据模型与双轨令牌刷新；Remote 私有协议调用与 OpenAI
Chat Completions 双向翻译；模型目录与 descriptor 绑定；并发/配额/风控错误映射进现有调度；
账号迁移路径。

## Impact

- **代码面**：平台登记两处常量 + `pkg/trae/`（约 1500–2500 行 Go，参照 kiro translator 体量的
  Remote-only 版）+ service 层 4 个新文件 + 网关分支 + 前端表单。无数据库迁移（credentials 为
  无 schema JSONB）。
- **不影响现有平台**：`trae` 是新增平台轴，不触碰 deepseek/openai 等现有语义；账号 40 迁移后
  `deepseek` 平台的 `-trae` 别名即可清理。
- **与 `add-openai-compatible-platform` 提案的关系**：本提案落地后，TRAE 不再需要
  `openai_compatible` 通用透传平台；该提案对其它任意 OpenAI 兼容上游仍然独立成立，两者可并存。
- **风险**：TRAE 协议为逆向所得，上游可能变动——端点与 descriptor 全部可配置/从上游拉取，
  变动影响面收敛在 `pkg/trae/`。协议搬运仅供内部使用，需遵守上游服务条款。
- **上游 PR 策略**：sub2api 上游已收录 kiro/adobe 等逆向协议平台，trae 属 CN 生态，具备上游
  化条件；但体量较大，建议先在 fork 内合入验证，稳定后再拆 PR 推上游。
