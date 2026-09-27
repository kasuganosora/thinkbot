package bot

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/sandbox"
	"github.com/kasuganosora/thinkbot/skill"
)

// workspaceSkillFS adapts the bot's sandbox workspace to skill.WorkspaceFS so
// skills the bot installs under <workspace>/skills/<name>/SKILL.md (docker
// mode: /data/skills inside the per-bot container) are discovered.
type workspaceSkillFS struct {
	ws sandbox.Workspace
}

var _ skill.WorkspaceFS = workspaceSkillFS{}

func (f workspaceSkillFS) WorkDir() string { return f.ws.WorkDir() }

func (f workspaceSkillFS) Exec(ctx context.Context, command string) (string, int, error) {
	res, err := f.ws.Exec(ctx, sandbox.ExecRequest{Command: command, Timeout: 15 * time.Second})
	if err != nil {
		return "", -1, err
	}
	return res.Stdout, res.ExitCode, nil
}

func (f workspaceSkillFS) ReadFile(ctx context.Context, path string) ([]byte, error) {
	return f.ws.ReadFile(ctx, path)
}

// attachWorkspaceSkills hooks the workspace skill source into mgr and runs the
// first scan in the background (it goes through docker exec).
func attachWorkspaceSkills(mgr *skill.SkillManager, ws sandbox.Workspace, logger skill.Logger) {
	if mgr == nil || ws == nil {
		return
	}
	src := skill.NewWorkspaceSource(workspaceSkillFS{ws: ws}, mgr, logger)
	src.Attach()
	go src.Refresh(context.Background())
}
