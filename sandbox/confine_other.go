//go:build !linux

package sandbox

import (
	"fmt"
	"os/exec"
	"runtime"
)

func ConfineIfRequested() bool { return false }

func applyLocalConfinement(cmd *exec.Cmd, command, root string, g localGuard) error {
	if g.offline || g.project != "" {
		return fmt.Errorf("sandbox/local: confinement is not available on %s", runtime.GOOS)
	}
	return nil
}
