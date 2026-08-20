package repository

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/enttest"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/stretchr/testify/require"

	"entgo.io/ent/dialect"
	entsql "entgo.io/ent/dialect/sql"
	_ "modernc.org/sqlite"
)

func newPromptRuleRepoSQLite(t *testing.T) (*promptRuleRepository, *ent.Client) {
	t.Helper()

	db, err := sql.Open("sqlite", fmt.Sprintf("file:%s?mode=memory&cache=shared&_fk=1", t.Name()))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec("PRAGMA foreign_keys = ON")
	require.NoError(t, err)

	driver := entsql.OpenDB(dialect.SQLite, db)
	client := enttest.NewClient(t, enttest.WithOptions(ent.Driver(driver)))
	t.Cleanup(func() { _ = client.Close() })
	return &promptRuleRepository{client: client}, client
}

func promptRuleForListTest(name, content string, description *string, order int) *model.PromptRule {
	return &model.PromptRule{
		Name:        name,
		Description: description,
		Enabled:     true,
		Order:       order,
		Role:        model.PromptRoleSystem,
		Content:     content,
		Action:      model.PromptActionPrepend,
	}
}

func TestPromptRuleRepositoryListPageSearchAndPagination(t *testing.T) {
	repo, _ := newPromptRuleRepoSQLite(t)
	ctx := context.Background()
	firstDescription := "ordinary description"
	needleDescription := "contains NEEDLE in description"

	_, err := repo.Create(ctx, promptRuleForListTest("Alpha", "ordinary content", &firstDescription, 3))
	require.NoError(t, err)
	_, err = repo.Create(ctx, promptRuleForListTest("Beta needle", "ordinary content", nil, 2))
	require.NoError(t, err)
	_, err = repo.Create(ctx, promptRuleForListTest("Gamma", "ordinary content", &needleDescription, 1))
	require.NoError(t, err)
	_, err = repo.Create(ctx, promptRuleForListTest("Delta", "needle in content", nil, 4))
	require.NoError(t, err)

	params := pagination.PaginationParams{Page: 1, PageSize: 2, SortBy: "name", SortOrder: "asc"}
	items, pageResult, err := repo.ListPage(ctx, params, service.PromptRuleListFilters{Search: "needle"})
	require.NoError(t, err)
	require.Equal(t, int64(3), pageResult.Total)
	require.Equal(t, 2, pageResult.Pages)
	require.Len(t, items, 2)
	require.Equal(t, []string{"Beta needle", "Delta"}, []string{items[0].Name, items[1].Name})

	params.Page = 2
	items, pageResult, err = repo.ListPage(ctx, params, service.PromptRuleListFilters{Search: "needle"})
	require.NoError(t, err)
	require.Equal(t, int64(3), pageResult.Total)
	require.Len(t, items, 1)
	require.Equal(t, "Gamma", items[0].Name)
}

func TestPromptRuleListOrder(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		params pagination.PaginationParams
		wantBy string
		want   string
	}{
		{name: "default order asc", params: pagination.PaginationParams{}, wantBy: "order", want: "asc"},
		{name: "name desc", params: pagination.PaginationParams{SortBy: "name", SortOrder: "DESC"}, wantBy: "name", want: "desc"},
		{name: "role asc", params: pagination.PaginationParams{SortBy: "role", SortOrder: "asc"}, wantBy: "role", want: "asc"},
		{name: "action desc", params: pagination.PaginationParams{SortBy: "action", SortOrder: "desc"}, wantBy: "action", want: "desc"},
		{name: "enabled asc", params: pagination.PaginationParams{SortBy: "enabled", SortOrder: "asc"}, wantBy: "enabled", want: "asc"},
		{name: "invalid falls back", params: pagination.PaginationParams{SortBy: "content", SortOrder: "desc"}, wantBy: "order", want: "asc"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			gotBy, gotOrder := promptRuleListOrder(tt.params)
			if gotBy != tt.wantBy || gotOrder != tt.want {
				t.Fatalf("promptRuleListOrder(%+v) = (%q, %q), want (%q, %q)", tt.params, gotBy, gotOrder, tt.wantBy, tt.want)
			}
		})
	}
}
