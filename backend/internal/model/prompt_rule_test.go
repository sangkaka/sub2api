package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestPromptRuleValidateReplaceMatchMode(t *testing.T) {
	base := PromptRule{
		Name:         "replace-rule",
		Role:         PromptRoleSystem,
		Action:       PromptActionReplace,
		Content:      "new text",
		MatchPattern: "old text",
	}

	t.Run("plain is valid", func(t *testing.T) {
		rule := base
		rule.MatchMode = PromptMatchModePlain
		require.NoError(t, rule.Validate())
	})

	t.Run("regex is valid", func(t *testing.T) {
		rule := base
		rule.MatchMode = PromptMatchModeRegex
		require.NoError(t, rule.Validate())
	})

	t.Run("empty match_mode defaults to plain", func(t *testing.T) {
		rule := base
		rule.MatchMode = ""
		require.NoError(t, rule.Validate())
		require.Equal(t, PromptMatchModePlain, rule.MatchMode)
	})

	t.Run("invalid match_mode is rejected", func(t *testing.T) {
		rule := base
		rule.MatchMode = "fuzzy"
		err := rule.Validate()
		require.Error(t, err)
		var ve *ValidationError
		require.ErrorAs(t, err, &ve)
		require.Equal(t, "match_mode", ve.Field)
	})

	t.Run("invalid regex pattern is rejected", func(t *testing.T) {
		rule := base
		rule.MatchMode = PromptMatchModeRegex
		rule.MatchPattern = "[invalid("
		err := rule.Validate()
		require.Error(t, err)
		var ve *ValidationError
		require.ErrorAs(t, err, &ve)
		require.Equal(t, "match_pattern", ve.Field)
	})

	t.Run("non-system role is rejected for replace", func(t *testing.T) {
		rule := base
		rule.Role = PromptRoleUser
		rule.MatchMode = PromptMatchModePlain
		err := rule.Validate()
		require.Error(t, err)
		var ve *ValidationError
		require.ErrorAs(t, err, &ve)
		require.Equal(t, "role", ve.Field)
	})
}
