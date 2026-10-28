//go:build unix

package channel

import (
	"fmt"
	"net"
	"os"
	"strconv"
	"syscall"
)

func openNewFile(path string) (*os.File, error) {
	// O_NOFOLLOW refuses to open a symlink planted at the temp path.
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
}

func listenUnix(path string) (net.Listener, error) {
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	return ln, err
}

func claimSockPeer(dir, sock, ready string) error {
	uid, gid, ok, err := sockPeerIDs()
	if err != nil || !ok {
		return err
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return err
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return err
	}
	if err := os.Chown(sock, uid, gid); err != nil {
		return err
	}
	if ready != "" {
		_ = os.Chown(ready, uid, gid)
	}
	return nil
}

// claimStateFiles chowns the state directory and any files that exist. Used
// by the TCP listener, which has no socket inode. A missing peer env is fine.
func claimStateFiles(dir string, paths ...string) error {
	uid, gid, ok, err := sockPeerIDs()
	if err != nil || !ok {
		return err
	}
	if err := os.Chmod(dir, 0o750); err != nil {
		return err
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		return err
	}
	for _, p := range paths {
		if p == "" {
			continue
		}
		if err := os.Chown(p, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

func sockPeerIDs() (uid, gid int, ok bool, err error) {
	uidStr := os.Getenv(EnvSockUID)
	if uidStr == "" || uidStr == "0" {
		return 0, 0, false, nil
	}
	uid, err = strconv.Atoi(uidStr)
	if err != nil || uid < 0 {
		return 0, 0, false, fmt.Errorf("channel: bad %s", EnvSockUID)
	}
	gid = uid
	if g := os.Getenv(EnvSockGID); g != "" {
		gid, err = strconv.Atoi(g)
		if err != nil || gid < 0 {
			return 0, 0, false, fmt.Errorf("channel: bad %s", EnvSockGID)
		}
	}
	return uid, gid, true, nil
}

func diskUsage(root string) DiskUsage {
	var st syscall.Statfs_t
	if err := syscall.Statfs(root, &st); err != nil {
		return DiskUsage{}
	}
	bsize := uint64(st.Bsize)
	return DiskUsage{
		Total: st.Blocks * bsize,
		Free:  st.Bfree * bsize,
		Avail: st.Bavail * bsize,
	}
}
