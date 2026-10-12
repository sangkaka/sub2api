package migrations

import (
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// expectedCompositeRouteTargetPlatforms 是 composite_model_routes.target_platform
// 允许的平台集合，需与 service.isConcreteRequestPlatform
// （internal/service/composite_platform.go）同步。
// 此处硬编码而非 import service：migrations 是叶子包，被 repository 依赖，
// 反向 import 会成环。
var expectedCompositeRouteTargetPlatforms = []string{
	"adobe", "anthropic", "antigravity", "deepseek", "gemini", "grok", "kimi", "kiro", "minimax", "openai", "opencode_go", "typesafe", "zhipu",
}

const compositeRouteTargetPlatformConstraint = "composite_model_routes_target_platform_check"

// TestCompositeRoutesKiroMigration 校验 229 号迁移把 227 漏掉的 kiro 加进
// composite_model_routes.target_platform 的 CHECK 约束。
//
// 227 用 DROP + 重建的写法把国产 3 家加入该约束时，重建的列表里没有本 fork
// 独有的 kiro，导致管理端保存 target_platform='kiro' 的 composite 路由违约失败。
func TestCompositeRoutesKiroMigration(t *testing.T) {
	content, err := FS.ReadFile("229_composite_routes_add_kiro.sql")
	require.NoError(t, err)

	sql := strings.Join(strings.Fields(string(content)), " ")
	require.Contains(t, sql, "DROP CONSTRAINT IF EXISTS "+compositeRouteTargetPlatformConstraint)
	require.Contains(t, sql,
		"CHECK (target_platform IN ('anthropic', 'openai', 'gemini', 'antigravity', 'kiro', 'grok', 'kimi', 'zhipu', 'deepseek'))")
}

// TestCompositeRouteTargetPlatformFinalStateCoversAllPlatforms 是防回归护栏。
//
// 243 起 composite_model_routes.target_platform 不再由数据库 CHECK 约束，
// 改由应用层 domain.IsConcretePlatform 校验。终态是最后一个触碰该约束的迁移
// 必须是这次 DROP。DROP 之前最后一个重建仍须覆盖当时的全部允许平台。
func TestCompositeRouteTargetPlatformFinalStateCoversAllPlatforms(t *testing.T) {
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
		if strings.Contains(string(body), compositeRouteTargetPlatformConstraint) {
			touching = append(touching, name)
		}
	}
	require.NotEmpty(t, touching, "no migration touches %s", compositeRouteTargetPlatformConstraint)
	sort.Strings(touching) // 与 ApplyMigrations 的执行顺序一致
	require.Equal(t, droppedAt, touching[len(touching)-1],
		"243 之后不得再触碰 composite_model_routes_target_platform_check")

	var lastRebuild string
	for _, name := range touching {
		if name >= droppedAt {
			break
		}
		body, readErr := FS.ReadFile(name)
		require.NoError(t, readErr)
		if strings.Contains(string(body), "ADD CONSTRAINT "+compositeRouteTargetPlatformConstraint) {
			lastRebuild = name
		}
	}
	require.NotEmpty(t, lastRebuild)

	body, err := FS.ReadFile(lastRebuild)
	require.NoError(t, err)
	normalized := strings.Join(strings.Fields(string(body)), " ")

	checkRe := regexp.MustCompile(`CHECK \(target_platform IN \(([^)]*)\)\)`)
	matches := checkRe.FindAllStringSubmatch(normalized, -1)
	require.NotEmpty(t, matches,
		"migration %s rebuilds the constraint but has no CHECK (target_platform IN (...))", lastRebuild)

	var got []string
	for _, raw := range strings.Split(matches[len(matches)-1][1], ",") {
		if v := strings.Trim(strings.TrimSpace(raw), "'"); v != "" {
			got = append(got, v)
		}
	}
	sort.Strings(got)

	require.Equal(t, expectedCompositeRouteTargetPlatforms, got,
		"migration %s 定义的平台列表与 243 之前的允许平台不一致；"+
			"重建 CHECK 约束时必须列出当时的全部允许平台，漏项会让对应平台的 composite 路由无法保存", lastRebuild)
}
