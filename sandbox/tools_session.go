package sandbox

// ============================================================================
// Interactive session tools
//
// Tool names are deliberately prefixed with sandbox_session_ (docs §4): the
// bare name "spawn" is already registered by subagent/tools.go and would be
// silently swallowed by the registry's static-first de-duplication.
//
// botID and agent are captured from the provider closure / session context.
// They are NEVER accepted from model input: letting a model choose the agent
// name would reset its sinceSeq cursor and replay the whole raw history.
// ============================================================================

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/secret"
)

const (
	ToolSessionSpawn    = "sandbox_session_spawn"
	ToolSessionPoll     = "sandbox_session_poll"
	ToolSessionSend     = "sandbox_session_send"
	ToolSessionSignal   = "sandbox_session_signal"
	ToolSessionClose    = "sandbox_session_close"
	ToolSessionWatch    = "sandbox_session_watch"
	ToolSessionCloseAll = "sandbox_session_close_all"
)

// sessionAgent is fixed for the first release; multi-agent cursors are a P2
// item. Keeping it out of tool parameters is a security boundary (docs §4).
const sessionAgent = "default"

var sessionToolNames = []string{
	ToolSessionSpawn, ToolSessionPoll, ToolSessionSend,
	ToolSessionSignal, ToolSessionClose, ToolSessionWatch, ToolSessionCloseAll,
}

func buildSessionTools(mgr *BotWorkspaceManager, botID string) []llm.Tool {
	return []llm.Tool{
		buildSessionSpawnTool(mgr, botID),
		buildSessionPollTool(mgr, botID),
		buildSessionSendTool(mgr, botID),
		buildSessionSignalTool(mgr, botID),
		buildSessionCloseTool(mgr, botID),
		buildSessionWatchTool(mgr, botID),
		buildSessionCloseAllTool(mgr, botID),
	}
}

func sessionRegistryOf(mgr *BotWorkspaceManager) (*SessionRegistry, error) {
	if mgr == nil || mgr.Sessions() == nil {
		return nil, ErrSessionUnsupported
	}
	return mgr.Sessions(), nil
}

func sessionString(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return strings.TrimSpace(v)
}

// rejectUnresolvedPlaceholders is a last-mile guard on the credential path.
// llm/orchestrate.go resolves {{secret:name}} before Execute is called, so a
// token still present here means the name is unknown, belongs to another bot,
// or the tool was invoked outside the orchestrator. Typing that literal text
// into a password prompt would burn an authentication attempt and could store
// the token in tool-call history. Fail loudly instead.
func rejectUnresolvedPlaceholders(scope string, parts ...string) error {
	for _, p := range parts {
		if names := secret.PlaceholderNames(p); len(names) > 0 {
			return fmt.Errorf("%w: %s still contains unresolved secret placeholder(s) %v; "+
				"call secret_input action=list/request and use a name that resolves for this bot",
				ErrSessionBadInput, scope, names)
		}
	}
	return nil
}

func sessionViewToMap(v SessionView) map[string]any {
	out := map[string]any{
		"sessionId":  v.SessionID,
		"pid":        v.PID,
		"pgid":       v.PGID,
		"state":      string(v.State),
		"backend":    v.Backend,
		"seq":        v.Seq,
		"settledSeq": v.SettledSeq,
		"delta":      v.Delta,
		"screen":     v.Screen,
		"urls":       v.URLs,
		"truncated":  v.Truncated,
		"merged":     v.Merged,
		"health":     v.Health,
	}
	if v.ExitCode != nil {
		out["exitCode"] = *v.ExitCode
	}
	if v.Hint != "" {
		out["hint"] = v.Hint
	}
	if v.StderrTail != "" {
		out["stderrTail"] = v.StderrTail
	}
	if v.BootDiag != "" {
		out["bootDiag"] = v.BootDiag
	}
	if v.Reason != "" {
		out["reason"] = v.Reason
	}
	if v.LastSeq != 0 {
		out["lastSeq"] = v.LastSeq
	}
	if v.Orphaned {
		out["orphaned"] = true
	}
	return out
}

func buildSessionSpawnTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: ToolSessionSpawn,
		Description: "Start an interactive CLI as a BACKGROUND session and return immediately with an opaque sessionId. " +
			"Use this for TUIs, REPLs, logins and any program that needs stdin later; use sandbox_exec for one-shot commands. " +
			"The session keeps running between tool calls. There is NO awaiting_input gate: state=running always allows send. " +
			"After spawn, call sandbox_session_poll to read the screen, then decide whether to send. " +
			"URLs in the result are out-of-band actions (login / OAuth / device code): hand them to the user instead of typing keys. " +
			"IMPORTANT: a zero-output session may have failed to start — read bootDiag / stderrTail before assuming it is working. " +
			"Limits: a few concurrent sessions per bot; the session is reclaimed after an idle period, and stopping a chat turn does NOT kill it. " +
			"For ssh and other login programs, spawn first and answer host-key / password / passphrase prompts with send + poll; never put a credential in the command line.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command that starts the interactive program.",
				},
				"cwd": map[string]any{
					"type":        "string",
					"description": "Working directory relative to the workspace root. Optional.",
				},
				"env": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Extra environment variables as KEY=VALUE. Host variables are NOT inherited wholesale; product-config and credential-like names are dropped.",
				},
				"cols": map[string]any{"type": "integer", "description": "Terminal columns (default 120)."},
				"rows": map[string]any{"type": "integer", "description": "Terminal rows (default 40)."},
				"term": map[string]any{"type": "string", "description": "TERM value (default xterm-256color)."},
			},
			"required": []string{"command"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			command := sessionString(m, "command")
			if command == "" {
				return nil, fmt.Errorf("command is required")
			}
			// A command line is visible to `ps` inside the sandbox and to the
			// shell history: never let an unresolved placeholder (or a literal
			// credential) be baked into it. Prefer answering the prompt via send.
			if err := rejectUnresolvedPlaceholders("command", command); err != nil {
				return nil, err
			}
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			// Resolve cwd through the same guard as sandbox_exec.
			ws, err := mgr.GetOrCreate(botID)
			if err != nil {
				return nil, err
			}
			cwd := ws.WorkDir()
			if rel := sessionString(m, "cwd"); rel != "" {
				if err := checkWorkspacePath("cwd", rel); err != nil {
					return nil, err
				}
				cwd, err = validatePath(cwd, rel)
				if err != nil {
					return nil, err
				}
			}
			var env []string
			if raw, ok := m["env"].([]any); ok {
				for _, item := range raw {
					if s, ok := item.(string); ok && strings.Contains(s, "=") {
						if err := rejectUnresolvedPlaceholders("env", s); err != nil {
							return nil, err
						}
						env = append(env, s)
					}
				}
			}
			cols, rows := uint16(0), uint16(0)
			if v, ok := toInt(m["cols"]); ok && v > 0 && v <= 1000 {
				cols = uint16(v)
			}
			if v, ok := toInt(m["rows"]); ok && v > 0 && v <= 500 {
				rows = uint16(v)
			}
			view, err := reg.Spawn(ctx, botID, sessionAgent, SpawnRequest{
				Command: command, Cwd: cwd, Env: env,
				Cols: cols, Rows: rows, Term: sessionString(m, "term"),
			})
			if err != nil {
				return nil, err
			}
			return sessionViewToMap(view), nil
		}),
	}
}

func buildSessionPollTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: ToolSessionPoll,
		Description: "Read the current screen and semantic changes of a background session. " +
			"Non-blocking by default. ALWAYS use the returned settledSeq as the sinceSeq of your next poll " +
			"(seq is a raw byte counter and using it can silently skip content). Repeating the same sinceSeq returns the same delta. " +
			"health is diagnostic only — it never changes the session state and never triggers an automatic action. " +
			"Read the screen before every send: a command may open a confirmation dialog, or may not. " +
			"URLs are out-of-band actions to hand to the user. If state=lost, use reason and lastSeq to decide to reopen rather than keep polling.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sessionId": map[string]any{"type": "string", "description": "Opaque session handle from spawn."},
				"sinceSeq":  map[string]any{"type": "integer", "description": "Use the settledSeq returned by the previous poll. Optional; the server keeps a per-agent cursor."},
				"mode":      map[string]any{"type": "string", "enum": []string{"screen", "tail", "raw"}, "description": "screen (default): screen + semantic delta; raw: raw bytes for debugging."},
				"lines":     map[string]any{"type": "integer", "description": "Maximum screen lines (default 40)."},
				"waitMs":    map[string]any{"type": "integer", "description": "Wait up to this long for new output, capped at 8000ms. Default 0 (non-blocking)."},
			},
			"required": []string{"sessionId"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			id := sessionString(m, "sessionId")
			if id == "" {
				return nil, fmt.Errorf("sessionId is required")
			}
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			var since uint64
			if v, ok := toInt(m["sinceSeq"]); ok && v > 0 {
				since = uint64(v)
			}
			mode := sessionString(m, "mode")
			if mode == "" {
				mode = "screen"
			}
			lines := 40
			if v, ok := toInt(m["lines"]); ok && v > 0 && v <= 500 {
				lines = v
			}
			var wait time.Duration
			if v, ok := toInt(m["waitMs"]); ok && v > 0 {
				wait = time.Duration(v) * time.Millisecond
			}
			view, err := reg.Poll(botID, sessionAgent, id, since, mode, lines, wait)
			if err != nil {
				return nil, err
			}
			return sessionViewToMap(view), nil
		}),
	}
}

