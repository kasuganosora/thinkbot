package api

import "testing"

// phantom tool call 判定：只有「解析出 map 且长度为 0」才算空参数占位。
//
// 回归背景：某些 LLM（如 GLM）流式输出工具调用时，会先发一个空参数 {} 的占位
// call，再发带真实参数的 call。占位 call 永远不会被执行、也收不到结果，
// 若不主动收敛就会让前端卡片永久停在「执行中」。
func TestIsEmptyToolInput(t *testing.T) {
	cases := []struct {
		name  string
		input any
		want  bool
	}{
		{"空 map 是 phantom 特征", map[string]any{}, true},
		{"带参数的真实调用", map[string]any{"url": "https://example.com"}, false},
		{"单个零值参数也算真实调用", map[string]any{"maxChars": 0}, false},
		// nil / 非 map 说明事件缺字段，不能据此断定为 phantom：
		// 误判会把真实调用标记成已取代，掩盖真正的执行卡死。
		{"nil 不算 phantom", nil, false},
		{"字符串不算 phantom", "{}", false},
		{"空字符串不算 phantom", "", false},
		{"数组不算 phantom", []any{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isEmptyToolInput(tc.input); got != tc.want {
				t.Errorf("isEmptyToolInput(%#v) = %v, want %v", tc.input, got, tc.want)
			}
		})
	}
}

// 只有 running 且空参数的调用才是待收敛的 phantom。
func TestIsPhantomRunning(t *testing.T) {
	cases := []struct {
		name string
		tc   map[string]any
		want bool
	}{
		{
			name: "running + 空参数 = phantom",
			tc:   map[string]any{"status": "running", "input": map[string]any{}},
			want: true,
		},
		{
			// 真实调用即使卡住也必须保留 running，交由前端超时提示。
			// 若误标成已取代，会掩盖真正的执行卡死。
			name: "running + 有参数 = 真实调用，不可收敛",
			tc:   map[string]any{"status": "running", "input": map[string]any{"url": "x"}},
			want: false,
		},
		{
			name: "已完成的空参数调用不再处理",
			tc:   map[string]any{"status": "success", "input": map[string]any{}},
			want: false,
		},
		{
			name: "已被取代的不重复处理",
			tc:   map[string]any{"status": "superseded", "input": map[string]any{}},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPhantomRunning(tc.tc); got != tc.want {
				t.Errorf("isPhantomRunning(%#v) = %v, want %v", tc.tc, got, tc.want)
			}
		})
	}
}

// 真实调用到达时，应精确命中同名的 phantom。
func TestFindPhantomToolCall(t *testing.T) {
	realInput := map[string]any{"maxChars": 3000}

	t.Run("命中同名 phantom", func(t *testing.T) {
		calls := []map[string]any{
			{"id": "c1", "name": "browser__get_text", "status": "running", "input": map[string]any{}},
		}
		if got := findPhantomToolCall(calls, "browser__get_text", realInput); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})

	t.Run("不同名的 phantom 不受影响", func(t *testing.T) {
		calls := []map[string]any{
			{"id": "c1", "name": "browser__navigate", "status": "running", "input": map[string]any{}},
		}
		if got := findPhantomToolCall(calls, "browser__get_text", realInput); got != -1 {
			t.Errorf("got %d, want -1 (不应跨工具取代)", got)
		}
	})

	t.Run("新调用本身是空参数则不取代", func(t *testing.T) {
		calls := []map[string]any{
			{"id": "c1", "name": "browser__get_text", "status": "running", "input": map[string]any{}},
		}
		// 两个连续 phantom 时不能互相取代，否则先到的那个会被错误收敛
		if got := findPhantomToolCall(calls, "browser__get_text", map[string]any{}); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("已成功的同名调用不会被误取代", func(t *testing.T) {
		calls := []map[string]any{
			{"id": "c1", "name": "browser__get_text", "status": "success", "input": map[string]any{}},
		}
		if got := findPhantomToolCall(calls, "browser__get_text", realInput); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})

	t.Run("多条中精确命中 phantom 而非真实调用", func(t *testing.T) {
		calls := []map[string]any{
			{"id": "c1", "name": "browser__navigate", "status": "success", "input": map[string]any{"url": "x"}},
			{"id": "c2", "name": "browser__get_text", "status": "running", "input": map[string]any{"maxChars": 1}},
			{"id": "c3", "name": "browser__get_text", "status": "running", "input": map[string]any{}},
		}
		got := findPhantomToolCall(calls, "browser__get_text", realInput)
		if got != 2 {
			t.Fatalf("got %d, want 2 (应命中空参数的 c3，而非正在跑的 c2)", got)
		}
	})

	t.Run("空列表安全", func(t *testing.T) {
		if got := findPhantomToolCall(nil, "browser__get_text", realInput); got != -1 {
			t.Errorf("got %d, want -1", got)
		}
	})
}

// 标记为已取代后，状态与说明文案都要落到累积结构上，
// 供 SSE 推送与落库共用（前端依赖 status 字段判定终态）。
func TestMarkToolCallSuperseded(t *testing.T) {
	tc := map[string]any{
		"id":     "c1",
		"name":   "browser__get_text",
		"status": "running",
		"input":  map[string]any{},
	}
	markToolCallSuperseded(tc)

	if tc["status"] != "superseded" {
		t.Errorf("status = %v, want superseded", tc["status"])
	}
	if tc["output"] != phantomSupersededMsg {
		t.Errorf("output = %v, want %q", tc["output"], phantomSupersededMsg)
	}
	// 标记后不应再被重复收敛（幂等）
	if isPhantomRunning(tc) {
		t.Error("已标记的调用仍被判定为待收敛 phantom")
	}
}

// 模拟截图中的真实场景：navigate 的 phantom 有后继可取代，
// 而 get_text 的 phantom 是本轮最后一个调用、无后继 —— 必须靠结束时的
// 兜底清扫收敛，否则永久停在「执行中」（这正是本次修复的 bug）。
func TestPhantomSweepCoversTrailingPhantom(t *testing.T) {
	calls := []map[string]any{
		{"id": "n1", "name": "browser__navigate", "status": "running", "input": map[string]any{}},
		{"id": "n2", "name": "browser__navigate", "status": "success", "input": map[string]any{"url": "https://bing.com"}},
		{"id": "g1", "name": "browser__get_text", "status": "running", "input": map[string]any{}},
		{"id": "g2", "name": "browser__get_text", "status": "success", "input": map[string]any{"maxChars": 3000}},
	}

	// navigate 的真实调用到达时取代 phantom
	if idx := findPhantomToolCall(calls, "browser__navigate", map[string]any{"url": "https://bing.com"}); idx >= 0 {
		markToolCallSuperseded(calls[idx])
	}

	// get_text 的 phantom 若只依赖 suppress 就漏了（真实调用事件已处理完），
	// 结束时的兜底清扫必须把它收敛
	swept := 0
	for i := range calls {
		if isPhantomRunning(calls[i]) {
			markToolCallSuperseded(calls[i])
			swept++
		}
	}
	if swept != 1 {
		t.Errorf("兜底清扫了 %d 条，want 1（g1）", swept)
	}

	for _, c := range calls {
		if c["status"] == "running" {
			t.Errorf("调用 %v 仍停在 running，前端会卡在「执行中」", c["id"])
		}
	}
	if calls[3]["status"] != "success" {
		t.Errorf("真实调用 g2 状态被破坏: %v", calls[3]["status"])
	}
}
