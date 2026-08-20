package model

import (
	"regexp"
	"time"
)

const (
	PromptActionPrepend = "prepend"
	PromptActionAppend  = "append"
	PromptActionReplace = "replace"

	PromptRoleSystem    = "system"
	PromptRoleUser      = "user"
	PromptRoleAssistant = "assistant"

	PromptMatchModePlain = "plain"
	PromptMatchModeRegex = "regex"
)

// PromptRule 提示词注入规则
type PromptRule struct {
	ID           int64     `json:"id"`
	Name         string    `json:"name"`
	Description  *string   `json:"description,omitempty"`
	Enabled      bool      `json:"enabled"`
	Order        int       `json:"order"`
	Role         string    `json:"role"`
	Content      string    `json:"content"`
	Action       string    `json:"action"`
	MatchPattern string    `json:"match_pattern,omitempty"`
	MatchMode    string    `json:"match_mode,omitempty"`
	GroupIDs     []int64   `json:"group_ids"`
	ModelIDs     []string  `json:"model_ids"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

func (r *PromptRule) Validate() error {
	if r.Name == "" {
		return &ValidationError{Field: "name", Message: "name is required"}
	}
	if r.Content == "" {
		return &ValidationError{Field: "content", Message: "content is required"}
	}
	if r.Role != PromptRoleSystem && r.Role != PromptRoleUser && r.Role != PromptRoleAssistant {
		return &ValidationError{Field: "role", Message: "role must be 'system', 'user', or 'assistant'"}
	}
	if r.Action != PromptActionPrepend && r.Action != PromptActionAppend && r.Action != PromptActionReplace {
		return &ValidationError{Field: "action", Message: "action must be 'prepend', 'append', or 'replace'"}
	}
	if r.Action == PromptActionReplace {
		if r.Role != PromptRoleSystem {
			return &ValidationError{Field: "role", Message: "role must be 'system' when action is 'replace'"}
		}
		if r.MatchPattern == "" {
			return &ValidationError{Field: "match_pattern", Message: "match_pattern is required when action is 'replace'"}
		}
		if r.MatchMode == "" {
			r.MatchMode = PromptMatchModePlain
		}
		if r.MatchMode != PromptMatchModePlain && r.MatchMode != PromptMatchModeRegex {
			return &ValidationError{Field: "match_mode", Message: "match_mode must be 'plain' or 'regex'"}
		}
		if r.MatchMode == PromptMatchModeRegex {
			if _, err := regexp.Compile(r.MatchPattern); err != nil {
				return &ValidationError{Field: "match_pattern", Message: "invalid regex: " + err.Error()}
			}
		}
	}
	return nil
}
