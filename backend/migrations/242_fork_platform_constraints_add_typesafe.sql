-- Migration: 242_fork_platform_constraints_add_typesafe
-- 上游 241_add_typesafe_platform.sql 用 DROP + 重建加 typesafe，列表抄的是官方平台，
-- 会再次抹掉 fork 的 kiro / adobe。本迁移排在其后，把约束收敛回代码侧全集。
--
--   - user_platform_quotas / composite_model_routes：service.AllowedQuotaPlatforms 全部 13 项
--   - channel_monitors / channel_monitor_request_templates：不含 adobe，与 239 一致
--
-- DROP ... IF EXISTS 保证可重入；新约束是 239 与 241 的超集，存量行瞬时校验通过。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE user_platform_quotas
    ADD CONSTRAINT user_platform_quotas_platform_check
    CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                        'kimi', 'zhipu', 'deepseek', 'kiro', 'minimax', 'adobe', 'opencode_go', 'typesafe'));

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;

ALTER TABLE composite_model_routes
    ADD CONSTRAINT composite_model_routes_target_platform_check
    CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok',
                               'kimi', 'zhipu', 'deepseek', 'kiro', 'minimax', 'adobe', 'opencode_go', 'typesafe'));

ALTER TABLE channel_monitors
    DROP CONSTRAINT IF EXISTS channel_monitors_provider_check;

ALTER TABLE channel_monitors
    ADD CONSTRAINT channel_monitors_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok',
                        'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));

ALTER TABLE channel_monitor_request_templates
    DROP CONSTRAINT IF EXISTS channel_monitor_request_templates_provider_check;

ALTER TABLE channel_monitor_request_templates
    ADD CONSTRAINT channel_monitor_request_templates_provider_check
    CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok',
                        'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'));
