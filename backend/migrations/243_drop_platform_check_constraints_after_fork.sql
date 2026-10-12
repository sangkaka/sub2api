-- Migration: 243_drop_platform_check_constraints_after_fork
-- 上游 242_drop_platform_check_constraints.sql 把 user_platform_quotas /
-- composite_model_routes 的平台白名单从数据库 CHECK 改到应用层清单。
-- fork 的 242_fork_platform_constraints_add_typesafe.sql 与它同号，按文件名
-- 排在 DROP 之后，会把约束加回去。本迁移排在两者之后再删一次。
--
-- channel_monitors / channel_monitor_request_templates 的 provider CHECK 表示
-- 已实现的探测能力，不属于平台清单，保留不动。
--
-- DROP ... IF EXISTS 保证可重入。已部署库若还没跑过 fork 的 242，这条也是无害的。

ALTER TABLE user_platform_quotas
    DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check;

ALTER TABLE composite_model_routes
    DROP CONSTRAINT IF EXISTS composite_model_routes_target_platform_check;
