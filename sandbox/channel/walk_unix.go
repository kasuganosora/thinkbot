//go:build unix

package channel

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// errNotDir is returned when a list target exists but is not a directory.
var errNotDir = errors.New("not a directory")

// openFileNoFollow opens path for reading without following a symlink that
// appears after JailPath returns. Parent components are walked with openat
// and O_NOFOLLOW, so a swapped directory symlink cannot escape the root.
func openFileNoFollow(root, full string) (*os.File, error) {
	return openWithin(root, full, unix.O_RDONLY, 0)
}

func openDirNoFollow(root, full string) (*os.File, error) {
	f, err := openWithin(root, full, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if err != nil {
		if errors.Is(err, syscall.ENOTDIR) {
			return nil, errNotDir
		}
		return nil, err
	}
	return f, nil
}

func openWithin(root, full string, lastFlags int, mode uint32) (*os.File, error) {
	root = filepath.Clean(root)
	full = filepath.Clean(full)
	fd, err := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	if full == root {
		return os.NewFile(uintptr(fd), root), nil
	}
	rel, err := filepath.Rel(root, full)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("%w: %s", ErrJail, full)
	}
	parts := strings.Split(rel, string(filepath.Separator))
	for i, part := range parts {
		if part == "" || part == "." {
			continue
		}
		flags := unix.O_NOFOLLOW | unix.O_CLOEXEC
		perm := uint32(0)
		if i == len(parts)-1 {
			flags |= lastFlags
			perm = mode
		} else {
			flags |= unix.O_RDONLY | unix.O_DIRECTORY
		}
		nfd, err := unix.Openat(fd, part, flags, perm)
		_ = unix.Close(fd)
		if err != nil {
			return nil, mapWalkErr(err)
		}
		fd = nfd
	}
	return os.NewFile(uintptr(fd), full), nil
}

func removeNoFollow(root, full string, recursive bool) error {
	root = filepath.Clean(root)
	full = filepath.Clean(full)
	if full == root {
		return errors.New("refusing to delete the virtual root")
	}
	parent := filepath.Dir(full)
	name := filepath.Base(full)
	pfile, err := openDirNoFollow(root, parent)
	if err != nil {
		return err
	}
	defer pfile.Close()
	pfd := int(pfile.Fd())
	var st unix.Stat_t
	if err := unix.Fstatat(pfd, name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	isDir := st.Mode&unix.S_IFMT == unix.S_IFDIR
	if isDir && recursive {
		child, err := unix.Openat(pfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return mapWalkErr(err)
		}
		err = removeTree(child)
		_ = unix.Close(child)
		if err != nil {
			return err
		}
		return unix.Unlinkat(pfd, name, unix.AT_REMOVEDIR)
	}
	flag := 0
	if isDir {
		flag = unix.AT_REMOVEDIR
	}
	return unix.Unlinkat(pfd, name, flag)
}

func removeTree(dirfd int) error {
	dup, err := unix.Dup(dirfd)
	if err != nil {
		return err
	}
	f := os.NewFile(uintptr(dup), "")
	entries, err := f.ReadDir(-1)
	_ = f.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "." || name == ".." {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 || !e.IsDir() {
			if err := unix.Unlinkat(dirfd, name, 0); err != nil {
				return err
			}
			continue
		}
		child, err := unix.Openat(dirfd, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if err != nil {
			return mapWalkErr(err)
		}
		err = removeTree(child)
		_ = unix.Close(child)
		if err != nil {
			return err
		}
		if err := unix.Unlinkat(dirfd, name, unix.AT_REMOVEDIR); err != nil {
			return err
		}
	}
	return nil
}

func mapWalkErr(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, syscall.ELOOP) || errors.Is(err, unix.ELOOP) {
		return fmt.Errorf("%w: symlink", ErrJail)
	}
	return err
}
