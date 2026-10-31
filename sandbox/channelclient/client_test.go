package channelclient

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

func TestDeriveTokenStableAndScoped(t *testing.T) {
	a, err := DeriveToken("server-secret", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := DeriveToken("server-secret", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	if a != b || a == "" || a == "server-secret" || strings.Contains(a, "server-secret") {
		t.Fatalf("token = %q", a)
	}
	other, err := DeriveToken("server-secret", "bot-b")
	if err != nil {
		t.Fatal(err)
	}
	if other == a {
		t.Fatal("token did not depend on bot id")
	}
	rotated, err := DeriveToken("other-secret", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	if rotated == a {
		t.Fatal("token did not depend on secret")
	}
	if _, err := DeriveToken("", "bot-a"); err == nil {
		t.Fatal("empty secret accepted")
	}
	if _, err := DeriveToken("server-secret", ""); err == nil {
		t.Fatal("empty bot id accepted")
	}
}

func TestSelectSecretPrefersEnv(t *testing.T) {
	if got := SelectSecret("from-env", "from-config", "from-jwt"); got != "from-env" {
		t.Fatalf("got %q", got)
	}
	if got := SelectSecret("", "from-config", "from-jwt"); got != "from-config" {
		t.Fatalf("got %q", got)
	}
	if got := SelectSecret("", "", "from-jwt"); got != "from-jwt" {
		t.Fatalf("got %q", got)
	}
}

func TestProbeHealthThenHello(t *testing.T) {
	token, err := DeriveToken("server-secret", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("/tmp", "tbcc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	root := dir + "/data"
	sock := dir + "/s"
	srv, err := channel.Listen(channel.Config{
		Root:        root,
		Token:       token,
		BotID:       "bot-a",
		SocketPath:  sock,
		MaxPutBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { _ = srv.Close() })

	pctx, cancelProbe := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelProbe()
	if err := Probe(pctx, sock, token, "bot-a"); err != nil {
		t.Fatal(err)
	}
	if err := Probe(pctx, sock, "wrong", "bot-a"); err == nil {
		t.Fatal("wrong token was accepted")
	}
	cli, err := Dial(pctx, sock, token, "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if _, err := cli.Put(pctx, "note.txt", strings.NewReader("ok"), channel.PutOpen{Mkdir: true}); err != nil {
		t.Fatal(err)
	}
	entries, err := cli.List(pctx, ".")
	if err != nil || len(entries) != 1 || entries[0].Name != "note.txt" {
		t.Fatalf("list %+v %v", entries, err)
	}
}

func TestDialEndpointTCPHello(t *testing.T) {
	token, err := DeriveToken("server-secret", "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	srv, err := channel.Listen(channel.Config{
		Root:        dir + "/data",
		Token:       token,
		BotID:       "bot-a",
		SocketPath:  dir + "/s",
		ListenAddr:  "127.0.0.1:0",
		MaxPutBytes: 1 << 20,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Serve(ctx) }()
	t.Cleanup(func() { _ = srv.Close() })

	ep := Endpoint{Network: channel.TransportTCP, Addr: srv.Addr()}
	pctx, cancelProbe := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelProbe()
	if err := ProbeEndpoint(pctx, ep, token, "bot-a"); err != nil {
		t.Fatal(err)
	}
	if err := ProbeEndpoint(pctx, Endpoint{Network: channel.TransportTCP, Addr: srv.Addr() + "?token=" + token}, token, "bot-a"); err == nil {
		t.Fatal("query token was accepted")
	}
	cli, err := DialEndpoint(pctx, ep, token, "bot-a")
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()
	if cli.Sock != srv.Addr() {
		t.Fatalf("sock %q", cli.Sock)
	}
}

func TestRecoverRecreatesOnce(t *testing.T) {
	var probes, recreates int
	err := Recover(context.Background(), func(context.Context) error {
		probes++
		if probes < 2 {
			return errors.New("unreachable")
		}
		return nil
	}, func(context.Context) error {
		recreates++
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if probes != 2 || recreates != 1 {
		t.Fatalf("probes=%d recreates=%d", probes, recreates)
	}
}

func TestRecoverStopsAfterSecondMiss(t *testing.T) {
	var recreates int
	err := Recover(context.Background(), func(context.Context) error {
		return errors.New("no socket")
	}, func(context.Context) error {
		recreates++
		return nil
	})
	if err == nil || !strings.Contains(err.Error(), "data volume kept") {
		t.Fatalf("err %v", err)
	}
	if recreates != 1 {
		t.Fatalf("recreates=%d", recreates)
	}
}

func TestRecoverDoesNotRecreateWhenHealthy(t *testing.T) {
	called := false
	err := Recover(context.Background(), func(context.Context) error { return nil }, func(context.Context) error {
		called = true
		return nil
	})
	if err != nil || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestRecoverSurfacesRecreateError(t *testing.T) {
	probes := 0
	err := Recover(context.Background(), func(context.Context) error {
		probes++
		return errors.New("down")
	}, func(context.Context) error {
		return errors.New("rm failed")
	})
	if err == nil || !strings.Contains(err.Error(), "rm failed") || probes != 1 {
		t.Fatalf("err=%v probes=%d", err, probes)
	}
}
