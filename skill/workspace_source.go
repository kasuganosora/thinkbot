package skill

import (
	"context"
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// ============================================================================
// WorkspaceSource — skills the bot installed in its own sandbox workspace
//
// Bundled skills (repo skills/, mounted read-only at /app/skills) and managed
// skills (data/skills/<bot>, written by the admin UI) live in the main
// process's filesystem. The bot itself can only write inside its sandbox
// workspace (docker mode: the per-bot container's /data volume), so skills it
// installs on its own end up in <workspace>/skills/<name>/SKILL.md — on prod
// 12 of them (ctrip-wendao, amap-*, bangumi, ...) sat in /data/skills and
// skill_search never saw them.
//
// WorkspaceSource lists <workspace>/skills/*/SKILL.md through the sandbox
// (one exec per refresh), reads only new/changed SKILL.md files, and
// registers them with Source="workspace" and Dir=<absolute path inside the
// sandbox>, so exec/run_code can run the skill's scripts where they are.
// Bundled/managed skills win on a name clash (admin-curated), vanished
// workspace skills are unregistered. Refresh is throttled and runs at bot
// start and lazily before skill_search / use_skill.
// ============================================================================

// SourceWorkspace is Skill.Source for skills found in the bot's workspace.
const SourceWorkspace = "workspace"

// WorkspaceSkillsDir is the workspace-relative directory scanned for skills.
const WorkspaceSkillsDir = "skills"

const (
	defaultWorkspaceRefreshInterval = 30 * time.Second
	defaultWorkspaceRefreshTimeout  = 15 * time.Second
	maxWorkspaceSkillBytes          = 512 * 1024
	maxWorkspaceResourcesPerDir     = 100
)

// WorkspaceFS is the minimal view of the bot's sandbox workspace needed to
// discover skills (implemented in agent/bot on top of sandbox.Workspace; the
// skill package does not import sandbox).
type WorkspaceFS interface {
	// WorkDir is the workspace root as seen by the bot's tools (docker: /data).
	WorkDir() string
	// Exec runs a shell command with the workspace root as working directory.
	Exec(ctx context.Context, command string) (stdout string, exitCode int, err error)
	// ReadFile reads a workspace-relative file.
	ReadFile(ctx context.Context, path string) ([]byte, error)
}

type workspaceEntry struct {
	sig  string // "<mtime>.<size>" of SKILL.md
	name string // registered skill name ("" when the dir failed to load / was skipped)
}

// WorkspaceSource discovers skills in the bot's workspace.
type WorkspaceSource struct {
	fs       WorkspaceFS
	mgr      *SkillManager
	logger   Logger
	interval time.Duration
	timeout  time.Duration

	mu      sync.Mutex // serialises refreshes
	last    time.Time
	entries map[string]workspaceEntry // rel dir (skills/x) → entry
	now     func() time.Time
}

// NewWorkspaceSource creates a source; call Attach to hook it into mgr.
func NewWorkspaceSource(fs WorkspaceFS, mgr *SkillManager, logger Logger) *WorkspaceSource {
	if logger == nil {
		logger = noopLogger{}
	}
	return &WorkspaceSource{
		fs:       fs,
		mgr:      mgr,
		logger:   logger,
		interval: defaultWorkspaceRefreshInterval,
		timeout:  defaultWorkspaceRefreshTimeout,
		entries:  make(map[string]workspaceEntry),
		now:      time.Now,
	}
}

// Attach registers the source as mgr's lazy refresher and install location.
func (w *WorkspaceSource) Attach() {
	w.mgr.setWorkspaceSource(w, path.Join(w.fs.WorkDir(), WorkspaceSkillsDir))
}

// MaybeRefresh refreshes unless the last refresh was less than the interval
// ago. Errors are logged, never returned: discovery must not break a turn.
func (w *WorkspaceSource) MaybeRefresh(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if !w.last.IsZero() && w.now().Sub(w.last) < w.interval {
		return
	}
	w.refreshLocked(ctx)
}

// Refresh rescans the workspace now.
func (w *WorkspaceSource) Refresh(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.refreshLocked(ctx)
}

// listScript prints one "S\t<dir>\t<sig>" line per skills/<dir>/SKILL.md and
// "R\t<file>" lines for its scripts/references/assets (POSIX sh + GNU stat).
const listScript = `for d in ` + WorkspaceSkillsDir + `/*/; do
  f="${d}SKILL.md"
  [ -f "$f" ] || continue
  printf 'S\t%s\t%s\n' "${d%/}" "$(stat -L -c '%Y.%s' "$f" 2>/dev/null)"
  for sub in scripts references assets; do
    [ -d "$d$sub" ] || continue
    find "$d$sub" -type f 2>/dev/null | head -n ` + "100" + ` | while IFS= read -r p; do printf 'R\t%s\n' "$p"; done
  done
done
exit 0`

type listedDir struct {
	dir       string
	sig       string
	resources []string
}

func parseListing(out string) []listedDir {
	var dirs []listedDir
	for _, line := range strings.Split(out, "\n") {
		parts := strings.Split(strings.TrimRight(line, "\r"), "\t")
		switch {
		case len(parts) >= 3 && parts[0] == "S":
			dirs = append(dirs, listedDir{dir: parts[1], sig: parts[2]})
		case len(parts) >= 2 && parts[0] == "R" && len(dirs) > 0:
			cur := &dirs[len(dirs)-1]
			if strings.HasPrefix(parts[1], cur.dir+"/") && len(cur.resources) < 3*maxWorkspaceResourcesPerDir {
				cur.resources = append(cur.resources, parts[1])
			}
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].dir < dirs[j].dir })
	return dirs
}

