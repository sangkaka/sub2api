-- 提示词注入规则表
CREATE TABLE IF NOT EXISTS prompt_rules (
    id BIGSERIAL PRIMARY KEY,
    name VARCHAR(200) NOT NULL,
    description TEXT,
    enabled BOOLEAN NOT NULL DEFAULT true,
    "order" INTEGER NOT NULL DEFAULT 0,
    role VARCHAR(20) NOT NULL DEFAULT 'system',
    content TEXT NOT NULL,
    action VARCHAR(10) NOT NULL DEFAULT 'prepend',
    group_ids JSONB DEFAULT '[]',
    model_ids JSONB DEFAULT '[]',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_prompt_rules_enabled ON prompt_rules(enabled);
CREATE INDEX IF NOT EXISTS idx_prompt_rules_order ON prompt_rules("order");
