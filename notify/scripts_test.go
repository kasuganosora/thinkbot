package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// 这些用例直接运行 scripts/notify 下的发送脚本与 mdadm / smartd 钩子（需要 python3 / sh，缺失则跳过）。

var scriptsDir = filepath.Join("..", "scripts", "notify")

type recordedCall struct {
	Path    string
	Auth    string
	Payload map[string]any
}

type notifyRecorder struct {
	mu    sync.Mutex
	calls []recordedCall
	srv   *httptest.Server
}

func newNotifyRecorder(t *testing.T) *notifyRecorder {
	r := &notifyRecorder{}
	r.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		b, _ := io.ReadAll(req.Body)
		var p map[string]any
		_ = json.Unmarshal(b, &p)
		r.mu.Lock()
		r.calls = append(r.calls, recordedCall{req.URL.Path, req.Header.Get("Authorization"), p})
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"delivered":true,"id":"ntf-test"}`))
	}))
	t.Cleanup(r.srv.Close)
	return r
}

func (r *notifyRecorder) take() []recordedCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func needTool(t *testing.T, name string) {
	t.Helper()
	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available", name)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// scriptEnv 返回一个干净的环境：不继承调用方的 THINKBOT_NOTIFY_*。
func scriptEnv(extra map[string]string) []string {
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8"}
	for k, v := range extra {
		env = append(env, k+"="+v)
	}
	return env
}

func runScript(t *testing.T, env map[string]string, stdin string, name string, args ...string) (int, string, string) {
	t.Helper()
	cmd := exec.Command(filepath.Join(scriptsDir, name), args...)
	cmd.Env = scriptEnv(env)
	cmd.Stdin = strings.NewReader(stdin)
	var stdout, stderr strings.Builder
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", name, err)
	}
	return code, stdout.String(), stderr.String()
}

func TestNotifyScriptBotURLAndModePrecedence(t *testing.T) {
	needTool(t, "python3")
	rec := newNotifyRecorder(t)
	conf := t.TempDir()
	writeFile(t, filepath.Join(conf, "notify.token"), "tbn_test_token\n")
	base := map[string]string{"THINKBOT_NOTIFY_CONF_DIR": conf}
	with := func(kv ...string) map[string]string {
		m := map[string]string{}
		for k, v := range base {
			m[k] = v
		}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	send := func(env map[string]string, args ...string) recordedCall {
		t.Helper()
		code, _, stderr := runScript(t, env, "", "thinkbot-notify", append([]string{"-s", "maid/test", "-l", "info", "-t", "hi"}, args...)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		calls := rec.take()
		if len(calls) != 1 {
			t.Fatalf("calls=%d", len(calls))
		}
		if calls[0].Auth != "Bearer tbn_test_token" {
			t.Fatalf("auth %q", calls[0].Auth)
		}
		return calls[0]
	}
	newURL := rec.srv.URL + "/api/notify"
	legacyURL := rec.srv.URL + "/api/bots/bot-legacy/notify"

	// 旧主机：只有 per-bot notify.url，没有 notify.bot → 用 URL 里的 bot id。
	writeFile(t, filepath.Join(conf, "notify.url"), legacyURL+"\n")
	c := send(with())
	if c.Path != "/api/bots/bot-legacy/notify" || c.Payload["bot"] != "bot-legacy" {
		t.Fatalf("legacy url fallback: %+v", c)
	}
	if _, ok := c.Payload["mode"]; ok {
		t.Fatalf("mode must be omitted by default: %+v", c.Payload)
	}
	// 环境变量 URL 优先于文件
	c = send(with("THINKBOT_NOTIFY_URL", rec.srv.URL+"/api/bots/bot-env-url/notify"))
	if c.Payload["bot"] != "bot-env-url" {
		t.Fatalf("%+v", c)
	}

	// 找不到任何 bot：明确报错、不发请求。
	code, _, stderr := runScript(t, with("THINKBOT_NOTIFY_URL", newURL), "", "thinkbot-notify", "-s", "maid/test", "-t", "hi")
	if code != 1 || !strings.Contains(stderr, "no bot configured") || len(rec.take()) != 0 {
		t.Fatalf("exit=%d stderr=%s", code, stderr)
	}

	// bot：--bot > env > notify.bot > URL
	writeFile(t, filepath.Join(conf, "notify.bot"), "bot-file\n")
	if c = send(with("THINKBOT_NOTIFY_URL", newURL)); c.Payload["bot"] != "bot-file" || c.Path != "/api/notify" {
		t.Fatalf("%+v", c)
	}
	if c = send(with("THINKBOT_NOTIFY_URL", newURL, "THINKBOT_NOTIFY_BOT", "bot-env")); c.Payload["bot"] != "bot-env" {
		t.Fatalf("%+v", c)
	}
	if c = send(with("THINKBOT_NOTIFY_URL", newURL, "THINKBOT_NOTIFY_BOT", "bot-env"), "--bot", "bot-flag"); c.Payload["bot"] != "bot-flag" {
		t.Fatalf("%+v", c)
	}

	// mode：-m > THINKBOT_NOTIFY_MODE > notify.mode > 省略
	env := with("THINKBOT_NOTIFY_URL", newURL)
	writeFile(t, filepath.Join(conf, "notify.mode"), "raw\n")
	if c = send(env); c.Payload["mode"] != "raw" {
		t.Fatalf("file mode: %+v", c.Payload)
	}
	if c = send(with("THINKBOT_NOTIFY_URL", newURL, "THINKBOT_NOTIFY_MODE", "bot")); c.Payload["mode"] != "bot" {
		t.Fatalf("env mode: %+v", c.Payload)
	}
	if c = send(with("THINKBOT_NOTIFY_URL", newURL, "THINKBOT_NOTIFY_MODE", "bot"), "-m", "raw"); c.Payload["mode"] != "raw" {
		t.Fatalf("flag mode: %+v", c.Payload)
	}
	if c = send(env, "--mode", "bot"); c.Payload["mode"] != "bot" {
		t.Fatalf("flag mode: %+v", c.Payload)
	}
	// 非法值：记日志后忽略（交给服务端默认），绝不因此丢通知。
	writeFile(t, filepath.Join(conf, "notify.mode"), "LOUD\n")
	code, _, stderr = runScript(t, env, "", "thinkbot-notify", "-s", "maid/test", "-t", "hi")
	calls := rec.take()
	if code != 0 || len(calls) != 1 || !strings.Contains(stderr, "ignoring invalid mode") {
		t.Fatalf("exit=%d calls=%d stderr=%s", code, len(calls), stderr)
	}
	if _, ok := calls[0].Payload["mode"]; ok {
		t.Fatalf("invalid mode must be dropped: %+v", calls[0].Payload)
	}
	os.Remove(filepath.Join(conf, "notify.mode"))
	if c = send(env); c.Payload["mode"] != nil {
		t.Fatalf("no mode configured: %+v", c.Payload)
	}
}

// stubNotify 写一个假的 thinkbot-notify：把参数（每行一个）和 stdin 记到文件里。
func stubNotify(t *testing.T) (bin, argsFile, bodyFile string) {
	dir := t.TempDir()
	bin, argsFile, bodyFile = filepath.Join(dir, "notify"), filepath.Join(dir, "args"), filepath.Join(dir, "body")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + argsFile + "'\ncat > '" + bodyFile + "'\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return
}

// argValue 返回参数列表里 flag 之后的值；flag 不存在返回 "<absent>"。
func argValue(args []string, flag string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			return args[i+1]
		}
	}
	return "<absent>"
}

func readHookCall(t *testing.T, argsFile, bodyFile string) ([]string, string) {
	t.Helper()
	a, err := os.ReadFile(argsFile)
	if err != nil {
		t.Fatalf("hook did not call notify: %v", err)
	}
	b, _ := os.ReadFile(bodyFile)
	os.Remove(argsFile)
	os.Remove(bodyFile)
	return strings.Split(strings.TrimRight(string(a), "\n"), "\n"), string(b)
}

func shortHost(t *testing.T) string {
	out, err := exec.Command("hostname", "-s").Output()
	if err != nil {
		t.Skip("hostname -s unavailable")
	}
	return strings.TrimSpace(string(out))
}

func TestSmartdHookSanitizesDeviceAndPassesMode(t *testing.T) {
	needTool(t, "sh")
	host := shortHost(t)
	bin, argsFile, bodyFile := stubNotify(t)
	conf := t.TempDir()
	env := map[string]string{
		"THINKBOT_NOTIFY_BIN": bin, "THINKBOT_NOTIFY_CONF_DIR": conf,
		"SMARTD_DEVICESTRING": "/dev/sda [SAT]", "SMARTD_DEVICE": "/dev/sda", "SMARTD_DEVICETYPE": "sat",
		"SMARTD_FAILTYPE":    "EmailTest",
		"SMARTD_DEVICEINFO":  "WDC WD40EFRX-68N32N0, S/N:WD-WCC7K1234567, WWN:5-0014ee-2b6b1c3d4, FW:82.00A82, 4.00 TB",
		"SMARTD_FULLMESSAGE": "This is a test email",
	}
	if code, _, stderr := runScript(t, env, "", "60thinkbot-notify"); code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	args, body := readHookCall(t, argsFile, bodyFile)
	if got := argValue(args, "-k"); got != "smartd/sda/EmailTest" {
		t.Fatalf("dedup key %q (%q)", got, args)
	}
	if got := argValue(args, "-s"); got != host+"/smartd/sda" {
		t.Fatalf("source %q", got)
	}
	if got := argValue(args, "-t"); got != "SMART EmailTest on sda" {
		t.Fatalf("title %q", got)
	}
	if got := argValue(args, "-l"); got != "info" {
		t.Fatalf("level %q", got)
	}
	if got := argValue(args, "-m"); got != "<absent>" {
		t.Fatalf("hook must not hardcode a mode, got %q", got)
	}
	for _, want := range []string{"device: /dev/sda [SAT]", "S/N:WD-WCC7K1234567", "WDC WD40EFRX-68N32N0", "event: EmailTest", "This is a test email"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}

	// 硬件钩子专用 mode：环境变量 / notify.hook-mode 文件 → 透传 -m
	env["THINKBOT_NOTIFY_HOOK_MODE"] = "raw"
	runScript(t, env, "", "60thinkbot-notify")
	if args, _ = readHookCall(t, argsFile, bodyFile); argValue(args, "-m") != "raw" {
		t.Fatalf("hook mode env: %q", args)
	}
	delete(env, "THINKBOT_NOTIFY_HOOK_MODE")
	writeFile(t, filepath.Join(conf, "notify.hook-mode"), "raw\n")
	runScript(t, env, "", "60thinkbot-notify")
	if args, _ = readHookCall(t, argsFile, bodyFile); argValue(args, "-m") != "raw" {
		t.Fatalf("hook mode file: %q", args)
	}
	writeFile(t, filepath.Join(conf, "notify.hook-mode"), "shout\n")
	runScript(t, env, "", "60thinkbot-notify")
	if args, _ = readHookCall(t, argsFile, bodyFile); argValue(args, "-m") != "<absent>" {
		t.Fatalf("invalid hook mode must be ignored: %q", args)
	}
	os.Remove(filepath.Join(conf, "notify.hook-mode"))

	// 控制器后面的盘共用一个设备路径：带上盘号；失败类型 → critical
	env["SMARTD_DEVICESTRING"] = "/dev/bus/0 [megaraid_disk_01] [SAT]"
	env["SMARTD_DEVICE"] = "/dev/bus/0"
	env["SMARTD_DEVICETYPE"] = "sat+megaraid,1"
	env["SMARTD_FAILTYPE"] = "CurrentPendingSector"
	runScript(t, env, "", "60thinkbot-notify")
	args, _ = readHookCall(t, argsFile, bodyFile)
	if got := argValue(args, "-k"); got != "smartd/bus-0-megaraid1/CurrentPendingSector" {
		t.Fatalf("megaraid key %q", got)
	}
	if argValue(args, "-l") != "critical" || argValue(args, "-s") != host+"/smartd/bus-0-megaraid1" {
		t.Fatalf("%q", args)
	}
}

func TestMdadmHookSanitizesKeyAndKeepsMdstat(t *testing.T) {
	needTool(t, "sh")
	host := shortHost(t)
	bin, argsFile, bodyFile := stubNotify(t)
	env := map[string]string{"THINKBOT_NOTIFY_BIN": bin, "THINKBOT_NOTIFY_CONF_DIR": t.TempDir()}
	if code, _, stderr := runScript(t, env, "", "thinkbot-mdadm-hook", "Fail", "/dev/md/0", "/dev/sdb2"); code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	args, body := readHookCall(t, argsFile, bodyFile)
	if got := argValue(args, "-k"); got != "mdadm/md-0/Fail/sdb2" {
		t.Fatalf("dedup key %q", got)
	}
	if argValue(args, "-l") != "critical" || argValue(args, "-s") != host+"/mdadm" || argValue(args, "-m") != "<absent>" {
		t.Fatalf("%q", args)
	}
	if argValue(args, "-t") != "mdadm Fail on /dev/md/0 (/dev/sdb2)" {
		t.Fatalf("title %q", argValue(args, "-t"))
	}
	for _, want := range []string{"event: Fail", "array: /dev/md/0", "component: /dev/sdb2", "disk: ", "/proc/mdstat:"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if mdstat, err := os.ReadFile("/proc/mdstat"); err == nil && !strings.Contains(body, strings.TrimSpace(string(mdstat))) {
		t.Fatalf("body must carry the full /proc/mdstat:\n%s", body)
	}
	// 无 component 的事件
	runScript(t, env, "", "thinkbot-mdadm-hook", "DegradedArray", "/dev/md0")
	if args, _ = readHookCall(t, argsFile, bodyFile); argValue(args, "-k") != "mdadm/md0/DegradedArray" {
		t.Fatalf("%q", args)
	}
	env["THINKBOT_NOTIFY_HOOK_MODE"] = "raw"
	runScript(t, env, "", "thinkbot-mdadm-hook", "TestMessage", "/dev/md0")
	if args, _ = readHookCall(t, argsFile, bodyFile); argValue(args, "-m") != "raw" || argValue(args, "-l") != "info" {
		t.Fatalf("%q", args)
	}
}

// 端到端：旧主机上只有 per-bot notify.url 的钩子 → 新发送脚本 → HTTP。
func TestHookToSenderEndToEndWithLegacyURL(t *testing.T) {
	needTool(t, "sh")
	needTool(t, "python3")
	rec := newNotifyRecorder(t)
	conf := t.TempDir()
	writeFile(t, filepath.Join(conf, "notify.token"), "tbn_e2e\n")
	writeFile(t, filepath.Join(conf, "notify.url"), rec.srv.URL+"/api/bots/bot-2d8f9b087270da0bcfe177a5/notify\n")
	sender, err := filepath.Abs(filepath.Join(scriptsDir, "thinkbot-notify"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"THINKBOT_NOTIFY_BIN": sender, "THINKBOT_NOTIFY_CONF_DIR": conf,
		"SMARTD_DEVICESTRING": "/dev/sda [SAT]", "SMARTD_DEVICE": "/dev/sda", "SMARTD_FAILTYPE": "EmailTest",
		"SMARTD_DEVICEINFO": "WDC WD40EFRX, S/N:WD-1"}
	if code, _, stderr := runScript(t, env, "", "60thinkbot-notify"); code != 0 {
		t.Fatalf("exit %d %s", code, stderr)
	}
	calls := rec.take()
	if len(calls) != 1 {
		t.Fatalf("calls=%d", len(calls))
	}
	p := calls[0].Payload
	if calls[0].Path != "/api/bots/bot-2d8f9b087270da0bcfe177a5/notify" || p["bot"] != "bot-2d8f9b087270da0bcfe177a5" ||
		p["dedup_key"] != "smartd/sda/EmailTest" || p["title"] != "SMART EmailTest on sda" || p["mode"] != nil {
		t.Fatalf("%+v", calls[0])
	}
	if !strings.Contains(p["body"].(string), "S/N:WD-1") {
		t.Fatalf("body: %v", p["body"])
	}
}
