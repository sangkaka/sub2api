-- 188_prompt_rule_replace_action.sql
-- 为 prompt_rules 表添加 replace 动作支持，需要 match_pattern 和 match_mode 字段

ALTER TABLE prompt_rules ADD COLUMN IF NOT EXISTS match_pattern TEXT;
ALTER TABLE prompt_rules ADD COLUMN IF NOT EXISTS match_mode VARCHAR(10) DEFAULT 'plain';
