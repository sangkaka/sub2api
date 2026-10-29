# `trae-native-platform` Specification

## Purpose

定义 TRAE（Trae CN）作为 sub2api 原生平台的登记、凭据、令牌刷新、私有协议调用与调度行为，
使其不再依赖外部 trae2api 中转。

### Requirement: 平台登记与网关归属

平台 `trae` MUST 在 `domain/platforms.go` 的 platformList 登记，归属
`PlatformGatewayOpenAI` 网关族（对用户暴露 OpenAI Chat Completions 形状），
`CNProvider=false`、`LiteLLMProvider` 留空（不参与 LiteLLM 定价目录与 CN 供应商余额能力）。

#### Scenario: 平台登记即目录生效

- **WHEN** 后端启动
- **THEN** `BuildPlatformCatalog()` 含 trae 条目（自动派生，无需前端登记）
- **AND** `IsConcretePlatform("trae")` 为 true，可作为账号平台与分组平台

#### Scenario: 网关路由

- **WHEN** 用户请求打到 OpenAI 网关且选中账号 `platform=trae`
- **THEN** 转发进入 TRAE 专用 runtime 分支（Chat Completions 双向翻译），不走通用
  OpenAI 透传路径

### Requirement: OAuth 凭据与令牌刷新

TRAE 账号 MUST 为 `type=oauth`，credentials 持久化 `refresh_token`、`user_id`、`client_id`、
`token`、`expires_at` 及上游身份字段（provider_specific）。系统 MUST 支持两种刷新触发：
后台 TokenRefreshService 定时扫描与请求热路径按需刷新；刷新 MUST 经上游
`/cloudide/api/v3/trae/oauth/ExchangeToken` 端点，并正确处理 refresh_token 轮换
（落库前校验请求时所用的 refresh_token 仍为当前值）。

#### Scenario: 仅凭 refresh_token 建号

- **WHEN** 管理员粘贴 refresh_token 创建 trae 账号
- **THEN** 后端调用 ExchangeToken 兑换 Cloud-IDE-JWT 并补全 user_id 与身份字段后落库
- **AND** 兑换失败时建号报错，不落半截凭据

#### Scenario: 到期前无感刷新

- **WHEN** token 距过期不足刷新窗口（默认 30 分钟）
- **THEN** 热路径/后台刷新产出新 token 与轮换后的 refresh_token 并更新 credentials
- **AND** 并发刷新经分布式锁串行，落库前做轮换校验

#### Scenario: 401/业务码 1001 自愈

- **WHEN** 上游调用返回 token 失效（HTTP 401 或业务码 1001）
- **THEN** 对该账号强制刷新一次并重试；刷新失败则账号标记错误且不重放请求

### Requirement: Remote 私有协议调用与翻译

TRAE 上游调用 MUST 使用 Remote 两步会话协议（`POST /chat_sessions` 建会话 +
`GET /chat_sessions/{id}/events` 读 SSE + `POST /stop` 收尾），鉴权头为
`Authorization: Cloud-IDE-JWT <token>`。请求 MUST 显式钉住 `agent_type`/`agent_id`；显式模型
MUST 回带上游模型 descriptor（`custom_model`），绑定失败时 MUST 报错而非让上游静默换模型。
SSE 翻译 MUST 处理：累计快照差分（plan_item 的 thought→reasoning_content、content→正文）、
heartbeat 透传、token_usage 映射、无 done 的截断判定。

#### Scenario: 流式对话

- **WHEN** 客户端发起 stream=true 的 Chat Completions（含 reasoning_effort）
- **THEN** 网关扁平化消息建 TRAE 会话，按思考强度钳制后随 custom_model 发送
- **AND** SSE 事件差分为增量 delta 输出，思考进 `delta.reasoning_content`，
     usage 在完成帧回填，心跳以 SSE 注释透传

#### Scenario: 上游错误映射

- **WHEN** 上游返回业务码 4011/429（并发/配额）、4028（模型未绑定）或首模型事件前空响应
- **THEN** 4011/429 触发账号冷却并可在首模型事件前换号重试；4028 透出不可重试错误；
     已观察到模型输出的会话 MUST NOT 重放请求

### Requirement: 模型目录与账号能力

模型列表 MUST 优先从上游 `/models` 目录按账号拉取（含 descriptor），并内置静态兜底表；
账号并发槽位 MUST 采用上游 entitlement `quota.solo_agent_parallel_limit`（未知时保守取 2）。

#### Scenario: 目录同步

- **WHEN** 管理员触发模型同步或建号测试
- **THEN** 拉取该账号可用模型（config_name + descriptor）供白名单/映射使用

#### Scenario: 并发受控

- **WHEN** 某账号在途请求数达到其 solo_agent_parallel_limit
- **THEN** 调度器不再向该账号派发新请求（排队或换号），不依赖上游 4011 兜底

### Requirement: 与中转部署的迁移边界

原生平台落地后 MUST 提供账号 40 → 原生账号的迁移路径（凭据来自中转 accounts.json 的
refresh_token），迁移完成前两套链路 MUST 可并存灰度；迁移后 `deepseek` 平台下的 `-trae`
别名与账号 40 MUST 退役。

#### Scenario: 灰度切流

- **WHEN** 原生 trae 账号加入分组并逐步承接流量
- **THEN** watchdog 观察策略漂移为 0、错误率与用量正常后移除账号 40
- **AND** 中转服务下线不再影响 sub2api 任何功能
