// Package channelclient is the main-process side of the in-sandbox channel.
//
// It derives a per-bot token from a server secret (HKDF, not stored per bot),
// dials the unix socket on the shared Volume B directory, and decides when a
// failed probe should recreate the bot container while keeping the data volume.
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
	Sock  string
}

// Dial health-checks the socket, then completes Hello with the token and bot id.
func Dial(ctx context.Context, sock, token, botID string) (*Client, error) {
	if err := probeHealth(ctx, sock); err != nil {
		return nil, err
	}
	inner, err := channel.Dial(ctx, sock, channel.DialOptions{
		Token:         token,
		BotID:         botID,
		ClientVersion: channel.Version,
	})
	if err != nil {
		return nil, fmt.Errorf("channel hello: %w", err)
	}
	return &Client{Client: inner, BotID: botID, Sock: sock}, nil
}

// Probe checks Health.Ping and then Hello. It does not keep the connection.
func Probe(ctx context.Context, sock, token, botID string) error {
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	cli, err := Dial(pctx, sock, token, botID)
	if err != nil {
		return err
	}
	return cli.Close()
}

func probeHealth(ctx context.Context, sock string) error {
	hctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := channel.Ping(hctx, sock); err != nil {
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
