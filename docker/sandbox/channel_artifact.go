package botsandbox

import (
	"crypto/md5"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// channelBinaryName is the file EnsureImage writes into the docker build context.
// The sandbox Dockerfile COPY's it to /usr/local/bin/thinkbot-channel.
const channelBinaryName = "thinkbot-channel"

// channelSourceDirs are hashed into the image tag when the binary is not
// embedded, so a channel change still produces a new thinkbot-bot:<md5>.
var channelSourceDirs = []string{
	"cmd/thinkbot-channel",
	"sandbox/channel",
}

func channelSourceDigest() ([]byte, error) {
	root, err := findModuleRoot()
	if err != nil {
		return nil, err
	}
	var files []string
	for _, dir := range channelSourceDirs {
		base := filepath.Join(root, filepath.FromSlash(dir))
		err := filepath.Walk(base, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if info.IsDir() {
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, path)
			return nil
		})
		if err != nil {
			return nil, errs.Wrapf(err, "botsandbox: walk %s", dir)
		}
	}
	sort.Strings(files)
	h := md5.New()
	for _, path := range files {
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		_, _ = h.Write([]byte(filepath.ToSlash(rel)))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
	}
	if len(files) == 0 {
		return nil, errs.New("botsandbox: channel source digest is empty")
	}
	return h.Sum(nil), nil
}

func findModuleRoot() (string, error) {
	var starts []string
	if env := os.Getenv("THINKBOT_SOURCE"); env != "" {
		starts = append(starts, env)
	}
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if exe, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(exe))
	}
	starts = append(starts, "/app/src")
	seen := map[string]bool{}
	for _, start := range starts {
		dir := start
		for dir != "" && dir != string(filepath.Separator) && !seen[dir] {
			seen[dir] = true
			if isThinkbotModule(dir) {
				return dir, nil
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	return "", errs.New("botsandbox: thinkbot module root not found (set THINKBOT_SOURCE, or rebuild with -tags embedchannel)")
}

func isThinkbotModule(dir string) bool {
	if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "cmd", "thinkbot-channel", "main.go"))
	return err == nil
}

// writeChannelBinary puts the linux channel daemon into the docker build context.
// Production slim images embed the bytes (-tags embedchannel). A dev tree with
// the Go toolchain compiles it instead.
func writeChannelBinary(dir string) error {
	path := filepath.Join(dir, channelBinaryName)
	if b := embeddedChannelBinary(); len(b) > 0 {
		return os.WriteFile(path, b, 0o755)
	}
	return compileChannel(path)
}

func compileChannel(out string) error {
	root, err := findModuleRoot()
	if err != nil {
		return err
	}
	goBin, err := exec.LookPath("go")
	if err != nil {
		return errs.Wrap(err, "botsandbox: go toolchain required to compile thinkbot-channel (or rebuild thinkbot with -tags embedchannel)")
	}
	cmd := exec.Command(goBin, "build", "-trimpath", "-ldflags=-s -w", "-o", out, "./cmd/thinkbot-channel")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+runtime.GOARCH)
	outBuf, err := cmd.CombinedOutput()
	if err != nil {
		tail := string(outBuf)
		if len(tail) > 4096 {
			tail = "..." + tail[len(tail)-4096:]
		}
		return errs.Wrapf(err, "botsandbox: build thinkbot-channel: %s", tail)
	}
	return nil
}
