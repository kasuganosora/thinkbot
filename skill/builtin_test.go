package skill

import (
	"strings"
	"testing"
)

func TestBuiltinBrowserSkillUsesThinkBotTools(t *testing.T) {
	mgr := NewSkillManager(nil, nil, nil)
	RegisterBuiltins(mgr)
	sk, ok := mgr.Get("browser")
	if !ok || sk == nil {
		t.Fatal("browser skill was not registered")
	}
	if !strings.Contains(sk.Content, "browser__navigate") || !strings.Contains(sk.Content, "browser__get_text") {
		t.Fatal("skill does not name the sandbox browser tools")
	}
	if strings.Contains(sk.Content, "browser_session({") || strings.Contains(browserSkillMD, "vue") {
		t.Fatal("skill still assumes an extension runtime")
	}
}
