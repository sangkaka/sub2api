package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

type promptRuleRepoCapture struct {
	listParams  pagination.PaginationParams
	listFilters service.PromptRuleListFilters
}

func (r *promptRuleRepoCapture) List(context.Context) ([]*model.PromptRule, error) {
	return []*model.PromptRule{}, nil
}

func (r *promptRuleRepoCapture) ListPage(_ context.Context, params pagination.PaginationParams, filters service.PromptRuleListFilters) ([]*model.PromptRule, *pagination.PaginationResult, error) {
	r.listParams = params
	r.listFilters = filters
	return []*model.PromptRule{{ID: 1, Name: "Rule"}}, &pagination.PaginationResult{
		Total:    21,
		Page:     params.Page,
		PageSize: params.PageSize,
		Pages:    3,
	}, nil
}

func (r *promptRuleRepoCapture) GetByID(context.Context, int64) (*model.PromptRule, error) {
	return nil, nil
}

func (r *promptRuleRepoCapture) Create(_ context.Context, rule *model.PromptRule) (*model.PromptRule, error) {
	return rule, nil
}

func (r *promptRuleRepoCapture) Update(_ context.Context, rule *model.PromptRule) (*model.PromptRule, error) {
	return rule, nil
}

func (r *promptRuleRepoCapture) Delete(context.Context, int64) error {
	return nil
}

func TestAdminPromptRuleListPaginationSearchAndSort(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &promptRuleRepoCapture{}
	handler := NewPromptRuleHandler(service.NewPromptRuleService(repo, nil))
	router := gin.New()
	router.GET("/admin/prompt-rules", handler.List)

	req := httptest.NewRequest(http.MethodGet, "/admin/prompt-rules?page=2&page_size=10&search=system&sort_by=name&sort_order=DESC", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, pagination.PaginationParams{
		Page:      2,
		PageSize:  10,
		SortBy:    "name",
		SortOrder: "DESC",
	}, repo.listParams)
	require.Equal(t, "system", repo.listFilters.Search)

	var body struct {
		Code int `json:"code"`
		Data struct {
			Items    []model.PromptRule `json:"items"`
			Total    int64              `json:"total"`
			Page     int                `json:"page"`
			PageSize int                `json:"page_size"`
			Pages    int                `json:"pages"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	require.Zero(t, body.Code)
	require.Len(t, body.Data.Items, 1)
	require.Equal(t, int64(21), body.Data.Total)
	require.Equal(t, 2, body.Data.Page)
	require.Equal(t, 10, body.Data.PageSize)
	require.Equal(t, 3, body.Data.Pages)
}

func TestAdminPromptRuleListSortDefaults(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := &promptRuleRepoCapture{}
	handler := NewPromptRuleHandler(service.NewPromptRuleService(repo, nil))
	router := gin.New()
	router.GET("/admin/prompt-rules", handler.List)

	req := httptest.NewRequest(http.MethodGet, "/admin/prompt-rules", nil)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	require.Equal(t, http.StatusOK, recorder.Code)
	require.Equal(t, "order", repo.listParams.SortBy)
	require.Equal(t, "asc", repo.listParams.SortOrder)
}
