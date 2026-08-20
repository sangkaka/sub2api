package repository

import (
	"context"
	"strings"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/ent/promptrule"
	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/service"
)

type promptRuleRepository struct {
	client *ent.Client
}

func NewPromptRuleRepository(client *ent.Client) service.PromptRuleRepository {
	return &promptRuleRepository{client: client}
}

func (r *promptRuleRepository) List(ctx context.Context) ([]*model.PromptRule, error) {
	rules, err := r.client.PromptRule.Query().
		Order(ent.Asc(promptrule.FieldOrder)).
		All(ctx)
	if err != nil {
		return nil, err
	}

	result := make([]*model.PromptRule, len(rules))
	for i, rule := range rules {
		result[i] = r.toModel(rule)
	}
	return result, nil
}

func (r *promptRuleRepository) ListPage(
	ctx context.Context,
	params pagination.PaginationParams,
	filters service.PromptRuleListFilters,
) ([]*model.PromptRule, *pagination.PaginationResult, error) {
	query := r.client.PromptRule.Query()
	if filters.Search != "" {
		query = query.Where(
			promptrule.Or(
				promptrule.NameContainsFold(filters.Search),
				promptrule.DescriptionContainsFold(filters.Search),
				promptrule.ContentContainsFold(filters.Search),
			),
		)
	}

	total, err := query.Count(ctx)
	if err != nil {
		return nil, nil, err
	}

	itemsQuery := query.Offset(params.Offset()).Limit(params.Limit())
	for _, order := range promptRuleListOrders(params) {
		itemsQuery = itemsQuery.Order(order)
	}

	rules, err := itemsQuery.All(ctx)
	if err != nil {
		return nil, nil, err
	}

	result := make([]*model.PromptRule, len(rules))
	for i, rule := range rules {
		result[i] = r.toModel(rule)
	}
	return result, paginationResultFromTotal(int64(total), params), nil
}

func promptRuleListOrder(params pagination.PaginationParams) (string, string) {
	sortOrder := params.NormalizedSortOrder(pagination.SortOrderAsc)
	switch strings.ToLower(strings.TrimSpace(params.SortBy)) {
	case "name":
		return promptrule.FieldName, sortOrder
	case "role":
		return promptrule.FieldRole, sortOrder
	case "action":
		return promptrule.FieldAction, sortOrder
	case "enabled":
		return promptrule.FieldEnabled, sortOrder
	case "id":
		return promptrule.FieldID, sortOrder
	case "", "order":
		return promptrule.FieldOrder, sortOrder
	default:
		return promptrule.FieldOrder, pagination.SortOrderAsc
	}
}

func promptRuleListOrders(params pagination.PaginationParams) []func(*entsql.Selector) {
	field, sortOrder := promptRuleListOrder(params)
	if sortOrder == pagination.SortOrderDesc {
		if field == promptrule.FieldID {
			return []func(*entsql.Selector){ent.Desc(field)}
		}
		return []func(*entsql.Selector){ent.Desc(field), ent.Desc(promptrule.FieldID)}
	}
	if field == promptrule.FieldID {
		return []func(*entsql.Selector){ent.Asc(field)}
	}
	return []func(*entsql.Selector){ent.Asc(field), ent.Asc(promptrule.FieldID)}
}

func (r *promptRuleRepository) GetByID(ctx context.Context, id int64) (*model.PromptRule, error) {
	rule, err := r.client.PromptRule.Get(ctx, id)
	if err != nil {
		if ent.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return r.toModel(rule), nil
}

func (r *promptRuleRepository) Create(ctx context.Context, rule *model.PromptRule) (*model.PromptRule, error) {
	builder := r.client.PromptRule.Create().
		SetName(rule.Name).
		SetEnabled(rule.Enabled).
		SetOrder(rule.Order).
		SetRole(rule.Role).
		SetContent(rule.Content).
		SetAction(rule.Action)

	if rule.Description != nil {
		builder.SetDescription(*rule.Description)
	}
	if rule.MatchPattern != "" {
		builder.SetMatchPattern(rule.MatchPattern)
	}
	if rule.MatchMode != "" {
		builder.SetMatchMode(rule.MatchMode)
	}
	if len(rule.GroupIDs) > 0 {
		builder.SetGroupIds(rule.GroupIDs)
	}
	if len(rule.ModelIDs) > 0 {
		builder.SetModelIds(rule.ModelIDs)
	}

	created, err := builder.Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.toModel(created), nil
}

func (r *promptRuleRepository) Update(ctx context.Context, rule *model.PromptRule) (*model.PromptRule, error) {
	builder := r.client.PromptRule.UpdateOneID(rule.ID).
		SetName(rule.Name).
		SetEnabled(rule.Enabled).
		SetOrder(rule.Order).
		SetRole(rule.Role).
		SetContent(rule.Content).
		SetAction(rule.Action)

	if rule.Description != nil {
		builder.SetDescription(*rule.Description)
	} else {
		builder.ClearDescription()
	}
	if rule.MatchPattern != "" {
		builder.SetMatchPattern(rule.MatchPattern)
	} else {
		builder.ClearMatchPattern()
	}
	if rule.MatchMode != "" {
		builder.SetMatchMode(rule.MatchMode)
	}
	if len(rule.GroupIDs) > 0 {
		builder.SetGroupIds(rule.GroupIDs)
	} else {
		builder.ClearGroupIds()
	}
	if len(rule.ModelIDs) > 0 {
		builder.SetModelIds(rule.ModelIDs)
	} else {
		builder.ClearModelIds()
	}

	updated, err := builder.Save(ctx)
	if err != nil {
		return nil, err
	}
	return r.toModel(updated), nil
}

func (r *promptRuleRepository) Delete(ctx context.Context, id int64) error {
	return r.client.PromptRule.DeleteOneID(id).Exec(ctx)
}

func (r *promptRuleRepository) toModel(e *ent.PromptRule) *model.PromptRule {
	rule := &model.PromptRule{
		ID:        int64(e.ID),
		Name:      e.Name,
		Enabled:   e.Enabled,
		Order:     e.Order,
		Role:      e.Role,
		Content:   e.Content,
		Action:    e.Action,
		MatchMode: e.MatchMode,
		GroupIDs:  e.GroupIds,
		ModelIDs:  e.ModelIds,
		CreatedAt: e.CreatedAt,
		UpdatedAt: e.UpdatedAt,
	}
	if e.Description != nil {
		rule.Description = e.Description
	}
	if e.MatchPattern != nil {
		rule.MatchPattern = *e.MatchPattern
	}
	if rule.GroupIDs == nil {
		rule.GroupIDs = []int64{}
	}
	if rule.ModelIDs == nil {
		rule.ModelIDs = []string{}
	}
	return rule
}
