package sandbox

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

// Docker's shell glob otherwise makes a missing directory look exactly like an
// empty one. Preserve typed errors for HTTP callers and quote paths as data.
func directoryCheckScript(path string) string {
	quoted := shellQuote(path)
	return fmt.Sprintf("[ -e %s ] || exit 44; [ -d %s ] || exit 45; [ -r %s ] && [ -x %s ] || exit 46; ", quoted, quoted, quoted, quoted)
}

func directoryExecError(err error, path string) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		var cause error
		switch exitErr.ExitCode() {
		case 44:
			cause = os.ErrNotExist
		case 45:
			cause = os.ErrInvalid
		case 46:
			cause = os.ErrPermission
		}
		if cause != nil {
			return &os.PathError{Op: "readdir", Path: path, Err: cause}
		}
	}
	return err
}
