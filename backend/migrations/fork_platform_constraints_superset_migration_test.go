package migrations

import (
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestForkPlatformConstraintsSupersetMigration(t *testing.T) {
	const name = "242_fork_platform_constraints_add_typesafe.sql"
	content, err := FS.ReadFile(name)
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'kiro', 'minimax', 'adobe', 'opencode_go', 'typesafe'))")
	require.Contains(t, sql,
		"CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'grok', 'kimi', 'zhipu', 'deepseek', 'kiro', 'minimax', 'adobe', 'opencode_go', 'typesafe'))")
	require.Contains(t, sql,
		"ADD CONSTRAINT channel_monitors_provider_check CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'))")
	require.Contains(t, sql,
		"ADD CONSTRAINT channel_monitor_request_templates_provider_check CHECK (provider IN ('openai', 'anthropic', 'gemini', 'grok', 'antigravity', 'kiro', 'kimi', 'zhipu', 'deepseek', 'minimax', 'opencode_go'))")

	// 242_drop 删除配额/组合路由的平台 CHECK 之后，本文件只应是 DROP 之前最后一个
	// 重建这两处约束的迁移。渠道监控的 provider CHECK 仍由数据库约束，本文件目前
	// 仍是它的终态；后续若有新迁移重建 channel_monitors_provider_check，必须继续
	// 覆盖 kiro（不含 adobe，与 239 一致）。
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)
	var quotaTouching []string
	var monitorTouching []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		body, err := FS.ReadFile(e.Name())
		require.NoError(t, err)
		text := string(body)
		if strings.Contains(text, "ADD CONSTRAINT user_platform_quotas_platform_check") ||
			strings.Contains(text, "ADD CONSTRAINT composite_model_routes_target_platform_check") {
			quotaTouching = append(quotaTouching, e.Name())
		}
		if strings.Contains(text, "ADD CONSTRAINT channel_monitors_provider_check") {
			monitorTouching = append(monitorTouching, e.Name())
		}
	}
	sort.Strings(quotaTouching)
	sort.Strings(monitorTouching)
	require.NotEmpty(t, quotaTouching)
	require.Equal(t, name, quotaTouching[len(quotaTouching)-1])
	require.NotEmpty(t, monitorTouching)
	require.Equal(t, name, monitorTouching[len(monitorTouching)-1])
}
