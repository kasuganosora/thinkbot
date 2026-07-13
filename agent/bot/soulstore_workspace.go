package bot

import (
	"context"
	"path/filepath"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/sandbox"
)

// workspaceSoulStore 让 SoulLoader 通过 bot 工作空间抽象读写 SOUL.md，
// 从而能访问 docker 持久容器（DooD）模式下 bot 容器内 named volume 中的真实
// 文件（/data/SOUL.md），而非主程序侧空目录 —— 单一数据源，agent 自改可持久化。
//
// 路径采用工作空间内相对路径（默认 "SOUL.md"，即容器 /data/SOUL.md）。
// 仅在 wsMgr.Backend()=="docker" 时注入；local 模式仍走主程序侧宿主路径（osSoulStore）。
type workspaceSoulStore struct {
	ws   sandbox.Workspace
	path string
}

// NewWorkspaceSoulStore 构造基于 sandbox.Workspace 的 SoulStore。
// relPath 为工作空间内相对路径，默认 "SOUL.md"。
func NewWorkspaceSoulStore(ws sandbox.Workspace, relPath string) prompt.SoulStore {
	if relPath == "" {
		relPath = "SOUL.md"
	}
	return &workspaceSoulStore{ws: ws, path: relPath}
}

func (s *workspaceSoulStore) ReadSoul(ctx context.Context, _ string) ([]byte, error) {
	return s.ws.ReadFile(ctx, s.path)
}

func (s *workspaceSoulStore) WriteSoul(ctx context.Context, _ string, data []byte) error {
	return s.ws.WriteFile(ctx, s.path, data)
}

// StatSoul 经 ListDir 父目录 + 比对文件名取 ModTime（容器模式 ListDir 会填 ModTime）。
func (s *workspaceSoulStore) StatSoul(ctx context.Context, _ string) prompt.SoulStat {
	parent := filepath.Dir(s.path)
	if parent == "." || parent == "" {
		parent = "."
	}
	entries, err := s.ws.ListDir(ctx, parent)
	if err != nil {
		return prompt.SoulStat{Err: err}
	}
	name := filepath.Base(s.path)
	for _, e := range entries {
		if e.Name == name {
			return prompt.SoulStat{Exists: true, ModTime: e.ModTime}
		}
	}
	return prompt.SoulStat{Exists: false}
}
