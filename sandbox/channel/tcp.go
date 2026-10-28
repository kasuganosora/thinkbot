package channel

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// listenTCP binds cfg.ListenAddr and serves the same frames as the unix socket.
// Loopback addresses are the direct case (127.0.0.1:0 lets the OS pick a port).
// A wildcard address is allowed only so Docker can publish that port onto the
// host's 127.0.0.1. Addr returns the loopback dial address either way.
func listenTCP(cfg Config) (*Server, error) {
	host, _, err := net.SplitHostPort(cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("channel: listen address: %w", err)
	}
	if !TCPListenHostAllowed(host) {
		return nil, fmt.Errorf("channel: tcp listen host %q is not loopback", host)
	}
	if err := os.MkdirAll(cfg.Root, 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return nil, err
	}
	dial := LocalDialAddr(ln.Addr().String())
	dir := stateDir(cfg)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		_ = ln.Close()
		return nil, err
	}
	ready := filepath.Join(dir, "ready")
	if err := os.WriteFile(ready, []byte("protocol="+Protocol+"\nversion="+Version+"\ntransport=tcp\n"), 0o600); err != nil {
		_ = ln.Close()
		return nil, err
	}
	var listenFile string
	// The listen file is meaningful when the dialer shares this network
	// namespace. A wildcard bind's local address is not the Docker-published
	// host port, so it is not written where it could be mistaken for one.
	if IsLoopbackHost(host) {
		listenFile = filepath.Join(dir, ListenFileName)
		if err := os.WriteFile(listenFile, []byte(dial+"\n"), 0o644); err != nil {
			_ = ln.Close()
			return nil, err
		}
	}
	if err := claimStateFiles(dir, ready, listenFile); err != nil {
		_ = ln.Close()
		return nil, err
	}
	return &Server{
		cfg:        cfg,
		ln:         ln,
		reaper:     NewReaper(),
		started:    time.Now(),
		ready:      ready,
		listenFile: listenFile,
		boundDial:  dial,
		conns:      map[net.Conn]struct{}{},
	}, nil
}

func stateDir(cfg Config) string {
	if cfg.SocketPath != "" {
		return filepath.Dir(cfg.SocketPath)
	}
	return filepath.Dir(DefaultSock)
}
