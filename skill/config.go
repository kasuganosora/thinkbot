package skill

import "context"

// ============================================================================
// StoreAdapter 的默认实现（适配 config.Store）
//
// 此文件提供 StoreAdapter 接口的默认实现，
// 由 agent 层在初始化时将 *config.Store 适配为 StoreAdapter。
//
// 用法：
//
//	adapter := skill.NewConfigStoreAdapter(store)
//	mgr.SetStore(adapter)
//
// ============================================================================

// ConfigStoreAdapter 将 *config.Store 适配为 StoreAdapter 接口。
type ConfigStoreAdapter struct {
	store interface {
		Get(key string) (string, bool)
		Set(ctx context.Context, key, value string) error
		GetBool(key string, def bool) bool
	}
}

// NewConfigStoreAdapter 创建 ConfigStoreAdapter。
// store 必须实现 Get/Set/GetBool 方法（*config.Store 天然满足）。
func NewConfigStoreAdapter(store interface {
	Get(key string) (string, bool)
	Set(ctx context.Context, key, value string) error
	GetBool(key string, def bool) bool
}) *ConfigStoreAdapter {
	return &ConfigStoreAdapter{store: store}
}

func (a *ConfigStoreAdapter) Get(key string) (string, bool) {
	return a.store.Get(key)
}

func (a *ConfigStoreAdapter) Set(ctx context.Context, key, value string) error {
	return a.store.Set(ctx, key, value)
}

func (a *ConfigStoreAdapter) GetBool(key string, def bool) bool {
	return a.store.GetBool(key, def)
}

// ============================================================================
// 持久化辅助方法（SkillManager 扩展）
// ============================================================================

// LoadEnabledStates 从 Store 加载所有 Skill 的启用状态并应用到 Manager。
// 在 Manager 注册完所有 Skill 后调用。
func (m *SkillManager) LoadEnabledStates() {
	if m.store == nil {
		return
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	for name, skill := range m.skills {
		key := "skill." + name + ".enabled"
		if val, ok := m.store.Get(key); ok {
			skill.Enabled = (val == "true")
			// 同步注入/移除 prompt
			if skill.Enabled && skill.Content != "" {
				m.registerPromptLocked(skill)
			} else {
				m.unregisterPromptLocked(name)
			}
		}
	}
}

// SaveEnabledStates 将所有 Skill 的启用状态持久化到 Store。
func (m *SkillManager) SaveEnabledStates(ctx context.Context) {
	if m.store == nil {
		return
	}

	m.mu.RLock()
	names := make([]string, 0, len(m.skills))
	for name := range m.skills {
		names = append(names, name)
	}
	skills := make([]*Skill, 0, len(names))
	for _, name := range names {
		if s, ok := m.skills[name]; ok {
			skills = append(skills, s)
		}
	}
	m.mu.RUnlock()

	for _, skill := range skills {
		key := "skill." + skill.Name + ".enabled"
		val := "false"
		if skill.Enabled {
			val = "true"
		}
		if err := m.store.Set(ctx, key, val); err != nil {
			m.logger.Warnw("failed to save skill enabled state",
				"name", skill.Name, "error", err)
		}
	}
}
