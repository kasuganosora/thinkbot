package channel

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startTestServer(t *testing.T, cfg Config) *Server {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "tbch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	root := filepath.Join(dir, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg.Root == "" {
		cfg.Root = root
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(dir, "s")
	}
	if cfg.MaxPutBytes == 0 {
		cfg.MaxPutBytes = 1 << 20
	}
	srv, err := Listen(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = srv.Serve(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		<-done
	})
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(cfg.SocketPath); err == nil {
			return srv
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("socket was not created")
	return nil
}

func dialToken(t *testing.T, srv *Server, token, botID string) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	t.Cleanup(cancel)
	cli, err := Dial(ctx, srv.Addr(), DialOptions{Token: token, BotID: botID})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

func TestHealthPingWithoutToken(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := Ping(ctx, srv.Addr()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ReadyFile(srv.Addr())); err != nil {
		t.Fatal(err)
	}
}

func TestHelloRejectsBadTokenAndBotID(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret", BotID: "bot-a"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, srv.Addr(), DialOptions{Token: "nope", BotID: "bot-a"}); err == nil {
		t.Fatal("expected token rejection")
	}
	if _, err := Dial(ctx, srv.Addr(), DialOptions{Token: "secret", BotID: "other"}); err == nil {
		t.Fatal("expected bot id rejection")
	}
	if _, err := Dial(ctx, srv.Addr(), DialOptions{Token: "secret"}); err == nil {
		t.Fatal("expected missing bot id to be rejected when the daemon has one")
	}
	cli := dialToken(t, srv, "secret", "bot-a")
	info, err := cli.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Protocol != Protocol || info.Root == "" || info.Disk.Total == 0 {
		t.Fatalf("info %+v", info)
	}
}

func TestEmptyTokenRejectsHello(t *testing.T) {
	srv := startTestServer(t, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, srv.Addr(), DialOptions{Token: "anything"}); err == nil {
		t.Fatal("empty server token must reject hello")
	}
}

