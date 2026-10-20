package memory

import (
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

func TestMemoryPromptsScopeSilenceRules(t *testing.T) {
	for name, p := range map[string]string{
		"compaction": defaultCompactionPrompt, "consolidate": defaultConsolidatePrompt,
		"compress": defaultCompressPrompt, "light": defaultLightExtractPrompt,
		"formation": defaultFormationPrompt, "botprofile": defaultBotProfilePrompt,
	} {
		if !strings.Contains(p, llm.SilenceRuleScope) {
			t.Errorf("%s prompt missing silence-rule scope", name)
		}
	}
}
