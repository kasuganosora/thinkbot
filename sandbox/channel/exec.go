package channel

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

func (c *conn) runExec(f Frame, sess *execSession) {
	defer c.endExec(f.ReqID, sess)

	var req ExecRequest
	if err := decode(f.Payload, &req); err != nil {
		c.sendResult(MsgCommandExit, f.ReqID, ExecResult{Code: CodeBadRequest, Error: err.Error(), ExitCode: -1})
		return
	}
	cmd, err := buildCommand(c.s.cfg.Root, req)
	if err != nil {
		code := CodeBadRequest
		if errors.Is(err, ErrJail) {
			code = CodeJail
		}
		c.sendResult(MsgCommandExit, f.ReqID, ExecResult{Code: code, Error: err.Error(), ExitCode: -1})
		return
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		c.sendResult(MsgCommandExit, f.ReqID, ExecResult{Code: CodeIO, Error: err.Error(), ExitCode: -1})
		return
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		c.sendResult(MsgCommandExit, f.ReqID, ExecResult{Code: CodeIO, Error: err.Error(), ExitCode: -1})
		return
	}
	if err := c.s.reaper.Start(cmd); err != nil {
		c.sendResult(MsgCommandExit, f.ReqID, ExecResult{Code: CodeIO, Error: err.Error(), ExitCode: -1})
		return
	}
	defer func() { _ = cmd.Process.Release() }()
	if err := sess.setPID(cmd.Process.Pid); err != nil {
		_ = SignalGroup(cmd.Process.Pid, syscall.SIGKILL)
	}

	var timer *time.Timer
	if req.TimeoutMs > 0 {
		timer = time.AfterFunc(time.Duration(req.TimeoutMs)*time.Millisecond, func() {
			_ = sess.signal(syscall.SIGKILL)
		})
		defer timer.Stop()
	}

	errCh := make(chan error, 2)
	go func() { errCh <- streamPipe(c, MsgCommandStdout, f.ReqID, stdout) }()
	go func() { errCh <- streamPipe(c, MsgCommandStderr, f.ReqID, stderr) }()
	st, waitErr := c.s.reaper.Wait(cmd.Process.Pid)
	// Drain pipes after the process exits so the client sees the full output
	// before the exit frame.
	err1 := <-errCh
	err2 := <-errCh
	res := ExecResult{ExitCode: st.Code, Signal: st.Signal}
	switch {
	case waitErr != nil:
		res.Error = waitErr.Error()
		res.Code = CodeIO
	case err1 != nil:
		res.Error = err1.Error()
		res.Code = CodeIO
	case err2 != nil:
		res.Error = err2.Error()
		res.Code = CodeIO
	}
	c.sendResult(MsgCommandExit, f.ReqID, res)
}

func streamPipe(c *conn, kind MsgType, id uint32, r io.Reader) error {
	buf := make([]byte, 32<<10)
	for {
		n, err := r.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if werr := c.sendRaw(kind, id, chunk); werr != nil {
				return werr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func buildCommand(root string, req ExecRequest) (*exec.Cmd, error) {
	cwd := root
	if req.Cwd != "" {
		var err error
		cwd, err = JailPath(root, req.Cwd)
		if err != nil {
			return nil, err
		}
	}
	info, err := os.Stat(cwd)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, errors.New("cwd is not a directory")
	}
	var cmd *exec.Cmd
	switch {
	case len(req.Argv) > 0:
		if strings.TrimSpace(req.Argv[0]) == "" {
			return nil, errors.New("empty argv")
		}
		cmd = exec.Command(req.Argv[0], req.Argv[1:]...)
	case strings.TrimSpace(req.Command) != "":
		shell := "/bin/sh"
		if _, err := os.Stat(shell); err != nil {
			shell = "sh"
		}
		cmd = exec.Command(shell, "-c", req.Command)
	default:
		return nil, errors.New("command or argv is required")
	}
	cmd.Dir = cwd
	cmd.Env = childEnv(req.Env)
	return cmd, nil
}

func childEnv(extra []string) []string {
	base := os.Environ()
	out := make([]string, 0, len(base)+len(extra))
	for _, e := range base {
		if scrubEnvKey(e) {
			continue
		}
		out = append(out, e)
	}
	for _, e := range extra {
		if scrubEnvKey(e) || !strings.Contains(e, "=") {
			continue
		}
		key := e[:strings.IndexByte(e, '=')]
		out = dropEnvKey(out, key)
		out = append(out, e)
	}
	return out
}

func scrubEnvKey(e string) bool {
	key, _, _ := strings.Cut(e, "=")
	return key == EnvToken
}

func dropEnvKey(env []string, key string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			continue
		}
		out = append(out, e)
	}
	return out
}
