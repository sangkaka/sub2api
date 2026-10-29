# Design: 原生 TRAE 平台

## 0. 背景与信息来源

协议事实全部来自 Trae2api-cn 源码通读（github.com/autumnsentiment/Trae2api-cn，本地
`~/Codes/Trae2api-cn`，4.111 部署即该仓库），关键文件：`src/auth.py`（授权/刷新/账号存储）、
`web_login.py` + `src/auth_helper.py`（网页授权与本机回调助手）、`src/trae_remote_client.py`
（Remote 两步会话协议）、`src/trae_client.py`（模型目录/签到/积分/entitlement）、`src/sse.py`
（SSE 快照→增量翻译）。账号 40 当前经由该中转消费的端点模式即下文的 **Remote 模式**
（`UPSTREAM_MODE=remote` 默认值）。

## 1. TRAE 协议事实（已从源码验证）

### 1.1 凭据与刷新

每账号持久化凭据（trae2api 存 `data/accounts.json`，等价于 sub2api 的 credentials JSONB）：

| 字段 | 说明 |
|---|---|
| `token` | Cloud-IDE-JWT（短期） |
| `refresh_token` | 长期刷新令牌 |
| `user_id` | 用户 ID |
| `client_id` | 固定公开值 `ono9krqynydwx5` |
| `expired_at` / `refresh_expired_at` | 秒/毫秒时间戳或 ISO，需归一化 |
| `provider_specific` | 身份字段：`webId`/`bizUserId`/`userUniqueId`/`scope`(marscode-cn)/`tenant`(marscode)/`region`(cn)/`aiRegion`/`appLanguage`/`appVersion`/`userRegion`/`userIdentity` |

**ExchangeToken（唯一刷新端点，也是"仅凭 refresh_token 建号"的依据）**：

```
POST https://api.trae.cn/cloudide/api/v3/trae/oauth/ExchangeToken
{"ClientID":"ono9krqynydwx5","RefreshToken":rt,"ClientSecret":"-","UserID":uid}
→ {"Result":{"Token","RefreshToken","TokenExpireAt","RefreshExpireAt","UserInfo":{UserID,Region,AIRegion,WebId,BizUserId,...}}}
```

刷新窗口：过期前 30 分钟（trae2api `needs_refresh` 用 `ts-1800`）；有效性判定留 5 分钟缓冲；
HTTP 401 或业务码 1001 时按账号强制刷新（并发用每账号锁串行）。refresh_token 每次刷新会轮换，
响应回来必须校验"请求时所用的 rt 仍是当前值"再落库（防账号切换竞态，trae2api 同款处理）。

### 1.2 授权（首次获得凭据）

- 授权页：`https://www.trae.cn/authorization?client_id=...&auth_callback_url=http://127.0.0.1:<port>/authorize&machine_id&device_id&login_channel=native_ide&auth_type=local&...`
- Trae **强制回调地址为 `http://127.0.0.1:<port>/authorize`**，浏览器页面无法监听本机 TCP 端口，
  因此 trae2api 需分发本机 Python 助手（8765 端口）接收回调再把凭据 POST 回中转。
- 回调 query：`userJwt={Token,RefreshToken,TokenExpireAt,RefreshExpireAt,ClientID}` +
  `userInfo={UserID,Region,AIRegion,WebId,BizUserId,UserUniqueId,Tenant,...}`。
- **老流程**：回调只带 `refreshToken`，随后走 1.1 的 ExchangeToken 兑换全部凭据与用户信息。

**结论**：v1 采用"用户粘贴 refresh_token（+可选 user_id）→ 后端调 ExchangeToken 验证并补全
凭据"，与 sub2api 现有 OAuth code 手工粘贴交互同构，不需要在 v1 复刻本机助手。refresh_token
的获取途径：中转的 accounts.json、本机 Trae IDE `storage.json` 解密、或任意本机回调监听抓包。
完整网页授权（sub2api 生成同款助手脚本）列为后续增强。

