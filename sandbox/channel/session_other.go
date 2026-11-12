//go:build !linux

package channel

import "fmt"

// Non-Linux daemons have no process-group reaper and no PTY support in the
// first release. Report it explicitly instead of pretending the session ran;
// the host then returns CodeUnsupported / state=lost (docs §12).
func killSessionGroup(pgid int, sig string) error {
	return fmt.Errorf("interactive PTY session requires a Linux container")
}

// Non-Linux daemons never start a session process group.
func sessionGroupAlive(pgid int) bool { return false }

func (c *conn) runSession(f Frame, s *sessionRunner) {
	defer c.removeSession(f.ReqID)
	defer s.stop()
	s.sendSpawnResult(sessionUnsupportedResult("daemon is not running on Linux"))
	s.sendExit(SessionExit{ExitCode: -1, Code: CodeUnsupported,
		Error: "interactive PTY session requires a Linux container"})
}
