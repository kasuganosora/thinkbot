package config

import "testing"

// TestGlobalMetaSpecsExposesSandboxCPULimit 守住两件事：
//  1. 系统设置页（GET /api/config）必须能看到 sandbox.cpu_limit，否则宿主级 CPU 配额
//     只能靠改库或 docker update，面板里改不了；
//  2. 不能顺手把 WorkspaceMetaSpecs 整包放进来——那里面有 channel 密钥，
//     还有镜像名与后端选择，改错会直接让 bot 容器起不来。
func TestGlobalMetaSpecsExposesSandboxCPULimit(t *testing.T) {
	keys := make(map[string]bool)
	for _, spec := range GlobalMetaSpecs() {
		keys[spec.Key] = true
	}

	if !keys[KeySandboxCPULimit] {
		t.Errorf("GlobalMetaSpecs() missing %q: the settings page cannot edit the sandbox CPU limit", KeySandboxCPULimit)
	}

	mustNotLeak := []string{KeySandboxChannelSecret, KeySandboxImage, KeySandboxBackend}
	for _, k := range mustNotLeak {
		if keys[k] {
			t.Errorf("GlobalMetaSpecs() should not expose %q on the generic settings page", k)
		}
	}
}