### 1.3 调用（Remote 模式；Work 共用同一端点族）

基址：`https://trae-api-cn.mchost.guru/api/remote/v1`（可配置）。

请求头（`build_headers`，trae_remote_client.py:350）：

```
Authorization: Cloud-IDE-JWT <token>      # 非标准 scheme，不是 Bearer
X-Trae-Client-Type: web
X-Preferenced-Language: zh-CN
x-user-region: CN
Origin / Referer: https://solo.trae.cn
User-Agent: Chrome/149 桌面 UA
Accept: text/event-stream（流式读事件时）/ application/json
```

两步会话：

1. `POST /chat_sessions`，body：

```json
{
  "mode": "code",                       // work 模式为 "work"
  "environment_id": "default",
  "initial_message": {
    "chat_session_id": "",
    "content": [],
    "query": "<扁平化 JSON 文本，见下>",
    "model_name": "<config_name>",
    "agent_type": "solo_agent_remote",  // 与 agent_id 同值；work 为 solo_work_remote
    "agent_id": "solo_agent_remote",    // 缺省会被上游静默换默认模型，必须显式钉住
    "model_selection_strategy": "manual",
    "model_config_source": 1, "model_is_preset": true, "model_provider": "...",
    "custom_model": { /* 完整模型 descriptor（见 1.4），手动模型必带 */ },
    "common_params": "<JSON 字符串：身份字段 + device_id/machine_id 派生值 + biz_session_id>"
  },
  "env": "remote", "auto_create_project": false, "origin": "web"
}
→ data: { chat_session_id, message_id }
```

2. `GET /chat_sessions/{sid}/events?reply_to_message_id={mid}`（SSE）；
   收尾/取消 `POST /chat_sessions/{sid}/stop`。

SSE 事件（`stream_events` + `translate_web_events`）：

| 事件 | 语义 |
|---|---|
| `heartbeat` | 空闲心跳，**必须透传**给下游（否则客户端误判结束而重放） |
| `model_config` | 实际 provider 模型名（用于回填 usage 的 model 字段与校验） |
| `plan_item` | **累计快照**：`thought`=思考链（→reasoning_content），`content`=答案正文 |
| `message`/`assistant_message` | 正文快照（content 为 str 或 blocks 数组） |
| `token_usage` | usage 映射（prompt/completion/total/cache 字段） |
| `done` | 终止帧（含 `data: [DONE]` 变体）；无 done 的 EOF 判为截断 |
| `error` | 业务码见下表 |

快照语义是翻译器的核心：thought/content 都是**整段重发**，需按 plan_item id 维护累加器做差分
出 delta；工具调用在 Remote 上为**文本桥接**（schema 进 query 提示，模型文本解析回
`delta.tool_calls`）。

业务错误码（`error` 事件与 HTTP 层）：

| 码 | 语义 | sub2api 映射 |
|---|---|---|
| 4011 / 429 | 并发超槽/配额限流 | `SetRateLimited` 冷却 + 换号重试（首事件前） |
| 1001 / HTTP 401 | token 失效 | 强制 `RefreshAccountToken` 后重试一次 |
| 4028 | 模型未绑定该账号 | 不可重试错误，透出明确信息 |
| 9074 | 设备维度风控（签到链路） | 见 1.6 签到 |
| 上游空响应/首事件超时(120s) | 会话建立但无输出 | 首模型事件前允许一次换端点/换号重试；已有输出后不重放 |

其它调用细节：思考强度 `reasoning_effort`→`custom_model.reasoning_effort`（light/high/extra_high，
按模型 descriptor `reasoning_effort_config` 钳制）；1M Max 模式：`model_selection_strategy=max`
+ `context_window_size` + `mode_type=1`（仅 Agent 会话）；历史压缩 500 条/480K 字符，扁平化后
query >500K 上游会**静默断流**，须预裁剪或 413。

