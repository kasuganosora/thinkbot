package sandbox

import (
	"context"
	"encoding/json"
	"net"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"github.com/kasuganosora/thinkbot/sandbox/channel"
	"github.com/kasuganosora/thinkbot/sandbox/channelclient"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// channelTransport resolves sandbox.channel.transport for a channel image.
// Linux defaults to the unix socket. Windows defaults to TCP loopback.
// "http" is the ops name for that TCP path. A tcp transport cannot be combined
// with --network none, because the host publish would have nowhere to land.
func (c *botContainer) channelTransport() (string, error) {
	if !c.usesChannel() {
		return "", nil
	}
	transport, err := channel.SelectTransport(c.cfg.ChannelTransport, runtime.GOOS)
	if err != nil {
		return "", errs.Wrap(err, "bot_container: channel transport")
	}
	if transport == channel.TransportTCP && c.cfg.NetworkDisabled {
		return "", errs.New("bot_container: tcp channel transport publishes on 127.0.0.1 and cannot use --network none")
	}
	return transport, nil
}

// channelEndpointLocked returns the address DialChannel should use.
// The caller holds c.mu. For TCP the host port is the one Docker published on
// 127.0.0.1; it is remembered on the container as runtime state, not written
// into the container environment and not treated as a secret.
func (c *botContainer) channelEndpointLocked(ctx context.Context, transport string) (channelclient.Endpoint, error) {
	if transport != channel.TransportTCP {
		return channelclient.Endpoint{Network: channel.TransportUnix, Addr: c.channelSockFile()}, nil
	}
	if c.channelAddr != "" {
		return channelclient.Endpoint{Network: channel.TransportTCP, Addr: c.channelAddr}, nil
	}
	addr, err := c.publishedChannelAddr(ctx)
	if err != nil {
		return channelclient.Endpoint{}, err
	}
	c.channelAddr = addr
	return channelclient.Endpoint{Network: channel.TransportTCP, Addr: addr}, nil
}

func (c *botContainer) publishedChannelAddr(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{json .NetworkSettings.Ports}}", c.container)
	out, err := cmd.Output()
	if err != nil {
		return "", errs.Wrapf(err, "bot_container: inspect published channel port for %q", c.container)
	}
	addr, err := parsePublishedLoopback(strings.TrimSpace(string(out)), channel.PublishPort)
	if err != nil {
		return "", errs.Wrapf(err, "bot_container: %s", c.container)
	}
	return addr, nil
}

type publishedPort struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// parsePublishedLoopback reads docker inspect's port map and returns the
// 127.0.0.1 dial address. A wildcard or empty host IP is refused: the publish
// flag is 127.0.0.1 only, and the client must not dial a public interface.
func parsePublishedLoopback(raw string, containerPort int) (string, error) {
	if raw == "" || raw == "null" {
		return "", errs.New("bot_container: channel tcp port is not published")
	}
	var ports map[string][]publishedPort
	if err := json.Unmarshal([]byte(raw), &ports); err != nil {
		return "", errs.Wrap(err, "bot_container: parse published channel port")
	}
	binds := ports[strconv.Itoa(containerPort)+"/tcp"]
	if len(binds) == 0 || binds[0].HostPort == "" {
		return "", errs.New("bot_container: channel tcp port is not published")
	}
	ip := binds[0].HostIP
	if ip == "" || !channel.IsLoopbackHost(ip) {
		return "", errs.Newf("bot_container: channel tcp port is not bound to loopback (host ip %q)", ip)
	}
	if strings.EqualFold(ip, "localhost") {
		ip = "127.0.0.1"
	}
	return net.JoinHostPort(ip, binds[0].HostPort), nil
}
