package sandbox

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/kasuganosora/thinkbot/llm"
)

// llmOrchestrateStripsPrefix 记录一个容易反复踩的契约：
// llm/orchestrate.go 的 stripSandboxPrefixes 会在工具 schema 送进模型前剥掉
// SandboxToolPrefix，所以模型看到的是 exec / session_spawn / session_poll ...，
// 而注册表、toolperm 规则、管理界面用的是 sandbox_exec / sandbox_session_*。
//
// 曾出现过的真实故障：所有提示词、工具描述、auto 升级 hint、错误文案里写的
// 都是全名，模型照着调必然 "tool not found"；尤其 background=auto 失败后
// 返回的 "retry with sandbox_session_spawn" 是一个模型根本调不到的名字。
//
// 因此这里用 AST 扫本包所有非测试 .go 的字符串字面量：允许「值恰好等于某个
// 注册全名」（那是定义/比较/日志 key），但禁止把全名嵌进任何句子里。
// 这样描述、prompt、hint、error 四类文本一次性锁死，新增文案不会漏网。
func TestNoFullSandboxToolNamesInLLMVisibleText(t *testing.T) {
	if llm.SandboxToolPrefix != "sandbox_" {
		t.Fatalf("SandboxToolPrefix changed to %q; re-check the visible-name contract", llm.SandboxToolPrefix)
	}

	fullNames := WorkspaceToolNames()
	exact := make(map[string]bool, len(fullNames))
	var prefixed []string
	for _, n := range fullNames {
		exact[n] = true
		// 只有带前缀的名字才会在模型侧被改写；run_code 之类本来就是短名，
		// 出现在文案里是合法的，无需检查。
		if strings.HasPrefix(n, llm.SandboxToolPrefix) {
			prefixed = append(prefixed, n)
		}
	}

	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			checked++
			if exact[v] {
				return true // 注册名本身，合法
			}
			for _, full := range prefixed {
				if strings.Contains(v, full) {
					pos := fset.Position(lit.Pos())
					t.Errorf("%s:%d: LLM-visible text still uses full tool name %q (model sees %q): %.120s",
						path, pos.Line, full, strings.TrimPrefix(full, llm.SandboxToolPrefix), v)
				}
			}
			return true
		})
	}
	if checked == 0 {
		t.Fatal("no string literals scanned; AST walk is broken")
	}
}

// 反向断言：光把全名删掉不算修好，模型必须真的能在文案里看到短名。
func TestVisibleSessionNamesAreDocumented(t *testing.T) {
	prompt := botWorkspaceToolPromptSection.Content
	// prompt 的循环段用 "poll/send" 裸词指代这两个会话工具，只有 spawn/watch
	// 是带 session_ 前缀的完整可见名。
	for _, want := range []string{"session_spawn", "session_watch", "poll", "send", "background=\"auto\""} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt section does not mention visible tool name %q", want)
		}
	}

	// autoGuard 非 nil 时 exec 的 schema/描述才会追加 auto 与会话短名提示，
	// 这正是生产 wecom 回合里模型看到的那份定义。
	defs := botWorkspaceToolDefsForAuto(nil, "test-bot", true, func() bool { return true })
	var execDef *llm.Tool
	for i := range defs {
		if defs[i].Name == "sandbox_exec" {
			execDef = &defs[i]
			break
		}
	}
	if execDef == nil {
		t.Fatal("sandbox_exec definition missing")
	}
	// 会话短名一部分在 Description 尾部，一部分在 background 参数的
	// schema description 里，两处都是模型可见文本，一起检查。
	blob := execDef.Description
	if b, err := json.Marshal(execDef.Parameters); err == nil {
		blob += string(b)
	} else {
		t.Fatalf("marshal exec parameters: %v", err)
	}
	for _, want := range []string{"session_spawn", "session_poll", "auto"} {
		if !strings.Contains(blob, want) {
			t.Errorf("sandbox_exec visible text does not mention %q", want)
		}
	}
	if strings.Contains(blob, llm.SandboxToolPrefix+"session_") {
		t.Errorf("sandbox_exec visible text still uses a prefixed session name")
	}
}