func TestFilePutGetListDeleteStat(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret"})
	cli := dialToken(t, srv, "secret", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	body := []byte("hello channel")
	n, err := cli.Put(ctx, "nested/a.txt", bytes.NewReader(body), PutOpen{Mkdir: true, Mode: 0o640})
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(len(body)) {
		t.Fatalf("wrote %d", n)
	}
	entries, err := cli.List(ctx, "nested")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name != "a.txt" || entries[0].IsDir || entries[0].Size != int64(len(body)) {
		t.Fatalf("list %+v", entries)
	}
	st, err := cli.Stat(ctx, "/data/nested/a.txt")
	if err != nil {
		t.Fatal(err)
	}
	if st.Size != int64(len(body)) || st.IsDir || st.Mode&0o777 != 0o640 {
		t.Fatalf("stat %+v", st)
	}
	var got bytes.Buffer
	gn, err := cli.Get(ctx, "nested/a.txt", &got, GetOpen{Offset: 6, Limit: 4})
	if err != nil {
		t.Fatal(err)
	}
	if gn != 4 || got.String() != "chan" {
		t.Fatalf("get %d %q", gn, got.String())
	}
	if err := cli.Delete(ctx, "nested/a.txt", false); err != nil {
		t.Fatal(err)
	}
	if _, err := cli.Stat(ctx, "nested/a.txt"); err == nil {
		t.Fatal("expected not found after delete")
	} else if rpc, ok := err.(*RPCError); !ok || rpc.Code != CodeNotFound {
		t.Fatalf("stat err %#v", err)
	}
	if err := cli.Delete(ctx, ".", false); err == nil {
		t.Fatal("deleting the virtual root must fail")
	}
}

func TestPutTooLargeRemovesPartial(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret", MaxPutBytes: 8})
	cli := dialToken(t, srv, "secret", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := cli.Put(ctx, "big.bin", bytes.NewReader([]byte("0123456789")), PutOpen{Mkdir: true})
	if err == nil {
		t.Fatal("expected too large")
	}
	rpc, ok := err.(*RPCError)
	if !ok || rpc.Code != CodeTooLarge {
		t.Fatalf("err %#v", err)
	}
	if _, err := cli.Stat(ctx, "big.bin"); err == nil {
		t.Fatal("partial destination must not remain")
	}
	matches, _ := filepath.Glob(filepath.Join(srv.cfg.Root, ".thinkbot-partial-*"))
	if len(matches) != 0 {
		t.Fatalf("partial files left: %v", matches)
	}
}

func TestPutRejectsSymlinkEscape(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret"})
	outside := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(srv.cfg.Root, "link")); err != nil {
		t.Fatal(err)
	}
	cli := dialToken(t, srv, "secret", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := cli.Put(ctx, "link", strings.NewReader("x"), PutOpen{})
	if err == nil {
		t.Fatal("expected jail error")
	}
	if rpc, ok := err.(*RPCError); !ok || rpc.Code != CodeJail {
		t.Fatalf("err %#v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "secret")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("outside file was created")
	}
}

func TestUnauthenticatedFileOpRejected(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret"})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := Dial(ctx, srv.Addr(), DialOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.List(ctx, "."); err == nil {
		t.Fatal("expected unauthorized")
	} else if rpc, ok := err.(*RPCError); !ok || rpc.Code != CodeUnauthorized {
		t.Fatalf("err %#v", err)
	}
}

func TestCommandExecStreamsAndScrubsToken(t *testing.T) {
	t.Setenv(EnvToken, "super-secret")
	srv := startTestServer(t, Config{Token: "super-secret"})
	cli := dialToken(t, srv, "super-secret", "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var stdout, stderr bytes.Buffer
	res, err := cli.Exec(ctx, ExecRequest{
		Command: `printf %s "$FOO"; printf '|%s' "$THINKBOT_CHANNEL_TOKEN" >&2`,
		Env:     []string{"FOO=visible", "THINKBOT_CHANNEL_TOKEN=injected"},
	}, func(stream string, data []byte) {
		switch stream {
		case "stdout":
			stdout.Write(data)
		case "stderr":
			stderr.Write(data)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit %+v", res)
	}
	if stdout.String() != "visible" {
		t.Fatalf("stdout %q", stdout.String())
	}
	if stderr.String() != "|" {
		t.Fatalf("token leaked to child stderr: %q", stderr.String())
	}
}

func TestCommandCwdJail(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret"})
	cli := dialToken(t, srv, "secret", "")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := cli.Exec(ctx, ExecRequest{Command: "pwd", Cwd: "../"}, nil)
	if err == nil {
		t.Fatal("expected jail error")
	}
	if rpc, ok := err.(*RPCError); !ok || rpc.Code != CodeJail {
		t.Fatalf("err %#v", err)
	}
}

func TestCommandCancelAndProcessKill(t *testing.T) {
	srv := startTestServer(t, Config{Token: "secret"})
	cli := dialToken(t, srv, "secret", "")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan ExecResult, 1)
	go func() {
		res, err := cli.Exec(ctx, ExecRequest{Argv: []string{"sleep", "30"}}, nil)
		if err != nil {
			t.Errorf("exec: %v", err)
		}
		done <- res
	}()
	var pid int
	deadline := time.Now().Add(3 * time.Second)
	cli2 := dialToken(t, srv, "secret", "")
	for pid == 0 && time.Now().Before(deadline) {
		procs, err := cli2.Processes(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range procs {
			if strings.Contains(p.Cmd, "sleep 30") {
				pid = p.PID
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if pid == 0 {
		cancel()
		t.Fatal("sleep 30 did not appear in the process list")
	}
	if err := cli2.KillProcess(context.Background(), 1); err == nil {
		t.Fatal("killing pid 1 must be refused")
	}
	if err := cli2.KillProcess(context.Background(), os.Getpid()); err == nil {
		t.Fatal("killing the daemon must be refused")
	}
	cancel()
	select {
	case res := <-done:
		if res.ExitCode == 0 {
			t.Fatalf("sleep should not exit 0: %+v", res)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("exec did not return after cancel")
	}
	if _, err := os.Stat("/proc/" + itoa(pid)); err == nil {
		raw, _ := os.ReadFile("/proc/" + itoa(pid) + "/stat")
		if !strings.Contains(string(raw), " Z ") && !strings.Contains(string(raw), ") Z") {
			t.Fatalf("sleep still running: %s", raw)
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [16]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
