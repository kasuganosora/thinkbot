// thinkbot-channel is the sandbox container's PID 1.
//
// Linux production listens on a unix socket. The Hello token is read from the
// one-shot token file next to that socket; THINKBOT_CHANNEL_TOKEN is only a
// fallback and is cleared from the environment. Set THINKBOT_CHANNEL_HTTP
// (or pass --http) to serve the same frames on TCP instead. Do not wrap this
// process in a shell or sleep: the sandbox image ENTRYPOINT is this binary
// in exec form.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
)

func main() {
	log.SetFlags(0)
	os.Exit(run(os.Args[1:]))
}

func run(args []string) int {
	if len(args) > 0 {
		switch args[0] {
		case "health":
			return runHealth(args[1:])
		case "version", "-version", "--version":
			fmt.Printf("thinkbot-channel %s protocol %s\n", channel.Version, channel.Protocol)
			return 0
		case "help", "-h", "--help":
			usage(os.Stdout)
			return 0
		}
	}
	if err := runDaemon(args); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}

func usage(w io.Writer) {
	fmt.Fprintf(w, `thinkbot-channel — in-sandbox control daemon (container PID 1)

Usage:
  thinkbot-channel [--sock PATH] [--root PATH] [--http ADDR]
  thinkbot-channel health [--sock PATH] [--http ADDR]
  thinkbot-channel version

The token is read once from <socket-dir>/%s (mode 0600) and the file is removed.
%s is only a fallback and is cleared from the environment after startup.
The token is sent only in the Hello frame, never in a URL or query string.
Health ping does not need a token.

Linux default is the unix socket. --http / %s selects TCP (loopback, or the
wildcard address Docker publishes onto the host's 127.0.0.1). The frames are
the same. A loopback bind with port 0 writes the chosen address to %s.

Environment:
  %s        optional; Hello.botId must match when set
  %s   socket path (default %s)
  %s   file jail (default %s)
  %s   TCP listen address; unset keeps the unix socket
`, channel.TokenFileName, channel.EnvToken, channel.EnvHTTP, channel.ListenFileName, channel.EnvBotID, channel.EnvSock, channel.DefaultSock, channel.EnvRoot, channel.DefaultRoot, channel.EnvHTTP)
}

func daemonConfig(args []string) (channel.Config, []string) {
	fs := flag.NewFlagSet("thinkbot-channel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sock := fs.String("sock", "", "unix socket path")
	root := fs.String("root", "", "virtual root for file operations")
	httpAddr := fs.String("http", "", "TCP listen address; empty keeps the unix socket")
	_ = fs.Parse(args)
	cfg := channel.Config{
		SocketPath: *sock,
		Root:       *root,
		ListenAddr: strings.TrimSpace(*httpAddr),
		BotID:      os.Getenv(channel.EnvBotID),
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = os.Getenv(channel.EnvSock)
	}
	if cfg.Root == "" {
		cfg.Root = os.Getenv(channel.EnvRoot)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = strings.TrimSpace(os.Getenv(channel.EnvHTTP))
	}
	return cfg, fs.Args()
}

// consumeChannelToken loads the one-shot token file next to the socket, or the
// process environment if the file is absent. Either source is then removed from
// the environment so /proc/1/environ does not keep the secret.
func consumeChannelToken(sock string) string {
	if sock == "" {
		sock = channel.DefaultSock
	}
	path := filepath.Join(filepath.Dir(sock), channel.TokenFileName)
	var fromFile string
	if b, err := os.ReadFile(path); err == nil {
		fromFile = strings.TrimSpace(string(b))
		_ = os.Remove(path)
	}
	fromEnv := os.Getenv(channel.EnvToken)
	_ = os.Unsetenv(channel.EnvToken)
	if fromFile != "" {
		return fromFile
	}
	return fromEnv
}

func runDaemon(args []string) error {
	cfg, extra := daemonConfig(args)
	sock := cfg.SocketPath
	if sock == "" {
		sock = os.Getenv(channel.EnvSock)
	}
	if sock == "" {
		sock = channel.DefaultSock
	}
	cfg.Token = consumeChannelToken(sock)
	if cfg.ListenAddr != "" && cfg.SocketPath == "" {
		// Ready and listen files stay next to the token. TCP mode does not
		// create the socket inode.
		cfg.SocketPath = sock
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cfg.OnShutdown = cancel

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigs)

	srv, err := channel.Listen(cfg)
	if err != nil {
		return err
	}
	defer srv.Close()

	if cfg.Token == "" {
		log.Printf("thinkbot-channel: no token file; authenticated RPC is rejected until one is provided (health ping still answers)")
	}
	if len(extra) > 0 {
		// A previous image contract appended `sleep infinity` as the container
		// command. With this binary as ENTRYPOINT those words are argv, not PID 1.
		log.Printf("thinkbot-channel: ignoring extra args %q; staying up as PID 1", extra)
	}
	where := "sock=" + srv.Addr()
	if cfg.ListenAddr != "" {
		where = "tcp=" + srv.Addr()
	}
	log.Printf("thinkbot-channel listening %s root=%s protocol=%s", where, cfg.Root, channel.Protocol)

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ctx) }()
	select {
	case <-sigs:
		log.Print("thinkbot-channel: signal received, stopping")
		cancel()
		<-errCh
	case err := <-errCh:
		if err != nil {
			return err
		}
	}
	// Only when this process is actually PID 1. A host-side Ctrl-C must not
	// signal unrelated processes; SignalOthers enforces that.
	if os.Getpid() == 1 {
		channel.SignalOthers(syscall.SIGTERM)
		time.Sleep(200 * time.Millisecond)
		channel.SignalOthers(syscall.SIGKILL)
	}
	return nil
}

func runHealth(args []string) int {
	fs := flag.NewFlagSet("health", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sock := fs.String("sock", "", "unix socket path")
	httpAddr := fs.String("http", "", "TCP listen address")
	if err := fs.Parse(args); err != nil {
		log.Print(err)
		return 2
	}
	addr := strings.TrimSpace(*httpAddr)
	if addr == "" {
		addr = strings.TrimSpace(os.Getenv(channel.EnvHTTP))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if addr != "" {
		if err := channel.PingTCP(ctx, channel.LocalDialAddr(addr)); err != nil {
			log.Print(err)
			return 1
		}
		return 0
	}
	path := *sock
	if path == "" {
		path = os.Getenv(channel.EnvSock)
	}
	if err := channel.Ping(ctx, path); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}
