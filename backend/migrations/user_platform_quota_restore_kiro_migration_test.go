package migrations

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedUserPlatformQuotaPlatforms 是 242 删除数据库 CHECK 之前，
// user_platform_quotas.platform 允许的平台集合。242 之后合法性改由应用层
// domain.IsConcretePlatform / service.AllowedQuotaPlatforms 校验，终态护栏见
// TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms。
// 此处硬编码而非 import service：migrations 是叶子包，被 repository 依赖，反向 import 会成环。
var expectedUserPlatformQuotaPlatforms = []string{
	"adobe", "anthropic", "antigravity", "deepseek", "gemini", "grok", "kimi", "kiro", "minimax", "openai", "opencode_go", "typesafe", "zhipu",
}

// TestUserPlatformQuotasRestoreKiroMigration 校验 227 号迁移把 224 漏掉的 kiro
// 重新加回 user_platform_quotas.platform 的 CHECK 约束。
//
// kiro 自 145 号迁移起就在约束内，224 号用 DROP + 重建的写法加国产 3 家时漏掉了它，
// 等于回退了 145。约束缺 kiro 时，注册预填充 9 平台默认配额会因该行违约中止整条
// INSERT → 快照 fail-open 仅 warn → 新用户零配额行（缺失配额行 = 无限额）。
func TestUserPlatformQuotasRestoreKiroMigration(t *testing.T) {
	content, err := FS.ReadFile("227_user_platform_quotas_restore_kiro.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS user_platform_quotas_platform_check")
	require.Contains(t, sql,
		"CHECK (platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek'))")
}

// TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms 是防回归护栏。
//
// 242_drop_platform_check_constraints.sql 起，user_platform_quotas.platform 不再由
// 数据库 CHECK 白名单约束，改由应用层校验。终态因此是：按文件名顺序，最后一个
// 触碰该约束的迁移必须是这次 DROP，且其后不得再 ADD 回去。
//
// DROP 之前最后一个重建该约束的迁移仍须覆盖当时的全部允许平台（224 的事故根因
// 就是 DROP + 重建时手抄列表漏项）。新平台只登记应用层清单，不再改这条 SQL 列表。
func TestUserPlatformQuotaPlatformCheckFinalStateCoversAllPlatforms(t *testing.T) {
	const droppedAt = "243_drop_platform_check_constraints_after_fork.sql"
	entries, err := FS.ReadDir(".")
	require.NoError(t, err)

	var touching []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".sql") {
			continue
		}
		body, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		if strings.Contains(string(body), "user_platform_quotas_platform_check") {
			touching = append(touching, name)
		}
	}
	require.NotEmpty(t, touching, "no migration touches user_platform_quotas_platform_check")
	sort.Strings(touching) // 与 ApplyMigrations 的执行顺序一致
	require.Equal(t, droppedAt, touching[len(touching)-1],
		"242 之后不得再触碰 user_platform_quotas_platform_check；平台合法性由应用层清单校验")

	var lastRebuild string
	for _, name := range touching {
		if name >= droppedAt {
			break
		}
		body, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		if strings.Contains(string(body), "ADD CONSTRAINT user_platform_quotas_platform_check") {
			lastRebuild = name
		}
	}
	require.NotEmpty(t, lastRebuild, "no migration rebuilds user_platform_quotas_platform_check before 242")

	body, err := FS.ReadFile(lastRebuild)
	require.NoError(t, err)
	normalized := strings.Join(strings.Fields(string(body)), " ")

	checkRe := regexp.MustCompile(`CHECK \(platform IN \(([^)]*)\)\)`)
	matches := checkRe.FindAllStringSubmatch(normalized, -1)
	require.NotEmpty(t, matches, "migration %s rebuilds the constraint but has no CHECK (platform IN (...))", lastRebuild)

	var got []string
	for _, raw := range strings.Split(matches[len(matches)-1][1], ",") {
		if v := strings.Trim(strings.TrimSpace(raw), "'"); v != "" {
			got = append(got, v)
		}
	}
	sort.Strings(got)

	require.Equal(t, expectedUserPlatformQuotaPlatforms, got,
		"migration %s 定义的平台列表与 242 之前的允许平台不一致；"+
			"重建 CHECK 约束时必须列出当时的全部允许平台，漏项会让对应平台的配额行插入失败", lastRebuild)
}
