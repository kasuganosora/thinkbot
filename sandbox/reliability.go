package sandbox

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ============================================================================
// 命令执行「完整性 / 可信度」检测
//
// 背景：shell 工具在沙箱内执行 lint/test/build 等验证型命令时，若因资源不足（OOM）、
// 超时或被信号杀死而中途失败 / 部分产出，旧实现无法向 LLM 表达「结果不完整 / 不可信」，
// 导致 LLM 把半份结果当成完整结果去推理（见 docs/shell_reliable_result_design.md）。
//
// 检测分三层，最终汇聚到 ExecResult.Reliable/Aborted/OOMKilled/Warnings：
//   1. 退出码 / 超时 / 输出文本特征（finalizeExecResult，在 runCommandWithStreaming 收尾处调用）
//   2. cgroup oom_kill 前后对比（调用方在 ExecStream 中执行，叠加 OOMKilled）
//   3. 验证型命令 OOM 时由调用方自动重试（RetryOOMWithElevatedMemory）
// ============================================================================

// fatalTextPatterns 是输出文本中可能表明命令被 OOM / 信号杀死的特征。
// 大小写不敏感匹配，用于兜底（覆盖 `cmd | head` 之类管道把退出码掩盖为 0 的情况）。
var fatalTextPatterns = []string{
	"out of memory",
	"cannot allocate memory",
	"signal: killed",
	"signal: terminated",
	"fatal error: runtime: out of memory",
	"killed",
	"signal: segmentation fault",
}

// finalizeExecResult 在命令结束后填充完整性 / 可信度信号（退出码 / 超时 / 输出文本特征）。
// cgroup OOM 由调用方（docker/local 后端）在前后对比后通过 OOMKilled 字段叠加，
// 这里只负责 exit / timeout / 文本扫描，并在最后根据 Aborted || OOMKilled 计算 Reliable。
func finalizeExecResult(result *ExecResult, killReason string) {
	if result == nil {
		return
	}
	// 超时 / 卡死（runCommandWithStreaming 已将 ExitCode 置 -1）
	if result.ExitCode == -1 {
		result.Aborted = true
		switch killReason {
		case "stuck":
			result.Warnings = append(result.Warnings,
				"命令被卡死看门狗终止（长时间无输出/无进展），结果不完整")
		case "hard":
			result.Warnings = append(result.Warnings,
				"命令超过硬上限被强制终止，结果不完整")
		default:
			result.Warnings = append(result.Warnings, "命令超时未跑完，结果不完整")
		}
	}
	// 退出码 137 = 128 + SIGKILL：docker 下 OOM 即返回 137；本地进程被杀死也可能落到此区间。
	if result.ExitCode == 137 {
		result.Aborted = true
		result.Warnings = append(result.Warnings,
			"命令被信号杀死(exit=137)，结果可能不完整（可能因内存不足被 OOM 终止）")
	}
	// 输出文本特征扫描（兜底，覆盖管道掩盖 exit=0 的情况）
	if msg := scanFatalText(result.Stdout + "\n" + result.Stderr); msg != "" {
		result.Aborted = true
		result.Warnings = append(result.Warnings, "输出中出现 OOM/中止特征: "+msg)
	}
	result.Reliable = !result.Aborted && !result.OOMKilled
}

// scanFatalText 在输出尾部扫描 OOM / 中止特征，返回匹配的证据片段（空表示未命中）。
// 仅扫描尾部 4KB，因为此类特征通常出现在输出末尾（如 bash 打印的 "Killed"）。
func scanFatalText(s string) string {
	low := strings.ToLower(s)
	tail := low
	if len(tail) > 4096 {
		tail = tail[len(tail)-4096:]
	}
	for _, p := range fatalTextPatterns {
		if i := strings.Index(tail, p); i >= 0 {
			start := i
			if start > 40 {
				start = i - 40
			}
			end := i + len(p) + 40
			if end > len(tail) {
				end = len(tail)
			}
			return strings.TrimSpace(tail[start:end])
		}
	}
	return ""
}

// readCgroupOOMKill 读取当前进程 cgroup 的 oom_kill 计数（v2 / v1 自动适配）。
// 不支持（无权限 / 非 cgroup 环境）时返回 (0, false)。适用于 local 后端
// （命令直接在宿主 cgroup 内运行）。
func readCgroupOOMKill() (int, bool) {
	if data, err := os.ReadFile("/sys/fs/cgroup/memory.events"); err == nil {
		if n, ok := parseOOMKill(string(data)); ok {
			return n, true
		}
	}
	if data, err := os.ReadFile("/sys/fs/cgroup/memory/memory.oom_control"); err == nil {
		if n, ok := parseOOMKill(string(data)); ok {
			return n, true
		}
	}
	return 0, false
}

// readContainerCgroupOOMKill 读取 docker 容器内 cgroup 的 oom_kill 计数。
// 用于在命令前后对比，可靠判定本次命令是否触发 OOM——不受管道掩盖退出码影响
// （如 `cmd | tee | head` 让管道尾命令成功，外层 docker exec 退出码为 0）。
func readContainerCgroupOOMKill(ctx context.Context, container string) (int, bool) {
	if container == "" {
		return 0, false
	}
	cmd := exec.CommandContext(ctx, "docker", "exec", container, "sh", "-c",
		"cat /sys/fs/cgroup/memory.events 2>/dev/null || cat /sys/fs/cgroup/memory/memory.oom_control 2>/dev/null")
	var out strings.Builder
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return 0, false
	}
	return parseOOMKill(out.String())
}

// parseOOMKill 从 cgroup 文件内容中解析 oom_kill 计数。
// v2 的 memory.events 与 v1 的 memory.oom_control 均包含 "oom_kill N" 行，统一解析。
func parseOOMKill(s string) (int, bool) {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "oom_kill") {
			var n int
			if _, err := fmt.Sscanf(line, "oom_kill %d", &n); err == nil {
				return n, true
			}
		}
	}
	return 0, false
}

// oomRetryElevatedMB 是 OOM 自动重试时临时提升到的沙箱内存上限（MB）。
// 取值依据实测：community(80 包) 的 golangci-lint 峰值约 4.9GB，6GB 可跑完
// （见 docs/shell_reliable_result_design.md）。提升仅作用于内存中的容器上限，不落库。
const oomRetryElevatedMB = int64(6144)

// oomRetryMaxMB 是 OOM 自动重试时内存上限的封顶值（MB）。
// 若单次提升后（6144m）仍 OOM，下次重试会按 2 倍逐级提升
// （6144 → 12288 → 16384 …），直至到达此封顶值后不再继续放大，
// 避免无限制吃光宿主机内存。提升仅作用于内存中的容器上限，不落库。
const oomRetryMaxMB = int64(16384)
