// thinkbot-channel is the sandbox container's PID 1.
//
// It listens on a unix socket, checks THINKBOT_CHANNEL_TOKEN, and serves the
// protocol in sandbox/channel. Do not wrap this process in a shell or sleep:
// the sandbox image ENTRYPOINT is this binary in exec form.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
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
  thinkbot-channel [--sock PATH] [--root PATH]
  thinkbot-channel health [--sock PATH]
  thinkbot-channel version

Environment:
  %s   required for authenticated RPC (health ping does not use it)
  %s        optional; Hello.botId must match when set
  %s   socket path (default %s)
  %s   file jail (default %s)
`, channel.EnvToken, channel.EnvBotID, channel.EnvSock, channel.DefaultSock, channel.EnvRoot, channel.DefaultRoot)
}

func daemonConfig(args []string) (channel.Config, []string) {
	fs := flag.NewFlagSet("thinkbot-channel", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	sock := fs.String("sock", "", "unix socket path")
	root := fs.String("root", "", "virtual root for file operations")
	_ = fs.Parse(args)
	cfg := channel.Config{
		SocketPath: *sock,
		Root:       *root,
		Token:      os.Getenv(channel.EnvToken),
		BotID:      os.Getenv(channel.EnvBotID),
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = os.Getenv(channel.EnvSock)
	}
	if cfg.Root == "" {
		cfg.Root = os.Getenv(channel.EnvRoot)
	}
	return cfg, fs.Args()
}

func runDaemon(args []string) error {
	cfg, extra := daemonConfig(args)
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
		log.Printf("thinkbot-channel: %s is empty; authenticated RPC is rejected until it is set (health ping still answers)", channel.EnvToken)
	}
	if len(extra) > 0 {
		// A previous image contract appended `sleep infinity` as the container
		// command. With this binary as ENTRYPOINT those words are argv, not PID 1.
		log.Printf("thinkbot-channel: ignoring extra args %q; staying up as PID 1", extra)
	}
	log.Printf("thinkbot-channel listening sock=%s root=%s protocol=%s", srv.Addr(), cfg.Root, channel.Protocol)

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
	if err := fs.Parse(args); err != nil {
		log.Print(err)
		return 2
	}
	path := *sock
	if path == "" {
		path = os.Getenv(channel.EnvSock)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := channel.Ping(ctx, path); err != nil {
		log.Print(err)
		return 1
	}
	return 0
}
