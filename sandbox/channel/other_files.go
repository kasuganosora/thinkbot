//go:build !unix

package channel

import (
	"errors"
	"net"
	"os"
)

func openNewFile(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
}

func listenUnix(path string) (net.Listener, error) {
	return nil, errors.New("channel: unix sockets are required")
}

func diskUsage(root string) DiskUsage { return DiskUsage{} }
