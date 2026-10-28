// Package channelclient is the main-process side of the in-sandbox channel.
//
// It derives a per-bot token from a server secret (HKDF, not stored per bot),
// dials the daemon, and decides when a failed probe should recreate the bot
// container while keeping the data volume. Linux dials the unix socket on the
// shared Volume B directory. Windows, and sandbox.channel.transport=http, dial
// 127.0.0.1 with the same Hello token. The token is not a URL or query value.
// File, exec, and process RPCs are available for later call-site migration.
// There is no PTY and no screenshot support.
package channelclient

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/hkdf"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

const (
	// EnvSecret is an optional process environment override for the server secret.
	// It is preferred over config so the secret need not be written to the database.
	EnvSecret = "THINKBOT_CHANNEL_SECRET"

	// EnvSockRoot overrides the directory this process uses to reach per-bot sockets.
	EnvSockRoot = "THINKBOT_CHANNEL_SOCK_ROOT"

	// EnvTransport overrides sandbox.channel.transport (auto, unix, tcp, or http).
	// It selects the dial path. It is not a secret and is not the Hello token.
	EnvTransport = "THINKBOT_CHANNEL_TRANSPORT"

	// DefaultSockRoot is the shared Volume B parent. In DooD it must be the same
	// path on the Docker host and inside the main container.
	DefaultSockRoot = "/var/run/thinkbot-bots"

	// ContainerSockDir is where the bot image listens (/run/thinkbot/channel.sock).
	ContainerSockDir = "/run/thinkbot"

	hkdfSalt = "thinkbot-channel-v1"
	hkdfInfo = "channel"
)

// SelectSecret picks the server secret. env wins, then explicit config, then a
// process-wide fallback (typically the existing auth secret). The value is not
// logged and is not written into the bot data volume.
func SelectSecret(env, configured, fallback string) string {
	if env != "" {
		return env
	}
	if configured != "" {
		return configured
	}
	return fallback
}

// DeriveToken returns the bot-scoped channel token.
// HKDF-SHA256(secret, salt=thinkbot-channel-v1, info=botID || NUL || "channel").
// The same secret and bot id always yield the same token, so nothing is stored.
func DeriveToken(secret, botID string) (string, error) {
	if secret == "" {
		return "", errors.New("channel secret is empty")
	}
	if botID == "" {
		return "", errors.New("channel bot id is empty")
	}
	info := make([]byte, 0, len(botID)+1+len(hkdfInfo))
	info = append(info, botID...)
	info = append(info, 0)
	info = append(info, hkdfInfo...)
	r := hkdf.New(sha256.New, []byte(secret), []byte(hkdfSalt), info)
	buf := make([]byte, 32)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// Client is an authenticated channel session. RPC methods come from the
// embedded protocol client (Put, Get, List, Delete, Stat, Exec, Processes,
// SignalProcess, KillProcess, Shutdown).
type Client struct {
	*channel.Client
	BotID string
	// Sock is the dial address: a unix path, or 127.0.0.1:port for TCP.
	Sock string
}

// Endpoint is one daemon address. Network is channel.TransportUnix or
// channel.TransportTCP. An empty Network means unix.
type Endpoint struct {
	Network string
	Addr    string
}

// Dial health-checks the unix socket, then completes Hello with the token and bot id.
func Dial(ctx context.Context, sock, token, botID string) (*Client, error) {
	return DialEndpoint(ctx, Endpoint{Network: channel.TransportUnix, Addr: sock}, token, botID)
}

// DialEndpoint health-checks the endpoint, then completes Hello. The token is
// sent only in the Hello frame.
func DialEndpoint(ctx context.Context, ep Endpoint, token, botID string) (*Client, error) {
	if ep.Network == "" {
		ep.Network = channel.TransportUnix
	}
	if ep.Addr == "" && ep.Network == channel.TransportUnix {
		ep.Addr = channel.DefaultSock
	}
	if err := channel.RejectTokenTarget(ep.Addr); err != nil {
		return nil, err
	}
	if err := probeHealthEndpoint(ctx, ep); err != nil {
		return nil, err
	}
	opt := channel.DialOptions{
		Token:         token,
		BotID:         botID,
		ClientVersion: channel.Version,
	}
	var (
		inner *channel.Client
		err   error
	)
	switch ep.Network {
	case channel.TransportUnix:
		inner, err = channel.Dial(ctx, ep.Addr, opt)
	case channel.TransportTCP:
		inner, err = channel.DialTCP(ctx, ep.Addr, opt)
	default:
		err = fmt.Errorf("channel: unknown transport %q", ep.Network)
	}
	if err != nil {
		return nil, fmt.Errorf("channel hello: %w", err)
	}
	return &Client{Client: inner, BotID: botID, Sock: ep.Addr}, nil
}

// Probe checks Health.Ping and then Hello on the unix socket. It does not keep the connection.
func Probe(ctx context.Context, sock, token, botID string) error {
	return ProbeEndpoint(ctx, Endpoint{Network: channel.TransportUnix, Addr: sock}, token, botID)
}

// ProbeEndpoint checks Health.Ping and then Hello. It does not keep the connection.
func ProbeEndpoint(ctx context.Context, ep Endpoint, token, botID string) error {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cli, err := DialEndpoint(pctx, ep, token, botID)
	if err != nil {
		return err
	}
	return cli.Close()
}

func probeHealthEndpoint(ctx context.Context, ep Endpoint) error {
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var err error
	switch ep.Network {
	case "", channel.TransportUnix:
		err = channel.Ping(hctx, ep.Addr)
	case channel.TransportTCP:
		err = channel.PingTCP(hctx, ep.Addr)
	default:
		err = fmt.Errorf("channel: unknown transport %q", ep.Network)
	}
	if err != nil {
		return fmt.Errorf("channel health: %w", err)
	}
	return nil
}

// Recover probes once. On failure it recreates the container (the callback must
// keep the data volume) and probes again. A second failure is returned; there
// is no alternate execution path.
func Recover(ctx context.Context, probe func(context.Context) error, recreate func(context.Context) error) error {
	if probe == nil {
		return errors.New("channel probe is nil")
	}
	if err := probe(ctx); err == nil {
		return nil
	} else if recreate == nil {
		return err
	} else if rerr := recreate(ctx); rerr != nil {
		return fmt.Errorf("recreate bot container after channel probe failure: %w (probe: %v)", rerr, err)
	}
	if err := probe(ctx); err != nil {
		return fmt.Errorf("channel probe failed after recreate (data volume kept): %w", err)
	}
	return nil
}
