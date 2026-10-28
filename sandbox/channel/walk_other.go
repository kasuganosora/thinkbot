//go:build !unix

package channel

import (
	"errors"
	"os"
)

var errNotDir = errors.New("not a directory")

func openFileNoFollow(root, full string) (*os.File, error) {
	return os.Open(full)
}

func openDirNoFollow(root, full string) (*os.File, error) {
	info, err := os.Stat(full)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errNotDir
	}
	return os.Open(full)
}

func removeNoFollow(root, full string, recursive bool) error {
	if recursive {
		return os.RemoveAll(full)
	}
	return os.Remove(full)
}
