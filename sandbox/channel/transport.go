package channel

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
)

const (
	// TransportUnix is the Linux production default: a mode-0600 socket on
	// Volume B plus the one-shot token file.
	TransportUnix = "unix"
	// TransportTCP is the same framed protocol over TCP. The host dials
	// 127.0.0.1 only. Windows prefers this because Docker Desktop cannot
	// share a unix socket off a Windows-backed mount.
	TransportTCP = "tcp"
	// TransportHTTP is the configuration name for TransportTCP
	// (sandbox.channel.transport=http). It is not an HTTP application
	// protocol: there is no request line, and the token is not accepted from
	// a URL path or a query string.
	TransportHTTP = "http"
)

// SelectTransport chooses how the host reaches thinkbot-channel.
//
// explicit is sandbox.channel.transport: "", "auto", "unix", "tcp", or "http".
// An empty or auto value follows the host OS: Windows prefers TCP loopback,
// anything else keeps the unix socket. "http" and "tcp" are the same transport.
func SelectTransport(explicit, goos string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(explicit)) {
	case "", "auto":
		if goos == "windows" {
			return TransportTCP, nil
		}
		return TransportUnix, nil
	case TransportUnix:
		return TransportUnix, nil
	case TransportTCP, TransportHTTP:
		return TransportTCP, nil
	default:
		return "", fmt.Errorf("channel: unknown transport %q", explicit)
	}
}

// PublishSpec is the docker -p value. The host side is loopback only, with an
// ephemeral host port. Docker assigns the host port; the main process records
// it in runtime state after inspect.
func PublishSpec() string {
	return "127.0.0.1::" + strconv.Itoa(PublishPort)
}

// ContainerListenAddr is the address passed to the daemon as THINKBOT_CHANNEL_HTTP.
// It is not a secret. Docker's port proxy connects to the container address,
// which cannot be the container's own 127.0.0.1, so the daemon binds the
// wildcard and the host publish stays on 127.0.0.1. Clients still dial
// loopback only.
func ContainerListenAddr() string {
	return net.JoinHostPort("0.0.0.0", strconv.Itoa(PublishPort))
}

// IsLoopbackHost reports whether host is a loopback name or IP.
func IsLoopbackHost(host string) bool {
	switch strings.ToLower(strings.Trim(host, "[]")) {
	case "localhost", "127.0.0.1", "::1":
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// IsWildcardHost reports whether host is an unspecified address. The daemon
// may bind it so a published host port can be forwarded. Dialing it is refused.
func IsWildcardHost(host string) bool {
	switch strings.Trim(host, "[]") {
	case "", "0.0.0.0", "::":
		return true
	}
	return false
}

// TCPListenHostAllowed is true for loopback and for the wildcard form used
// inside a container whose port is published on the host's 127.0.0.1.
// A specific LAN or public address is refused.
func TCPListenHostAllowed(host string) bool {
	return IsLoopbackHost(host) || IsWildcardHost(host)
}

// LocalDialAddr maps a wildcard bind to 127.0.0.1 on the same port so health
// checks and same-namespace clients never dial 0.0.0.0.
func LocalDialAddr(addr string) string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return addr
	}
	if IsWildcardHost(host) {
		return net.JoinHostPort("127.0.0.1", port)
	}
	return addr
}

// RejectTokenTarget refuses a dial target that could carry the token outside
// the Hello frame. Query strings, token= parameters, and URLs are rejected
// before any connection is opened. The TCP listener is not an HTTP server, so
// a request line cannot authenticate either.
func RejectTokenTarget(addr string) error {
	if addr == "" {
		return nil
	}
	lower := strings.ToLower(addr)
	if strings.Contains(addr, "?") || strings.Contains(lower, "token=") || strings.Contains(lower, "://") {
		return errors.New("channel: token is accepted only in the Hello frame, not in the dial target")
	}
	return nil
}

func requireLoopbackDial(addr string) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("channel: dial address: %w", err)
	}
	if !IsLoopbackHost(host) {
		return fmt.Errorf("channel: refusing to dial non-loopback host %q", host)
	}
	return nil
}
