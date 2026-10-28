//go:build unix

package channel

import (
	"net"
	"os"
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
