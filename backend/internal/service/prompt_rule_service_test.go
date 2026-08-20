package service

import (
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/model"
	"github.com/stretchr/testify/require"
)

func TestPromptRuleServiceGetMatchingRules(t *testing.T) {
	groupID := int64(7)

	tests := []struct {
		name    string
		rule    *model.PromptRule
		modelID string
		want    bool
	}{
		{
			name: "empty groups do not match any request",
			rule: &model.PromptRule{
				Enabled:  true,
				Action:   model.PromptActionPrepend,
				GroupIDs: []int64{},
				ModelIDs: []string{},
			},
			modelID: "claude-sonnet-4-6",
			want:    false,
		},
		{
			name: "empty models match every model in the selected group",
			rule: &model.PromptRule{
				Enabled:  true,
				Action:   model.PromptActionPrepend,
				GroupIDs: []int64{groupID},
				ModelIDs: []string{},
			},
			modelID: "claude-sonnet-4-6",
			want:    true,
		},
		{
			name: "configured models require an exact match",
			rule: &model.PromptRule{
				Enabled:  true,
				Action:   model.PromptActionPrepend,
				GroupIDs: []int64{groupID},
				ModelIDs: []string{"claude-opus-4-6"},
			},
			modelID: "claude-sonnet-4-6",
			want:    false,
		},
		{
			name: "disabled rules never match",
			rule: &model.PromptRule{
				Enabled:  false,
				Action:   model.PromptActionPrepend,
				GroupIDs: []int64{groupID},
				ModelIDs: []string{},
			},
			modelID: "claude-sonnet-4-6",
			want:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := &PromptRuleService{localCache: []*model.PromptRule{tt.rule}}

			prepend, appendRules, replaceRules := svc.GetMatchingRules(&groupID, tt.modelID)

			if tt.want {
				require.Len(t, prepend, 1)
			} else {
				require.Empty(t, prepend)
			}
			require.Empty(t, appendRules)
			require.Empty(t, replaceRules)
		})
	}
}
