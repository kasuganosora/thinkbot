package llm

import (
	"strings"
	"testing"
)

func TestSelfCompactPromptScopesSilenceRules(t *testing.T) {
	if !strings.HasSuffix(SelfCompactSystemPrompt, SilenceRuleScope) || !strings.Contains(SilenceRuleScope, "directly @mentioned") {
		t.Fatal("self-compact prompt must carry the silence-rule scope")
	}
}