var keySequences = map[string]string{
	"Enter": "\r", "enter": "\r", "Return": "\r",
	"Esc": "\x1b", "esc": "\x1b", "Escape": "\x1b",
	"Tab": "\t", "Space": " ", "Backspace": "\x7f",
	"Ctrl+C": "\x03", "Ctrl+D": "\x04", "Ctrl+L": "\x0c",
	"Up": "\x1b[A", "Down": "\x1b[B", "Right": "\x1b[C", "Left": "\x1b[D",
}

func buildSessionSendTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: ToolSessionSend,
		Description: "Write input to a running background session. text is written literally with NO automatic newline; " +
			"set submit=true to send the text and then Enter (~100ms apart, because TUIs treat tightly-coupled text+Enter as a paste). " +
			"keys sends a key name (Enter, Esc, Tab, Up, Down, y, n, Ctrl+C...). " +
			"CREDENTIALS (ssh password, key passphrase, OTP, PIN): NEVER paste a literal value. Put the {{secret:name}} placeholder from secret_input in text; " +
			"the orchestrator resolves it only at execution time, so the real value never enters tool-call history. An unresolved placeholder is rejected. " +
			"Use secret_input action=list/request first if no name is ready, and never blindly retry after a connect_failed verdict. " +
			"CRITICAL: on a confirmation screen, Enter or y means APPROVING an action (often shell execution) — read the screen with poll first. " +
			"ALWAYS poll after sending: a command may open a nested confirmation dialog, or may not. Never send two commands back to back and never guess results with a fixed sleep. " +
			"After Ctrl+C, poll and wait for a fresh shell prompt before the next send: the interrupted input line can swallow the first byte (measured on a real ssh session). " +
			"The returned written byte count is NOT proof the program consumed the input; only a screen change is. " +
			"send is a sensitive tool governed by permission rules regardless of poll being read-only.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sessionId": map[string]any{"type": "string"},
				"text":      map[string]any{"type": "string", "description": "Text to type. No newline is appended unless submit=true. For passwords / passphrases / PINs use a {{secret:name}} placeholder, never the literal value."},
				"keys":      map[string]any{"type": "string", "description": "A key name, or a literal single character."},
				"submit":    map[string]any{"type": "boolean", "description": "After writing text, send Enter separately. Default false."},
			},
			"required": []string{"sessionId"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			id := sessionString(m, "sessionId")
			if id == "" {
				return nil, fmt.Errorf("sessionId is required")
			}
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			text := m["text"]
			str, _ := text.(string)
			keys, _ := m["keys"].(string)
			if str == "" && keys == "" {
				return nil, fmt.Errorf("text or keys is required")
			}
			// By this point the orchestrator has already resolved registered
			// {{secret:name}} tokens. A remaining token would be typed verbatim
			// into (typically) a password prompt: reject it rather than burn an
			// authentication attempt or persist it in the tool call record.
			if err := rejectUnresolvedPlaceholders("send", str, keys); err != nil {
				return nil, err
			}
			var data []byte
			if str != "" {
				data = append(data, str...)
			}
			if keys != "" {
				seq, ok := keySequences[keys]
				if !ok {
					seq = keys
				}
				data = append(data, seq...)
			}
			submit, _ := m["submit"].(bool)
			if submit && str != "" && keys == "" {
				if err := reg.Send(botID, id, data); err != nil {
					return nil, err
				}
				time.Sleep(100 * time.Millisecond) // input pacing, NOT a result wait
				data = []byte("\r")
			}
			if err := reg.Send(botID, id, data); err != nil {
				return nil, err
			}
			return map[string]any{
				"written": len(data),
				"warning": "written != accepted; poll the screen to confirm the program consumed the input",
				"hint":    "poll before and after every send; confirmation screens may appear",
			}, nil
		}),
	}
}

func buildSessionSignalTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: ToolSessionSignal,
		Description: "Signal a background session's process group. WARNING: on a raw TUI, INT terminates the whole session " +
			"(state=exited) and it cannot be reused — spawn a new one. This is NOT 'interrupt the current task and continue'. " +
			"Prefer Ctrl+C via send(keys) when the program is reading stdin.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sessionId": map[string]any{"type": "string"},
				"sig":       map[string]any{"type": "string", "enum": []string{"INT", "TERM", "KILL"}},
			},
			"required": []string{"sessionId", "sig"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			id := sessionString(m, "sessionId")
			sig := strings.ToUpper(sessionString(m, "sig"))
			if id == "" || sig == "" {
				return nil, fmt.Errorf("sessionId and sig are required")
			}
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			if err := reg.Signal(botID, id, sig); err != nil {
				return nil, err
			}
			return map[string]any{
				"state":   "signalled",
				"warning": "INT usually ends the session; poll to confirm state before sending again",
			}, nil
		}),
	}
}

func buildSessionCloseTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name:        ToolSessionClose,
		Description: "Close one background session: terminate its process group and release the screen buffer. Irreversible.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"sessionId": map[string]any{"type": "string"},
			},
			"required": []string{"sessionId"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid input: expected object")
			}
			id := sessionString(m, "sessionId")
			if id == "" {
				return nil, fmt.Errorf("sessionId is required")
			}
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			if err := reg.Close(botID, id); err != nil {
				return nil, err
			}
			return map[string]any{"sessionId": id, "state": "closed"}, nil
		}),
	}
}

func buildSessionWatchTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name: ToolSessionWatch,
		Description: "Read-only attention queue across this bot's background sessions. " +
			"Returns sessions that need a decision first: P0 actionable prompt / credential prompt / connection failure / suspected stuck / daemon fault, " +
			"P1 exited or idle-prompt or orphaned, P2 quiet progress (folded). " +
			"watch does NOT consume output, does NOT advance your cursor, and never sends or kills anything. " +
			"Use attention[].sinceSeq with sandbox_session_poll. botID and agent are supplied by the server and cannot be set here. " +
			"Set waitMs to long-poll: the call blocks until a session needs a decision or the wait elapses, " +
			"instead of you sleeping and calling watch repeatedly. On timeout it returns the current queue; " +
			"act only on attention items, never on a plain timeout.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"limit":        map[string]any{"type": "integer", "description": "Maximum attention items (default 8)."},
				"includeQuiet": map[string]any{"type": "boolean", "description": "Include P2 items in the list (default false; quiet is returned as a count)."},
				"waitMs": map[string]any{
					"type":        "integer",
					"description": "Optional long-poll wait in milliseconds (0 = instant snapshot, default 0). Server caps it at sandbox.session_watch_wait_max (30s). The call returns early when a session produces an actionable prompt, exits, becomes orphaned, or its daemon faults.",
				},
			},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			m, _ := input.(map[string]any)
			if m == nil {
				m = map[string]any{}
			}
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			limit := 8
			if v, ok := toInt(m["limit"]); ok && v > 0 && v <= 64 {
				limit = v
			}
			includeQuiet, _ := m["includeQuiet"].(bool)
			waitMs := 0
			if v, ok := toInt(m["waitMs"]); ok && v > 0 {
				waitMs = v
			}
			if waitMs == 0 {
				return reg.Watch(botID, sessionAgent, limit, includeQuiet), nil
			}
			toolCtx := context.Background()
			if ctx != nil && ctx.Context != nil {
				toolCtx = ctx.Context
			}
			// SSE heartbeat rides the SAME SendProgress channel that exec uses.
			// It is a progress signal only: no message injection, no tool call,
			// no new agent turn (docs §11 "不得绕过 toolperm").
			var onHeartbeat func(int64)
			if ctx != nil && ctx.SendProgress != nil {
				onHeartbeat = func(elapsedMs int64) {
					ctx.SendProgress(map[string]any{
						"stream": "heartbeat", "phase": "session_watch", "elapsedMs": elapsedMs,
					})
				}
			}
			return reg.WaitForAttention(toolCtx, botID, sessionAgent, limit, includeQuiet,
				time.Duration(waitMs)*time.Millisecond, onHeartbeat), nil
		}),
	}
}

func buildSessionCloseAllTool(mgr *BotWorkspaceManager, botID string) llm.Tool {
	return llm.Tool{
		Name:        ToolSessionCloseAll,
		Description: "Close ALL background sessions belonging to this bot. The bot scope is supplied by the server, never by model input. Irreversible.",
		Parameters:  map[string]any{"type": "object", "properties": map[string]any{}},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			reg, err := sessionRegistryOf(mgr)
			if err != nil {
				return nil, err
			}
			return map[string]any{"closed": reg.CloseAll(botID)}, nil
		}),
	}
}
