package sandbox

import (
	"path/filepath"
	"strings"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// credentialNames are directories that stay unreachable from the local sandbox,
// even when they sit inside a bound project folder.
var credentialNames = map[string]struct{}{
	".ssh": {}, ".aws": {}, ".gnupg": {}, ".kube": {}, ".docker": {},
}

// localGuard is the write and secret policy for the local backend.
// Project, when set, is a workspace-relative directory the agent may write.
// The rest of the workspace stays readable except secrets and except writes
// into a .git directory.
type localGuard struct {
	project string
	offline bool
}

func (g localGuard) check(root, path string, write bool) error {
	validated, err := validatePath(root, path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(root, validated)
	if err != nil {
		return errs.Wrap(err, "sandbox/local: rel path")
	}
	parts := strings.Split(filepath.ToSlash(rel), "/")
	for i, part := range parts {
		if part == ".git" && write {
			return errs.Newf("sandbox/local: %q is read-only", path)
		}
		if _, secret := credentialNames[part]; secret {
			return errs.Newf("sandbox/local: %q is not accessible", path)
		}
		if i+1 < len(parts) && part == ".config" && (parts[i+1] == "gh" || parts[i+1] == "gcloud") {
			return errs.Newf("sandbox/local: %q is not accessible", path)
		}
	}
	if write && g.project != "" {
		proj := filepath.ToSlash(filepath.Clean(g.project))
		relSlash := filepath.ToSlash(rel)
		if relSlash != proj && !strings.HasPrefix(relSlash, proj+"/") {
			return errs.Newf("sandbox/local: writes are limited to %q", g.project)
		}
	}
	return nil
}
