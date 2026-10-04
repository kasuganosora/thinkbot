package skill

import _ "embed"

//go:embed builtin/browser.md
var browserSkillMD string

// RegisterBuiltins adds skills compiled into the binary.
// The skills/ directory is runtime data and is not the source of these.
func RegisterBuiltins(mgr *SkillManager) {
	if mgr == nil {
		return
	}
	sk, err := newSkillFromContent(browserSkillMD, "bundled", "builtin/browser")
	if err != nil {
		return
	}
	mgr.Register(sk)
}
