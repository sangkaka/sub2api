# 已解决：nano-banana 恒 422 —— catalog 指向已下架的上游 v1 版本号

> 状态：**已修复（待部署）** · 首次记录 2026-10-29 · 复盘更新 2026-10-29 · 生产版本 `sub2api:0.2.19-kiro.d389dd6`
> 生产环境：`http://43.213.108.160:12110`（阿里云独立部署），Adobe 分组 id=14

## 结论（推翻首版判断）

**不是 Adobe entitlement 限制，免费通道完全支持 Google 模型。** 根因是本仓库 catalog 把
`firefly-nano-banana` 族指向上游 `modelVersion=nano-banana`（v1），而上游已把 v1 从
`generate-async` 的 IMAGE 端点下架，提交恒 422 `validation_error`。网页版正常，是因为
网页只提供现存版本；同一账号、同一端点，`modelVersion=nano-banana-2 / -3` 提交即成功。

## 复盘时间线（2026-10-29 实测）

1. 直连上游（用生产账号 44 的 firefly 通道 token、与网关完全相同的头部/端点）对照提交：
   - `modelVersion=nano-banana`（v1）→ **422**，完整错误体（管理端 300B 截断导致首版
     排查只看到前几个条目）：
     ```text
     gemini-flash nano-banana is not supported for ModelModality.IMAGE endpoint.
     Supported models: ['flux'×9, 'fluxPro', 'fluxUltra', 'firefly'×2, 'firefly-image'×2,
     'flux2-klein', 'fal-ideogram', 'fal-gemini-flash'×3, 'qwen-image-edit', 'humain',
     'gemini-flash'×4, 'imagen'×11, 'firefly', 'firefly-image', 'gpt-4o-image',
     'gpt-image'×3, 'runway', 'runway-gen4-image', 'topaz'×9]
     ```
     列表按「每个版本重复一次 modelId」计数：**gemini-flash 有 4 个版本在支持列表里**，
     只是不含 v1。
   - `modelVersion=nano-banana-2` → **HTTP 200，真实出图**（1.4MB PNG）。
2. 生产网关复测（同批账号、正式网关路径）：
   - `nano-banana-pro`（本就映射 `nano-banana-2`）→ **200 出图**；
   - `nano-banana2`（本就映射 `nano-banana-3`）→ **200 出图**。
   即首版文档「banana 系全部 422」不成立，损坏的只有 `firefly-nano-banana` 族的 v1 映射。
3. 参照项目 adobe2api-plus（2026-08 起）同样把公开名 `nano-banana` 落到 `nano-banana-2`。
4. **两条计费通道都实测放行 Google 模型**（首版文档的「express 通道未验证」事项就此关闭）：
   - firefly 免费通道（`access_token` + `clio-playground-web` 身份）：直连提交
     `nano-banana-2` → 200 出图（多次复测，1024x1024）；
   - express 付费通道（`express_access_token` + `ExpressIdentity`：x-api-key
     `projectx_webapp`、origin `new.express.adobe.com`）：直连提交 `nano-banana-2`
     → 200 出图，credits/balance 的 used 65→105，确认在扣积分（单张 banana 积分
     消耗明显高于 gpt-image 的 ~2~2.5，注意余额；读数可能含此前 express 出图的
     滞后入账）。生产三个账号均开 `adobe_use_credits=true`，前述网关成功请求按
     调度逻辑走的也是 express 通道。

## 修复（本仓库）

1. **catalog 版本映射**（`backend/internal/pkg/adobe/catalog.go`）：
   `firefly-nano-banana.upstreamModelVersion` `nano-banana` → `nano-banana-2`。公开名
   `nano-banana` / `gemini-2.5-flash-image(-preview)` 保留，与 Pro 共用 v2 落点
   （守卫测试 `TestNanoBananaFamilyMapsToUnpluralizedUpstream` 已同步改写）。
