# Claude 账号并发配置调研

更新时间：2026-09-29

## 结论

Claude OAuth/订阅账号没有公开的、对所有账号都适用的固定并发上限。并发设置是中转网关的本地准入限制，不能等同于 Anthropic 对账号的官方承载能力。

以风控和稳定性为优先时，建议单个 Claude OAuth 账号按以下范围配置：

| 场景 | 建议账号并发 |
| --- | ---: |
| 单人、稳定优先 | 1 |
| 单人 Claude Code，偶尔启动一个子代理 | 2 |
| 单人频繁使用子代理 | 3 |
| 多人共享一个 OAuth 账号 | 不建议；应拆分账号 |

建议从 2 开始运行，观察 1～2 天的 429、登录失效、请求排队和并发槽位异常，再决定是否提高到 3。5 以上不建议作为默认值，0 或负数也不应作为 OAuth 账号的风控配置，因为在网关中通常表示不限制。

## sub2api 实现

- 新增账号页面把 `concurrency` 初始填为 10：[`CreateAccountModal.vue`](../frontend/src/components/account/CreateAccountModal.vue#L5581)。这是前端表单初值，项目历史中没有找到说明它来自 Claude 官方限制或压测结果。
- Ent 账号模型的后端默认值实际是 3：[`account.go`](../backend/ent/schema/account.go#L98)。用户并发默认值是 5：[`user.go`](../backend/ent/schema/user.go#L55)。因此“仓库默认 10”主要指 UI 初始值，并不是统一的后端默认策略。
- 账号槽位按请求/turn 获取并在请求完成后释放：[`concurrency_service.go`](../backend/internal/service/concurrency_service.go#L339)。主代理和并行子代理同时发请求时会叠加占用账号并发。
- `maxConcurrency <= 0` 在普通账号槽位逻辑中表示不限制：[`concurrency_service.go`](../backend/internal/service/concurrency_service.go#L342)。在部分 OAuth 的 `mode_router_v2` 路径中，并发小于等于 0 又可能被判定为不可调度，不能把它当作可靠的“无限容量”配置。

## 社区证据

- [sub2api Issue #1213](https://github.com/Wei-Shaw/sub2api/issues/1213)：账号配置为 10 时，用户报告实际只有一个 Claude Code 和一个子代理，却出现账号并发异常占满。评论还提到将限制从 10 调到 100 后，显示并发会瞬间拉高并且长期降不下来。该报告说明配置值过大可能放大槽位回收或统计异常的影响，不能证明账号支持 10 并发。
- [Claude Code Issue #96872](https://github.com/anthropics/claude-code/issues/96872)：Max 账号在约 6 个桌面会话同时活跃时出现 429，而 5 小时和周用量仍只有约 26%。这说明动态限流可能早于订阅用量上限触发。
- [Claude Code Issue #80452](https://github.com/anthropics/claude-code/issues/80452)：Remote Control 环境报告 32 个并发会话硬上限。这是环境会话容量，不是单个 Claude OAuth 账号的推荐 API 并发。
- [Claude Code Issue #97129](https://github.com/anthropics/claude-code/issues/97129)：一次性启动 12 个并行子代理导致账号使用上限耗尽，说明子代理会同时增加请求压力和订阅用量消耗。

## CPA/CLIProxyAPI 对照

公开的 [CLIProxyAPI README](https://github.com/router-for-me/CLIProxyAPI) 主要描述多账号接入、轮询负载均衡和配额管理，没有给出单个 Claude OAuth 账号的固定安全并发数字。相关面板和插件通常提供账号级监控、RPM 或并发控制，但这些是代理侧策略，不是 Anthropic 对 OAuth 账号的官方保证。

因此，CPA 与 sub2api 的配置原则应一致：把并发作为保护上游账号的限流阀，从 1～2 的低值开始，通过实际 429、认证刷新失败、请求延迟和槽位回收情况逐步调高；不要把 UI 默认值、API 连接池大小或某个环境的会话上限当成 Claude 账号的官方并发额度。

## 配置建议

1. Claude OAuth：默认 2，风险敏感场景设为 1。
2. 有并行子代理时，账号并发至少要覆盖“主代理 + 同时活跃的子代理”，但不建议为了吞吐直接设为 10。
3. 多用户共享时按账号总峰值计算，而不是按单个用户计算；更稳妥的方案是拆分账号或使用正式 API Key/企业渠道。
4. Claude API Key：按 Anthropic 组织的 RPM、输入/输出 token 限额压测，不能套用 OAuth 账号的经验数字。

