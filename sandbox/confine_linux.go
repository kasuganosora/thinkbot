//go:build linux

package sandbox

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ConfineIfRequested applies the local-sandbox mount policy and then replaces
// this process with the user shell. Production main and the sandbox TestMain
// both call it so `unshare` can re-exec this binary inside the new namespaces.
// It returns false when this process is not the confinement child.
func ConfineIfRequested() bool {
	if len(os.Args) < 2 || os.Args[1] != "--local-confine-inside" {
		return false
	}
	if err := runConfine(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "sandbox/local: %v\n", err)
		os.Exit(126)
	}
	os.Exit(0)
	return true
}

func runConfine(args []string) error {
	if len(args) != 4 {
		return fmt.Errorf("confine args")
	}
	root, project, work, command := args[0], args[1], args[2], args[3]
	if project == "-" {
		project = ""
	}
	if err := protectWorkspace(root, project); err != nil {
		return err
	}
	// cmd.Dir was opened before the bind mount, so it still points at the
	// uncovered tree. Enter the protected path by absolute name.
	if work == "" || work == "-" {
		work = root
	}
	if err := unix.Chdir(work); err != nil {
		return fmt.Errorf("chdir: %w", err)
	}
	if err := denyRemount(); err != nil {
		return err
	}
	sh, err := exec.LookPath("sh")
	if err != nil {
		return err
	}
	return syscall.Exec(sh, []string{"sh", "-c", command}, os.Environ())
}

func protectWorkspace(root, project string) error {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return fmt.Errorf("workspace %q is not a directory", root)
	}
	if project != "" {
		proj := filepath.Join(root, filepath.Clean(project))
		st, err := os.Stat(proj)
		if err != nil || !st.IsDir() {
			return fmt.Errorf("project %q is not a directory", project)
		}
		stage, err := os.MkdirTemp("/tmp", "thinkbot-proj")
		if err != nil {
			return err
		}
		if err := unix.Mount(proj, stage, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind project: %w", err)
		}
		if err := unix.Mount(root, root, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("bind workspace: %w", err)
		}
		if err := unix.Mount("", root, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
			return fmt.Errorf("workspace read-only: %w", err)
		}
		if err := unix.Mount(stage, proj, "", unix.MS_BIND, ""); err != nil {
			return fmt.Errorf("reopen project: %w", err)
		}
	}
	return filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return nil
		}
		depth := strings.Count(filepath.ToSlash(rel), "/") + 1
		if d.IsDir() && depth > 6 {
			return filepath.SkipDir
		}
		name := d.Name()
		parent := filepath.Base(filepath.Dir(path))
		if d.IsDir() && (isCredentialDir(name) || (parent == ".config" && (name == "gh" || name == "gcloud"))) {
			if err := unix.Mount("tmpfs", path, "tmpfs", 0, "size=64k,mode=000"); err != nil {
				return fmt.Errorf("mask %s: %w", name, err)
			}
			return filepath.SkipDir
		}
		if d.IsDir() && name == ".git" {
			if err := unix.Mount(path, path, "", unix.MS_BIND, ""); err != nil {
				return fmt.Errorf("bind .git: %w", err)
			}
			if err := unix.Mount("", path, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
				return fmt.Errorf(".git read-only: %w", err)
			}
			return filepath.SkipDir
		}
		return nil
	})
}

func isCredentialDir(name string) bool {
	_, ok := credentialNames[name]
	return ok
}

func denyRemount() error {
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	denied := []uintptr{
		unix.SYS_MOUNT,
		unix.SYS_UMOUNT2,
		unix.SYS_PIVOT_ROOT,
		unix.SYS_UNSHARE,
		unix.SYS_SETNS,
		unix.SYS_CHROOT,
	}
	filter := make([]unix.SockFilter, 0, len(denied)*2+2)
	filter = append(filter, unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0})
	for _, nr := range denied {
		filter = append(filter,
			unix.SockFilter{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: uint32(nr), Jt: 0, Jf: 1},
			unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
		)
	}
	filter = append(filter, unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW})
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	_, _, errno := unix.Syscall(unix.SYS_SECCOMP, uintptr(unix.SECCOMP_SET_MODE_FILTER), 0, uintptr(unsafe.Pointer(&prog)))
	if errno != 0 {
		return fmt.Errorf("seccomp (%s): %w", runtime.GOARCH, errno)
	}
	return nil
}

// applyLocalConfinement re-execs this binary inside a user, mount, and
// (when offline) network namespace before the command runs. Credential
// directories are masked and .git is read-only. When a project is set, the
// rest of the workspace is read-only. The child cannot undo those mounts.
func applyLocalConfinement(cmd *exec.Cmd, command, root string, g localGuard) error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("sandbox/local: confinement is not available on %s", runtime.GOOS)
	}
	unshare, err := exec.LookPath("unshare")
	if err != nil {
		return fmt.Errorf("sandbox/local: confinement needs unshare: %w", err)
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("sandbox/local: executable: %w", err)
	}
	project := g.project
	if project == "" {
		project = "-"
	}
	args := []string{"unshare"}
	if g.offline {
		args = append(args, "-n")
	}
	if os.Geteuid() != 0 {
		args = append(args, "-r")
	}
	work := cmd.Dir
	if work == "" {
		work = "-"
	}
	args = append(args, "-m", "--", exe, "--local-confine-inside", root, project, work, command)
	cmd.Path = unshare
	cmd.Args = args
	return nil
}
