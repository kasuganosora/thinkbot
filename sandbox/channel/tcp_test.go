package channel

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func startTCPServer(t *testing.T, listenAddr string, cfg Config) *Server {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if cfg.Root == "" {
		cfg.Root = root
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(dir, "channel.sock")
	}
	if cfg.MaxPutBytes == 0 {
		cfg.MaxPutBytes = 1 << 20
	}
	cfg.ListenAddr = listenAddr
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
	return srv
}

func TestSelectTransport(t *testing.T) {
	unix, err := SelectTransport("", "linux")
	if err != nil || unix != TransportUnix {
		t.Fatalf("linux default %q %v", unix, err)
	}
	win, err := SelectTransport("auto", "windows")
	if err != nil || win != TransportTCP {
		t.Fatalf("windows default %q %v", win, err)
	}
	forced, err := SelectTransport("http", "linux")
	if err != nil || forced != TransportTCP {
		t.Fatalf("http on linux %q %v", forced, err)
	}
	same, err := SelectTransport("tcp", "windows")
	if err != nil || same != TransportTCP {
		t.Fatalf("tcp %q %v", same, err)
	}
	keep, err := SelectTransport("unix", "windows")
	if err != nil || keep != TransportUnix {
		t.Fatalf("explicit unix on windows %q %v", keep, err)
	}
	if _, err := SelectTransport("websocket", "linux"); err == nil {
		t.Fatal("unknown transport accepted")
	}
}

func TestPublishSpecIsLoopback(t *testing.T) {
	spec := PublishSpec()
	if !strings.HasPrefix(spec, "127.0.0.1:") {
		t.Fatalf("publish spec %q", spec)
	}
	if strings.Contains(spec, "0.0.0.0") {
		t.Fatalf("host publish must not be wildcard: %s", spec)
	}
	listen := ContainerListenAddr()
	if listen != "0.0.0.0:17931" && listen != net.JoinHostPort("0.0.0.0", "17931") {
		t.Fatalf("container listen %q", listen)
	}
}

func TestTCPLoopbackHelloAndQueryTokenRejected(t *testing.T) {
	srv := startTCPServer(t, "127.0.0.1:0", Config{Token: "secret", BotID: "bot-a"})
	host, port, err := net.SplitHostPort(srv.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("bound host %q, want 127.0.0.1", host)
	}
	if srv.Addr() != net.JoinHostPort("127.0.0.1", port) {
		t.Fatalf("dial addr %q", srv.Addr())
	}
	body, err := os.ReadFile(filepath.Join(filepath.Dir(srv.cfg.SocketPath), ListenFileName))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(body)) != srv.Addr() {
		t.Fatalf("listen file %q", body)
	}
	if _, err := os.Stat(srv.cfg.SocketPath); !os.IsNotExist(err) {
		t.Fatal("tcp listen created a unix socket")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := PingTCP(ctx, srv.Addr()); err != nil {
		t.Fatal(err)
	}
	if _, err := DialTCP(ctx, srv.Addr(), DialOptions{Token: "nope", BotID: "bot-a"}); err == nil {
		t.Fatal("bad token accepted over tcp")
	}
	cli, err := DialTCP(ctx, srv.Addr(), DialOptions{Token: "secret", BotID: "bot-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	info, err := cli.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.Protocol != Protocol {
		t.Fatalf("info %+v", info)
	}

	if _, err := DialTCP(ctx, srv.Addr()+"?token=secret", DialOptions{Token: "secret", BotID: "bot-a"}); err == nil {
		t.Fatal("query token in the dial target was accepted")
	}
	if _, err := DialTCP(ctx, "http://127.0.0.1:"+port+"/channel?token=secret", DialOptions{Token: "secret"}); err == nil {
		t.Fatal("url token was accepted")
	}
	if _, err := DialTCP(ctx, "10.1.2.3:80", DialOptions{Token: "secret"}); err == nil {
		t.Fatal("non-loopback dial was accepted")
	}

	nc, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	if _, err := io.WriteString(nc, "GET /channel?token=secret HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = nc.SetReadDeadline(time.Now().Add(time.Second))
	buf, _ := io.ReadAll(nc)
	if bytes.Contains(buf, []byte("secret")) || bytes.Contains(buf, []byte("capabilities")) || bytes.Contains(buf, []byte("hello")) {
		t.Fatalf("http query token was treated as Hello: %q", buf)
	}
}

func TestTCPRejectsNonLoopbackListen(t *testing.T) {
	dir := t.TempDir()
	for _, addr := range []string{"10.1.2.3:0", "192.168.1.5:80", "8.8.8.8:17931"} {
		_, err := Listen(Config{
			Root:       dir,
			SocketPath: filepath.Join(dir, "s"),
			ListenAddr: addr,
		})
		if err == nil {
			t.Fatalf("listen %s was accepted", addr)
		}
	}
}

func TestWildcardListenDialsViaLoopback(t *testing.T) {
	srv := startTCPServer(t, "0.0.0.0:0", Config{Token: "secret", BotID: "bot-a"})
	boundHost, _, err := net.SplitHostPort(srv.ListenerAddr())
	if err != nil {
		t.Fatal(err)
	}
	if !IsWildcardHost(boundHost) {
		t.Fatalf("bound %q", srv.ListenerAddr())
	}
	if !strings.HasPrefix(srv.Addr(), "127.0.0.1:") {
		t.Fatalf("local dial %q", srv.Addr())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(srv.cfg.SocketPath), ListenFileName)); !os.IsNotExist(err) {
		t.Fatal("wildcard bind wrote a listen file that is not the host published port")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	cli, err := DialTCP(ctx, srv.Addr(), DialOptions{Token: "secret", BotID: "bot-a"})
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if err := PingTCP(ctx, "0.0.0.0:1"); err == nil {
		t.Fatal("wildcard dial was accepted")
	}
}
