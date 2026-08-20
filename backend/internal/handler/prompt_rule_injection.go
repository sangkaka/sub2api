package handler

import (
	"github.com/Wei-Shaw/sub2api/internal/service"
	"go.uber.org/zap"
)

func injectMatchingPromptRules(
	reqLog *zap.Logger,
	promptRuleService *service.PromptRuleService,
	groupID *int64,
	model string,
	protocol service.PromptRuleProtocol,
	body []byte,
) []byte {
	if promptRuleService == nil {
		return body
	}

	prepend, appendRules, replaceRules := promptRuleService.GetMatchingRules(groupID, model)
	if len(prepend) == 0 && len(appendRules) == 0 && len(replaceRules) == 0 {
		return body
	}

	result := service.InjectPromptRules(protocol, body, prepend, appendRules, replaceRules)
	if reqLog != nil {
		for _, skipped := range result.Skipped {
			reqLog.Warn("prompt_rule.skipped",
				zap.Int64("rule_id", skipped.RuleID),
				zap.String("role", skipped.Role),
				zap.String("action", skipped.Action),
				zap.String("protocol", string(protocol)),
				zap.String("model", model),
				zap.String("reason", skipped.Reason),
			)
		}
	}
	return result.Body
}
