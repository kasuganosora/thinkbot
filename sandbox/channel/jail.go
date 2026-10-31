package channel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrJail means the path escapes the virtual root, including via "..", a
// symlink, or a prefix trick such as root "/data" vs "/data2".
var ErrJail = errors.New("path escapes virtual root")

// ErrTooLarge means a streaming put passed its limit. The partial file is deleted.
var ErrTooLarge = errors.New("file exceeds size limit")

// JailPath maps a user path into root.
//
// Paths equal to the virtual root "/data", or starting with "/data/", drop that
// prefix so docker (root == "/data") and a relocated root share one spelling.
// An absolute path that is not under that prefix is still confined: the leading
// slash is stripped and the remainder is joined onto root.
func JailPath(root, path string) (string, error) {
	root = filepath.Clean(root)
	if root == "" || root == "." {
		return "", fmt.Errorf("%w: empty root", ErrJail)
	}
	if strings.ContainsRune(path, 0) {
		return "", fmt.Errorf("%w: NUL in path", ErrJail)
	}
	if path == "" {
		path = "."
	}
	cleaned := strings.ReplaceAll(path, "\\", "/")
	const virtual = "/data"
	switch {
	case cleaned == virtual:
		cleaned = "."
	case strings.HasPrefix(cleaned, virtual+"/"):
		cleaned = cleaned[len(virtual)+1:]
	}
	for _, part := range strings.Split(cleaned, "/") {
		if part == ".." {
			return "", fmt.Errorf("%w: %q", ErrJail, path)
		}
	}
	cleaned = strings.TrimLeft(cleaned, "/")
	full := root
	if cleaned != "" && cleaned != "." {
		full = filepath.Join(root, filepath.FromSlash(cleaned))
	}
	full = filepath.Clean(full)
	if !within(root, full) {
		return "", fmt.Errorf("%w: %q", ErrJail, path)
	}
	if err := rejectSymlinkEscape(root, full); err != nil {
		return "", err
	}
	return full, nil
}

func within(root, target string) bool {
	root = filepath.Clean(root)
	target = filepath.Clean(target)
	if target == root {
		return true
	}
	sep := string(filepath.Separator)
	return strings.HasPrefix(target, root+sep)
}

// rejectSymlinkEscape walks each existing component. A symlink is resolved even
// when its target is missing (EvalSymlinks gives up on dangling links), and the
// target must stay inside root. Components that do not exist yet stop the walk.
func rejectSymlinkEscape(root, full string) error {
	root = filepath.Clean(root)
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("%w: root: %v", ErrJail, err)
		}
		resolvedRoot = root
	}
	if full == root {
		return nil
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("%w: %q", ErrJail, full)
	}
	cur := root
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		cur = filepath.Join(cur, part)
		info, err := os.Lstat(cur)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return fmt.Errorf("%w: %v", ErrJail, err)
		}
		if info.Mode()&os.ModeSymlink == 0 {
			continue
		}
		target, err := symlinkTarget(cur)
		if err != nil {
			return fmt.Errorf("%w: %v", ErrJail, err)
		}
		if !within(resolvedRoot, target) && !within(root, target) {
			return fmt.Errorf("%w: symlink", ErrJail)
		}
	}
	return nil
}

func symlinkTarget(link string) (string, error) {
	if resolved, err := filepath.EvalSymlinks(link); err == nil {
		return filepath.Clean(resolved), nil
	}
	raw, err := os.Readlink(link)
	if err != nil {
		return "", err
	}
	if !filepath.IsAbs(raw) {
		raw = filepath.Join(filepath.Dir(link), raw)
	}
	return filepath.Clean(raw), nil
}
