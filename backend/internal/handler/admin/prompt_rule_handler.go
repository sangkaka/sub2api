package admin

import (
	"strconv"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/response"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
)

type PromptRuleHandler struct {
	service *service.PromptRuleService
}

func NewPromptRuleHandler(service *service.PromptRuleService) *PromptRuleHandler {
	return &PromptRuleHandler{service: service}
}

type CreatePromptRuleRequest struct {
	Name         string   `json:"name" binding:"required"`
	Description  *string  `json:"description"`
	Enabled      *bool    `json:"enabled"`
	Order        int      `json:"order"`
	Role         string   `json:"role"`
	Content      string   `json:"content" binding:"required"`
	Action       string   `json:"action"`
	MatchPattern string   `json:"match_pattern"`
	MatchMode    string   `json:"match_mode"`
	GroupIDs     []int64  `json:"group_ids"`
	ModelIDs     []string `json:"model_ids"`
}

type UpdatePromptRuleRequest struct {
	Name         *string  `json:"name"`
	Description  *string  `json:"description"`
	Enabled      *bool    `json:"enabled"`
	Order        *int     `json:"order"`
	Role         *string  `json:"role"`
	Content      *string  `json:"content"`
	Action       *string  `json:"action"`
	MatchPattern *string  `json:"match_pattern"`
	MatchMode    *string  `json:"match_mode"`
	GroupIDs     []int64  `json:"group_ids"`
	ModelIDs     []string `json:"model_ids"`
}

// List GET /api/v1/admin/prompt-rules
func (h *PromptRuleHandler) List(c *gin.Context) {
	page, pageSize := response.ParsePagination(c)
	search := strings.TrimSpace(c.Query("search"))
	if len(search) > 200 {
		search = search[:200]
	}
	params := pagination.PaginationParams{
		Page:      page,
		PageSize:  pageSize,
		SortBy:    c.DefaultQuery("sort_by", "order"),
		SortOrder: c.DefaultQuery("sort_order", "asc"),
	}
	rules, paginationResult, err := h.service.ListPage(
		c.Request.Context(),
		params,
		service.PromptRuleListFilters{Search: search},
	)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Paginated(c, rules, paginationResult.Total, page, pageSize)
}

// GetByID GET /api/v1/admin/prompt-rules/:id
func (h *PromptRuleHandler) GetByID(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid rule ID")
		return
	}

	rule, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if rule == nil {
		response.NotFound(c, "Rule not found")
		return
	}
	response.Success(c, rule)
}

// Create POST /api/v1/admin/prompt-rules
func (h *PromptRuleHandler) Create(c *gin.Context) {
	var req CreatePromptRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	rule := &model.PromptRule{
		Name:         req.Name,
		Content:      req.Content,
		Order:        req.Order,
		MatchPattern: req.MatchPattern,
	}

	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	} else {
		rule.Enabled = true
	}
	if req.Role != "" {
		rule.Role = req.Role
	} else {
		rule.Role = model.PromptRoleSystem
	}
	if req.Action != "" {
		rule.Action = req.Action
	} else {
		rule.Action = model.PromptActionPrepend
	}
	if req.MatchMode != "" {
		rule.MatchMode = req.MatchMode
	} else {
		rule.MatchMode = model.PromptMatchModePlain
	}
	rule.Description = req.Description
	if req.GroupIDs != nil {
		rule.GroupIDs = req.GroupIDs
	} else {
		rule.GroupIDs = []int64{}
	}
	if req.ModelIDs != nil {
		rule.ModelIDs = req.ModelIDs
	} else {
		rule.ModelIDs = []string{}
	}

	created, err := h.service.Create(c.Request.Context(), rule)
	if err != nil {
		if _, ok := err.(*model.ValidationError); ok {
			response.BadRequest(c, err.Error())
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, created)
}

// Update PUT /api/v1/admin/prompt-rules/:id
func (h *PromptRuleHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid rule ID")
		return
	}

	var req UpdatePromptRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		response.BadRequest(c, "Invalid request: "+err.Error())
		return
	}

	existing, err := h.service.GetByID(c.Request.Context(), id)
	if err != nil {
		response.ErrorFrom(c, err)
		return
	}
	if existing == nil {
		response.NotFound(c, "Rule not found")
		return
	}

	rule := &model.PromptRule{
		ID:           id,
		Name:         existing.Name,
		Description:  existing.Description,
		Enabled:      existing.Enabled,
		Order:        existing.Order,
		Role:         existing.Role,
		Content:      existing.Content,
		Action:       existing.Action,
		MatchPattern: existing.MatchPattern,
		MatchMode:    existing.MatchMode,
		GroupIDs:     existing.GroupIDs,
		ModelIDs:     existing.ModelIDs,
	}

	if req.Name != nil {
		rule.Name = *req.Name
	}
	if req.Description != nil {
		rule.Description = req.Description
	}
	if req.Enabled != nil {
		rule.Enabled = *req.Enabled
	}
	if req.Order != nil {
		rule.Order = *req.Order
	}
	if req.Role != nil {
		rule.Role = *req.Role
	}
	if req.Content != nil {
		rule.Content = *req.Content
	}
	if req.Action != nil {
		rule.Action = *req.Action
	}
	if req.MatchPattern != nil {
		rule.MatchPattern = *req.MatchPattern
	}
	if req.MatchMode != nil {
		rule.MatchMode = *req.MatchMode
	}
	if req.GroupIDs != nil {
		rule.GroupIDs = req.GroupIDs
	}
	if req.ModelIDs != nil {
		rule.ModelIDs = req.ModelIDs
	}

	updated, err := h.service.Update(c.Request.Context(), rule)
	if err != nil {
		if _, ok := err.(*model.ValidationError); ok {
			response.BadRequest(c, err.Error())
			return
		}
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, updated)
}

// Delete DELETE /api/v1/admin/prompt-rules/:id
func (h *PromptRuleHandler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.BadRequest(c, "Invalid rule ID")
		return
	}

	if err := h.service.Delete(c.Request.Context(), id); err != nil {
		response.ErrorFrom(c, err)
		return
	}
	response.Success(c, gin.H{"message": "Rule deleted successfully"})
}