func (w *WorkspaceSource) refreshLocked(parent context.Context) {
	w.last = w.now()
	ctx, cancel := context.WithTimeout(parent, w.timeout)
	defer cancel()

	out, code, err := w.fs.Exec(ctx, listScript)
	if err != nil || code != 0 {
		w.logger.Warnw("workspace skills: list failed", "err", err, "exit_code", code)
		return
	}
	listed := parseListing(out)
	root := w.fs.WorkDir()

	seen := make(map[string]bool, len(listed))
	claimed := make(map[string]string) // skill name → dir, first (sorted) dir wins
	added, updated, removed := 0, 0, 0
	for _, ld := range listed {
		seen[ld.dir] = true
		prev, known := w.entries[ld.dir]
		if known && prev.sig == ld.sig && ld.sig != "" {
			if prev.name != "" {
				claimed[prev.name] = ld.dir
			}
			continue
		}
		sk, err := w.load(ctx, root, ld)
		entry := workspaceEntry{sig: ld.sig}
		if err != nil {
			w.logger.Warnw("workspace skills: skip", "dir", ld.dir, "err", err)
		} else if other, dup := claimed[sk.Name]; dup {
			w.logger.Warnw("workspace skills: duplicate name, keeping first", "name", sk.Name, "dir", ld.dir, "kept", other)
		} else if cur, exists := w.mgr.Get(sk.Name); exists && cur.Source != SourceWorkspace {
			w.logger.Infow("workspace skills: name taken by a "+cur.Source+" skill, skipped", "name", sk.Name, "dir", ld.dir)
		} else {
			w.mgr.Register(sk)
			entry.name = sk.Name
			claimed[sk.Name] = ld.dir
			if known {
				updated++
			} else {
				added++
			}
		}
		// Renamed skill (same dir, new name): drop the old registration.
		if known && prev.name != "" && prev.name != entry.name {
			w.unregisterIfOurs(prev.name, root, ld.dir)
		}
		w.entries[ld.dir] = entry
	}
	for dir, e := range w.entries {
		if seen[dir] {
			continue
		}
		if e.name != "" {
			w.unregisterIfOurs(e.name, root, dir)
			removed++
		}
		delete(w.entries, dir)
	}
	if added+updated+removed > 0 {
		w.logger.Infow("workspace skills refreshed", "root", path.Join(root, WorkspaceSkillsDir),
			"added", added, "updated", updated, "removed", removed, "total", len(listed))
	}
}

func (w *WorkspaceSource) unregisterIfOurs(name, root, dir string) {
	if cur, ok := w.mgr.Get(name); ok && cur.Source == SourceWorkspace && cur.Dir == path.Join(root, dir) {
		w.mgr.Unregister(name)
	}
}

func (w *WorkspaceSource) load(ctx context.Context, root string, ld listedDir) (*Skill, error) {
	if size := sigSize(ld.sig); size > maxWorkspaceSkillBytes {
		return nil, fmt.Errorf("SKILL.md too large (%d bytes > %d)", size, maxWorkspaceSkillBytes)
	}
	data, err := w.fs.ReadFile(ctx, ld.dir+"/SKILL.md")
	if err != nil {
		return nil, err
	}
	sk, err := newSkillFromContent(string(data), SourceWorkspace, path.Join(root, ld.dir))
	if err != nil {
		return nil, err
	}
	for _, rel := range ld.resources {
		abs := path.Join(root, rel)
		sub := strings.TrimPrefix(rel, ld.dir+"/")
		switch {
		case strings.HasPrefix(sub, "scripts/"):
			sk.Resources.Scripts = append(sk.Resources.Scripts, abs)
		case strings.HasPrefix(sub, "references/"):
			sk.Resources.References = append(sk.Resources.References, abs)
		case strings.HasPrefix(sub, "assets/"):
			sk.Resources.Assets = append(sk.Resources.Assets, abs)
		}
	}
	return sk, nil
}

// sigSize extracts the size from a "<mtime>.<size>" signature (0 if absent).
func sigSize(sig string) int {
	i := strings.LastIndexByte(sig, '.')
	if i < 0 {
		return 0
	}
	n := 0
	for _, c := range sig[i+1:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// ----------------------------------------------------------------------------
// SkillManager hooks
// ----------------------------------------------------------------------------

// setWorkspaceSource wires the lazy refresher and the install location.
func (m *SkillManager) setWorkspaceSource(src *WorkspaceSource, installDir string) {
	m.mu.Lock()
	m.workspace = src
	m.installDir = installDir
	m.refreshTriggerLocked()
	m.mu.Unlock()
}

// refreshWorkspace runs a throttled workspace refresh (no-op without source).
func (m *SkillManager) refreshWorkspace(ctx context.Context) {
	m.mu.RLock()
	src := m.workspace
	m.mu.RUnlock()
	if src != nil {
		src.MaybeRefresh(ctx)
	}
}

// InstallDir returns where the bot should install its own skills ("" when no
// workspace source is attached).
func (m *SkillManager) InstallDir() string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.installDir
}

// installHintLocked explains where self-installed skills must go.
func (m *SkillManager) installHintLocked() string {
	if m.installDir == "" {
		return ""
	}
	return fmt.Sprintf("To install a new skill yourself, put it at %s/<skill-name>/SKILL.md (YAML front matter with `name` and `description`, then the instructions; optional scripts/, references/, assets/ subdirectories). It is discovered automatically within about 30 seconds and then found by skill_search. Skills placed anywhere else (e.g. ~/.claude/skills, /tmp) are never loaded.", m.installDir)
}
