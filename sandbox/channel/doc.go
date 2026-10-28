// Package channel is the in-sandbox control protocol used by thinkbot-channel.
//
// The daemon listens on a unix socket (default /run/thinkbot/channel.sock),
// checks a token presented at Hello, and jails file IO under a virtual root
// (default /data). Protocol 1 covers health, streaming file put/get/list/delete,
// non-interactive command exec, and process list/signal/kill.
//
// It does not implement a PTY or any screenshot / display capture.
// The binary is built to be container PID 1: it reaps grandchildren and, when
// it actually is PID 1, forwards SIGTERM/SIGINT to the rest of the container.
package channel
