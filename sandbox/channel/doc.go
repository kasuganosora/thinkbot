// Package channel is the in-sandbox control protocol used by thinkbot-channel.
//
// The daemon listens on a unix socket (default /run/thinkbot/channel.sock).
// Linux production keeps that socket. Windows, and an explicit http/tcp
// transport, use the same frames on TCP: the host dials 127.0.0.1 only.
// The token is presented in the Hello frame, never in a URL or query string.
// File IO is jailed under a virtual root (default /data). Protocol 1 covers
// health, streaming file put/get/list/delete, non-interactive command exec,
// and process list/signal/kill.
//
// It does not implement a PTY or any screenshot / display capture.
// The binary is built to be container PID 1: it reaps grandchildren and, when
// it actually is PID 1, forwards SIGTERM/SIGINT to the rest of the container.
package channel
