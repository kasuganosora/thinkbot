package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
	"github.com/kasuganosora/thinkbot/sandbox/channelclient"
	"github.com/kasuganosora/thinkbot/util/errs"
)

func (c *botContainer) readFileChannel(ctx context.Context, path string) ([]byte, error) {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	return readChannelFile(ctx, cli, path)
}

func readChannelFile(ctx context.Context, cli *channelclient.Client, path string) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := cli.Get(ctx, path, &buf, channel.GetOpen{}); err != nil {
		return nil, mapChannelErr(path, err)
	}
	return buf.Bytes(), nil
}

func (c *botContainer) writeFileChannel(ctx context.Context, path string, data []byte, limit int64) error {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	return writeChannelFile(ctx, cli, path, bytes.NewReader(data), limit)
}

func (c *botContainer) writeStreamChannel(ctx context.Context, path string, r io.Reader, limit int64) (int64, error) {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return 0, err
	}
	defer cli.Close()
	n, err := cli.Put(ctx, path, r, channel.PutOpen{Mkdir: true, Limit: limit})
	if err != nil {
		return n, mapChannelErr(path, err)
	}
	return n, nil
}

func writeChannelFile(ctx context.Context, cli *channelclient.Client, path string, r io.Reader, limit int64) error {
	if _, err := cli.Put(ctx, path, r, channel.PutOpen{Mkdir: true, Limit: limit}); err != nil {
		return mapChannelErr(path, err)
	}
	return nil
}

func (c *botContainer) listDirChannel(ctx context.Context, path string) ([]FileEntry, error) {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	return listChannelDir(ctx, cli, path)
}

func listChannelDir(ctx context.Context, cli *channelclient.Client, path string) ([]FileEntry, error) {
	entries, err := cli.List(ctx, path)
	if err != nil {
		return nil, mapChannelErr(path, err)
	}
	out := make([]FileEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, FileEntry{Name: e.Name, IsDir: e.IsDir, Size: e.Size, ModTime: e.ModTime})
	}
	return out, nil
}

func (c *botContainer) statChannel(ctx context.Context, path string) (bool, bool, int64, error) {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return false, false, 0, err
	}
	defer cli.Close()
	st, err := cli.Stat(ctx, path)
	if err != nil {
		var rpc *channel.RPCError
		if errors.As(err, &rpc) && rpc.Code == channel.CodeNotFound {
			return false, false, 0, nil
		}
		return false, false, 0, mapChannelErr(path, err)
	}
	return true, st.IsDir, st.Size, nil
}

func (c *botContainer) mkdirChannel(ctx context.Context, path string) error {
	validated, err := validatePath(containerWorkDir, path)
	if err != nil {
		return err
	}
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	var detail strings.Builder
	res, err := cli.Exec(ctx, channel.ExecRequest{Argv: []string{"mkdir", "-p", "--", validated}}, func(stream string, data []byte) {
		if stream == "stderr" {
			detail.Write(data)
		}
	})
	if err != nil {
		return mapChannelErr(path, err)
	}
	if res.ExitCode != 0 {
		return errs.Newf("bot_container: mkdir %q failed: %s", path, strings.TrimSpace(detail.String()))
	}
	return nil
}

// ListContainerProcesses returns processes inside the bot container.
func (c *botContainer) ListContainerProcesses(ctx context.Context) ([]channel.Process, error) {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	return cli.Processes(ctx)
}

// SignalContainerProcess signals one pid inside the bot container.
func (c *botContainer) SignalContainerProcess(ctx context.Context, pid int, sig string) error {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.SignalProcess(ctx, pid, sig)
}

// KillContainerProcess sends SIGKILL to one pid inside the bot container.
func (c *botContainer) KillContainerProcess(ctx context.Context, pid int) error {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return err
	}
	defer cli.Close()
	return cli.KillProcess(ctx, pid)
}

func (c *botContainer) execStreamChannel(ctx context.Context, req ExecRequest, onChunk func(ExecChunk)) (*ExecResult, error) {
	cli, err := c.DialChannel(ctx)
	if err != nil {
		return nil, err
	}
	defer cli.Close()
	return c.runChannelCommand(ctx, cli, req, onChunk)
}