### 1.4 模型目录与 descriptor

- `GET {remote_base}/models?functions=solo_agent_remote,solo_work_remote,solo_design_remote&show_custom_model=true`
  → `data.list[]`（按 function 分组）→ `models[]`：`config_name`（请求用 ID）、
  `raw_chat_function`、descriptor（`reasoning_effort_config`、`max_mode`、上下文/输出上限、视觉能力）。
- 手动指定模型时**必须**回带完整 descriptor 作为 `custom_model`，否则上游静默选用其默认模型
  （trae2api 有显式绑定校验 `_bound_model_name` 防静默回落）。
- sub2api 映射：`FetchUpstreamSupportedModels`（upstream_models.go:211）拉取 + 账号内缓存；
  `defaultModelIDsForPlatform` 提供静态兜底表。

### 1.5 签到 / 积分 / entitlement

- 签到状态：`POST https://api.trae.cn/trae/api/v2/ug/checkin_credits/status`；领取：
  `/claim`（空 body）。头：`x-device-id`（16 位数字，`sha256(account_id)` 派生、每账号稳定，
  9074 时"换代"轮换）、`x-device-brand`/`x-device-type`。9074 后不可立即重试（会拉长风控窗口），
  需退避 + 换设备代。
- 积分/entitlement：`POST https://api.trae.cn/trae/api/v2/pay/ide_user_ent_usage`
  `{"require_usage":true,"req_source":1}` → `user_entitlement_pack_list[]`（product_type=2 的包有
  credits_limit/used）、`user_pay_identity(_str)`、**`quota.solo_agent_parallel_limit`（账号并发上限，
  免费号实测 2）**。
- 用户信息：`POST https://api.trae.cn/cloudide/api/v3/trae/GetUserInfo`（注意头是
  `x-cloudide-token`，`Authorization` 反而被拒）。

## 2. sub2api 侧映射（样板均为现有代码）

| TRAE 侧 | sub2api 落点 | 样板 |
|---|---|---|
| 平台登记 | `domain/constants.go` `PlatformTrae`；`domain/platforms.go` platformList 一条（`Gateway: PlatformGatewayOpenAI`，CNProvider=false，LiteLLMProvider 空） | grok/cline 条目 |
| 凭据 JSONB | `ent/schema/account.go` credentials（无迁移）；`trae_oauth_service.go` 的 BuildAccountCredentials：粘贴 refresh_token → ExchangeToken 验证 → 补全 token/user_id/身份字段 | `kiro_oauth_service.go:473` |
| 双轨刷新 | `trae_token_refresher.go`（后台扫描，注册进 `token_refresh_service.go:124-151`）+ `trae_token_provider.go`（热路径，过期前刷新 + Redis 锁 + 缓存） | `kiro_token_provider.go:54-100`、`oauth_refresh_api.go` |
| 转发分支 | `openai_gateway_chat_completions.go` 在 Grok 分支（:106）旁加 `if account.Platform == PlatformTrae` → `trae_runtime.go` Forward | `kiro_runtime.go:69/364` |
| 协议客户端 | `pkg/trae/`：`oauth.go`（ExchangeToken/GetUserInfo）、`catalog.go`（/models + descriptor）、`client.go`（chat_sessions/events/stop）、`translator.go`（OpenAI↔TRAE：query 扁平化、快照差分、usage、错误码） | `pkg/kiro/translator.go`、`pkg/antigravity/*transformer*` |
| 模型目录 | 上游拉取（账号维度）+ 静态兜底 `defaultModelIDsForPlatform` | `upstream_models.go:211`、antigravity `FetchAvailableModels` |
| 并发槽位 | 账号 `concurrency` 字段 ← `solo_agent_parallel_limit`（建号测试/刷新时抓取写 extra，变更时更新） | `concurrency_service.go:342` |
| 冷却/换号 | 4011/429→SetRateLimited；1001/401→强刷；首事件前空响应→换号 | `kirocooldown/store.go`、gateway_scheduling 排除集 |
| 用量/积分 | `account_usage_service.go` trae 分支（ide_user_ent_usage → 通用积分，映射为余额展示） | `cn_provider_balance_service.go` |
| 连通性测试 | `account_test_service.go` trae 分支（GetUserInfo + 一次最小 chat_sessions） | kiro 测试链路 :764 |
| 前端 | platformCatalog 自动；PlatformIcon/platformColors/CreateAccountModal(trae tab：粘贴 refresh_token)/useTraeOAuth/api/admin/trae.ts/i18n | kiro 前端套件 |