2. **banana 必须显式发 aspectRatio（含 1:1）**（`backend/internal/pkg/adobe/payloads.go`）：
   复测时发现请求 `size=1024x1024`（方图）时，上游 v2/v3 在不带 `modelSpecificPayload.aspectRatio`
   时按模型默认比例出图——网关产物实际是 **1408x768 宽图**。改为除空/`auto` 外一律显式
   发送；修复后同请求上游返回 **1024x1024 方图**。
3. 验证：`go test -tags unit ./internal/pkg/adobe/ ./internal/service/ ./internal/handler/`
   全绿（service 需临时移开与本问题无关的坏测试 `image_count_billing_switch_test.go`，
   该文件在 main 上本就编译失败）；并用修复后代码直连上游完成「解析 → 提交 → 轮询」
   全链路出图（产物 1024x1024）。

> 注意：生产仍跑旧构建，**需要重新部署**后 `nano-banana` 才会在网关侧恢复。

## 遗留跟进

- [ ] **部署新构建**并在生产网关复测 `model_id=nano-banana`。
- [ ] **透出上游 422 细节**：`classifyAdobeHTTPError`（`backend/internal/pkg/adobe/client.go`）
      对外只给固定文案 `Adobe rejected the request (HTTP 422)`。本次排查正是被它+300B
      截断挡住：`error_code=validation_error` 且 message 命中 `not supported` 时应给出
      更明确的「模型/版本不受支持」语义（注意不泄漏内部字段）。
- [ ] **BodyPreview 截断加长**：`maxErrorBodyBytes=300` 让支持列表中途截断，首版排查因此
      误判「支持列表里没有 Google 模型」。
- [ ] **catalog 按上游刷新**：上游支持列表已出现 `flux2-klein`、`fal-ideogram`、
      `fal-gemini-flash`、`qwen-image-edit`、`humain`、`topaz` 等新族（`testdata/discovery_snapshot.json`
      是 9 月的旧快照，且 gemini-flash 已有 4 个版本、v1 已移除）；可重新抓 discovery 更新
      目录与快照守卫测试。

## 相关代码路径

- 模型族表（外部名→firefly 族→上游 modelId/modelVersion）：
  `backend/internal/pkg/adobe/catalog.go` 的 `rawImageFamilySpecs` / `externalImageModelAliases`。
- banana payload 构造（aspectRatio 规则）：`backend/internal/pkg/adobe/payloads.go` 的
  `buildNanoBananaPayloads`。
- 422 分类为终态 4xx（`adobe_request`）：`backend/internal/pkg/adobe/client.go` 的
  `classifyAdobeHTTPError`；原文仅在日志 `Message`。
- 网关入口：`backend/internal/handler/adobe_images.go`（`/v1/images/generations`、`/v1/images/edits`）。
- 单账号按模型复测入口：`POST /api/v1/admin/accounts/{id}/test`，body `{"model_id":"nano-banana"}`，
  SSE 输出含上游错误体原文（300B 截断）。

## 首版文档的错误认定（留档）

首版把 422 判定为「免费通道整体不含 Google 模型的 entitlement 限制」，依据是截断后的
支持列表里只见 flux/firefly 系，且三个账号（43/44/45，14000/10000 积分池）同报错。
实际是版本号淘汰：错误列表完整展开后 gemini-flash 有 4 个受支持版本，且当时未实测
`nano-banana-pro` / `nano-banana2`（它们一直正常）。教训：**对截断的错误体不做外推，
结论前先跑对照组（当时同一批账号的 gpt-image 全通过，说明通道与鉴权无恙）。**

## 复现（修复后应全部 200）

```sh
for m in nano-banana nano-banana-pro nano-banana2 gemini-2.5-flash-image gemini-3-pro-image; do
  curl -sS -m 180 http://<host>:12110/v1/images/generations \
    -H "Authorization: Bearer <adobe-group-key>" -H "Content-Type: application/json" \
    -d "{\"model\":\"$m\",\"prompt\":\"a tiny red circle\",\"size\":\"1024x1024\",\"n\":1}" \
    -o "/tmp/probe_$m.json" -w "$m → %{http_code}\n"
done
```