func (c *botContainer) runChannelCommand(ctx context.Context, cli *channelclient.Client, req ExecRequest, onChunk func(ExecChunk)) (*ExecResult, error) {
	if req.Command == "" {
		return nil, errs.New("bot_container: command is empty")
	}
	execCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	stuck, hard := resolveExecTimeouts(req, c.cfg)
	snap0, _ := channelOOMKill(execCtx, cli)

	var stdout, stderr bytes.Buffer
	var lastActivity atomic.Int64
	start := time.Now()
	lastActivity.Store(start.UnixNano())
	var reason atomic.Value
	reason.Store("")

	startupGrace := stuck / 2
	if startupGrace > maxStartupGrace {
		startupGrace = maxStartupGrace
	}
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		ticker := time.NewTicker(watchdogTickFor(stuck, hard))
		defer ticker.Stop()
		heart := time.NewTicker(heartbeatInterval)
		defer heart.Stop()
		for {
			select {
			case <-execCtx.Done():
				return
			case <-ticker.C:
				elapsed := time.Since(start)
				if elapsed > hard {
					reason.Store("hard")
					cancel()
					return
				}
				if elapsed > startupGrace && time.Now().UnixNano()-lastActivity.Load() > int64(stuck) {
					reason.Store("stuck")
					cancel()
					return
				}
			case <-heart.C:
				if onChunk != nil && time.Since(time.Unix(0, lastActivity.Load())) >= heartbeatInterval {
					onChunk(ExecChunk{Stream: "heartbeat"})
				}
			}
		}
	}()

	res, execErr := cli.Exec(execCtx, channel.ExecRequest{
		Command:   req.Command,
		Cwd:       req.WorkDir,
		TimeoutMs: hard.Milliseconds(),
	}, func(stream string, data []byte) {
		lastActivity.Store(time.Now().UnixNano())
		chunk := string(data)
		if stream == "stderr" {
			stderr.WriteString(chunk)
		} else {
			stdout.WriteString(chunk)
		}
		if onChunk != nil {
			onChunk(ExecChunk{Stream: stream, Data: chunk})
		}
	})
	cancel()
	<-watchDone

	result := &ExecResult{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: res.ExitCode}
	if c.cfg.MaxOutput > 0 {
		if s, trunc := truncateString(result.Stdout, c.cfg.MaxOutput); trunc {
			result.Stdout = s
			result.Truncated = true
		}
		if s, trunc := truncateString(result.Stderr, c.cfg.MaxOutput); trunc {
			result.Stderr = s
			result.Truncated = true
		}
	}
	why, _ := reason.Load().(string)
	if why != "" {
		result.ExitCode = -1
		if why == "stuck" {
			result.Stderr = fmt.Sprintf("命令被卡死看门狗终止：连续 %s 无输出且无进展\n", stuck) + result.Stderr
		} else {
			result.Stderr = fmt.Sprintf("命令超过硬上限 %s 被强制终止\n", hard) + result.Stderr
		}
	} else if execErr != nil && execCtx.Err() != nil {
		why = "cancelled"
		result.ExitCode = -1
		result.Stderr = "命令已取消\n" + result.Stderr
	} else if execErr != nil {
		return nil, errs.Wrapf(execErr, "bot_container: exec in %q", c.container)
	}
	finalizeExecResult(result, why)
	snapCtx, snapCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer snapCancel()
	if snap1, ok := channelOOMKill(snapCtx, cli); ok && snap1 > snap0 {
		result.OOMKilled = true
		result.Aborted = true
		result.Reliable = false
		result.Warnings = append(result.Warnings,
			fmt.Sprintf("Process was likely OOM-killed (cgroup oom_kill rose %d→%d); the result is incomplete", snap0, snap1))
	}
	return result, nil
}

func channelOOMKill(ctx context.Context, cli *channelclient.Client) (int, bool) {
	var out strings.Builder
	res, err := cli.Exec(ctx, channel.ExecRequest{Command: "cat /sys/fs/cgroup/memory.events 2>/dev/null || cat /sys/fs/cgroup/memory/memory.oom_control 2>/dev/null"}, func(stream string, data []byte) {
		if stream == "stdout" {
			out.Write(data)
		}
	})
	if err != nil || res.ExitCode != 0 {
		return 0, false
	}
	return parseOOMKill(out.String())
}

func mapChannelErr(path string, err error) error {
	var rpc *channel.RPCError
	if !errors.As(err, &rpc) {
		return err
	}
	switch rpc.Code {
	case channel.CodeNotFound:
		return &os.PathError{Op: "channel", Path: path, Err: os.ErrNotExist}
	case channel.CodeTooLarge:
		return ErrUserUploadTooLarge
	case channel.CodeJail:
		return errs.Newf("sandbox: %s", rpc.Error())
	case channel.CodeBadRequest:
		if strings.Contains(strings.ToLower(rpc.Message), "directory") {
			return &os.PathError{Op: "readdir", Path: path, Err: os.ErrInvalid}
		}
		return errs.New(rpc.Error())
	default:
		return errs.New(rpc.Error())
	}
}