## 3. 账号语义

- `platform=trae`，`type=oauth`；创建路径：粘贴 `refresh_token`（必填）+ `user_id`（可选，
  ExchangeToken 响应可补全）→ 后端兑换并落 credentials；上游 `provider_specific` 身份字段随
  兑换结果一并入库（Remote common_params 需要它们，缺失时用 trae2api 同款默认值兜底）。
- 会话粘性：sub2api 现有 sessionHash 粘账号机制直接适用（TRAE 上游侧的会话由
  `biz_session_id`（账号+模型派生）承载，译者保持同款派生即可复用上游会话上下文）。
- 账号 40 迁移：从 4.111 `data/accounts.json` 读取既有账号的 `refresh_token/user_id/client_id/
  provider_specific` 建原生账号 → 分组灰度切流（watchdog policy_drift 清零）→ 退役账号 40 与
  `-trae` 别名 → 中转容器保留只读观察一段时间后下线。

## 4. 分期与 PR 切分

**PR-1 后端核心（可独立上线，纯配置切流）**
平台登记 + `pkg/trae`（oauth/catalog/client/translator，Remote-only）+ token provider/refresher +
网关 Chat Completions 分支（流式/非流式、思考强度、usage、心跳透传、错误映射）+ 调度槽位与冷却 +
账号测试分支 + 单测（协议 fixture 直接取 Trae2api-cn 的 tests/ 离线用例改写为 Go）。

**PR-2 前端与用量**
CreateAccountModal trae 表单（refresh_token 粘贴）、图标/配色/i18n、积分展示、模型目录同步按钮。

**PR-3 能力增强（按需）**
工具调用（v1 先不做：Remote 文本桥接的 ToolCallAccumulator 移植，或评估 IDE 原生工具端点
`/api/ide/v2/llm_raw_chat` chat_v3）、图片输入（media parts）、1M Max 开关、签到自动化
（后台任务 + 9074 退避/设备换代）、积分优先调度。

**迁移**（独立于 PR 序列）：账号 40 → 原生账号灰度切流 → 下线中转。

## 5. Non-goals

- 不做 IDE / Solo / Raw 端点（个人账号缺企业 app config，raw 固定 2001；IDE 原生工具列为 PR-3
  评估项）；不做国际版（trae.ai）host 切换。
- 不在 v1 做完整网页授权助手（本机回调监听器）分发。
- 不做 trae2api 的消费记录/控制台（sub2api 已有用量与计费体系）。
- 不实现 Auto 模型路由（`model=auto` 按任务复杂度选模型）——保持显式模型语义。

## 6. 风险

- **上游协议变动**：端点/头/descriptor 结构可能随 Trae 客户端版本变化。缓解：全部端点常量集中
  `pkg/trae`；descriptor 从上游拉取而非硬编码；账号连通性测试可快速暴露协议漂移。
- **规模**：翻译器是最大件（快照差分 + 心跳 + 错误映射），参照 kiro translator（~3700 行），
  Remote-only 预估 1500–2500 行 Go。
- **合规**：逆向协议，仅内部使用，遵守上游 ToS；凭据含长期 refresh_token，走 sub2api 现有
  凭据脱敏（account_credentials_redact.go）。
