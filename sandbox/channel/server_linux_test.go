//go:build linux

package channel

import (
	"os"
	"strconv"
	"syscall"
	"testing"
)

func TestSocketOwnedByConfiguredPeer(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root owns the socket without the peer chown path")
	}
	t.Setenv(EnvSockUID, strconv.Itoa(os.Getuid()))
	t.Setenv(EnvSockGID, strconv.Itoa(os.Getgid()))
	srv := startTestServer(t, Config{Token: "secret"})
	fi, err := os.Stat(srv.Addr())
	if err != nil {
		t.Fatal(err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("stat type")
	}
	if int(st.Uid) != os.Getuid() || int(st.Gid) != os.Getgid() {
		t.Fatalf("socket uid/gid %d/%d", st.Uid, st.Gid)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %o", fi.Mode().Perm())
	}
}
