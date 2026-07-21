# 设计文档：Shell 工具结果「完整性 / 可信度」加固

> 状态：已实现（2026-07-21 落地，`sandbox/tools.go` 为唯一事实来源，`tools/shell.go` 错误双实现已删除）
> 作者：thinkbot agent
> 日期：2026-07-21
> 关联事故：bot 沙箱内跑 `golangci-lint`（逐包循环）因 2G 容器内存不足被 OOM killer 反复杀死，仅 8/80 包跑出结果（7 个 errcheck），却被当成「完整 lint 结果」喂给 LLM，导致 LLM 基于残缺信息工作。

---

## 1. 问题陈述

bot 的 shell 工具在沙箱内执行命令（如 lint/test/build），当命令因 **资源不足（OOM）、超时、或被信号杀死** 而**中途失败或部分产出**时，当前机制无法向 LLM 表达「这份结果不完整 / 不可信」。LLM 会把它当成完整结果去推理、修代码，造成：

- 虚假的安全感（以为只有 N 个 issue）；
- 基于残缺结果做出错误的修复（遗漏大量真实问题）。

这不是「内存给多少」的问题，而是**工具结果缺少「完整性契约」**的系统性缺陷。

---

## 2. 根因分析（代码锚点，已确认）

### 2.1 结果结构无「完整性」语义
`sandbox/tools.go` 的 `execResultToToolOutput`（旧 `tools/shell.go:206-212`）返回给 LLM 的 map 原本只有：
```go
return map[string]any{
    "exitCode":  res.ExitCode,
    "stdout":    res.Stdout,
    "stderr":    res.Stderr,
    "truncated": res.Truncated,
    "workdir":   ws.WorkDir(),
}, nil
```
没有 `reliable` / `aborted` / `warnings` 等字段。LLM 只能看到原始文本，无法区分「命令跑完了、输出全」与「命令崩了、只拿到半截」。

### 2.2 OOM / 信号杀死不被识别
- `sandbox/docker.go:409-415` 退出码只从 `exitErr.ExitCode()` 取。OOM 杀死的进程在 docker 下返回 **137（128+SIGKILL）**，但用户脚本 `... | tee /tmp/x | head -300` 让**管道退出码变 0**（`head` 成功），bot 误判成功。
- `sandbox/botcontainer.go:405-411` 同构问题。
- 全程**没有**检测 `Killed` / `out of memory` 文本，也没有对比 cgroup OOM 计数。

### 2.3 `truncated` 语义错位
- `sandbox/docker.go:394-402`：`Truncated` 仅在**输出超长**（超过 `maxOut`）时置真，含义是「输出被截断（但命令跑完了）」。
- 它**不表示**「命令没跑完」。OOM 中途死掉、只拿到部分输出时，`Truncated` 可能为 false（输出没超长），导致「执行中断」和「输出过长」被混淆。

### 2.4 容器 cgroup OOM 计数可读（关键证据）
在容器内实测：`cat /sys/fs/cgroup/memory.events` →
```
oom 409
oom_kill 92
```
说明该容器历史上被 OOM 杀过 92 次。该计数**执行前后对比**即可可靠判定本次命令是否触发 OOM，无需依赖解析文本或退出码。
- cgroup v2：`/sys/fs/cgroup/memory.events` 的 `oom_kill` 字段
- cgroup v1：`/sys/fs/cgroup/memory/memory.oom_control`（`oom_kill` 字段）或 `memory.failcnt`

### 2.5 实测数据
- community 项目（142MB / 80 包，无 `.golangci` 配置）：host 实测 `golangci-lint run ./...` 峰值常驻内存 **≈4.9 GB**。
- 2G 容器下必然 OOM；完整 lint 实际有 **44 个问题**（41 errcheck + 3 staticcheck），容器内只拿到 7 个。

---

## 3. 目标 / 非目标

**目标**
- G1：shell 工具能**检测**命令是否完整、可信地执行（OOM / 信号杀 / 输出截断 / 文本异常）。
- G2：把「完整性」作为**结构化信号**回传给 LLM（不只是文本）。
- G3：agent 机制层**拦截**不可信结果，避免 LLM 基于半份结果继续工作（lint/test/build 等验证型工具尤其重要）。

**非目标**
- 不改 bot 默认内存（2G 合理）；不做「无限内存」。
- 不重写 exec 后端（docker/local 共用检测逻辑）。
- 本期不实现「自动换 host 执行」（列为后续增强，见 §7）。

---

## 4. 方案总览

两层：

```
命令执行
  └─ sandbox 执行层（docker.go / botcontainer.go / local.go）
       ├─ 正常退出 + 无异常特征        → Reliable = true
       └─ 检测到 OOM / 137 / Killed / 截断
            └─ 置 Reliable = false + 填充 Warnings[]
                 └─ sandbox/tools.go 将 reliable/warnings 写入返回 map
                      └─ agent 门禁（llmroute.go）
                           ├─ 轻量版：结果前置 ⚠️ 警告 + 工具描述约束 LLM
                           └─ 强版：验证型工具不可信 → 自动重试 / 强制声明不完整
```

---

## 5. 层 1：sandbox 执行层检测 + 发信号

### 5.1 数据结构变更

底层 `ExecResult`（`sandbox/sandbox.go:105` 附近）新增字段（`WsExecResult` 已随 `tools/shell.go` 一并删除，不再有双结构）：

```go
type ExecResult struct {
    ExitCode  int
    Stdout    string
    Stderr    string
    Truncated bool      // 仅表示「输出超长被截断，执行已完成」
    // —— 新增 ——
    Reliable  bool      // 命令是否正常且完整地执行（默认 true，检测命中则 false）
    Aborted   bool      // 命令是否中途失败（OOM / 信号杀 / 超时）
    OOMKilled bool      // 执行期间 cgroup oom_kill 计数是否增加
    Warnings  []string  // 人类可读的不可信原因，回传给 LLM
}
```

约定：`Reliable = !Aborted && !OOMKilled && !(异常文本命中)`。`Truncated` 单独保留（执行成功但输出过长，不算不可信）。

### 5.2 检测逻辑（在每个 exec 收尾处加，docker.go / botcontainer.go / local.go 三处统一）

```
① cgroup OOM 对比（最可靠）
   - 执行前读 oom_kill 计数 snap0（v2: /sys/fs/cgroup/memory.events；v1: memory.oom_control）
   - 执行后读 snap1
   - if snap1 > snap0 → OOMKilled = true; Aborted = true
                       Warnings = append(..., "进程可能被 OOM 杀死（cgroup oom_kill 增加），结果不完整")

② 退出信号
   - ExitCode == 137（或被信号杀死，ExitCode < 0 且含 signal）且非超时
     → Aborted = true; Warnings = append(..., "命令被信号杀死(exit=137)，结果可能不完整")

③ 输出文本特征扫描（兜底，覆盖无 cgroup 读权限的情况）
   - 对 stdout+stderr 做不区分大小写匹配：
     "killed" / "out of memory" / "signal: killed" / "cannot allocate memory"
     / "fatal error: runtime: out of memory" / "signal: terminated"
   - 命中 → Aborted = true; Warnings = append(..., "输出中出现 OOM/中止特征: <匹配片段>")

④ 超时
   - 已有 ctx.DeadlineExceeded → ExitCode = -1（docker.go:405）
     补充：Aborted = true; Warnings = append(..., "命令超时未跑完，结果不完整")

⑤ 汇总
   - Reliable = !(Aborted || OOMKilled || 异常文本命中)
   - 若 !Reliable：Warnings 至少一条
```

注意：cgroup 读取需在**沙箱容器内**进行（命令就在容器里跑），读取自身 cgroup 文件即可，无需特权。

### 5.3 sandbox/tools.go 返回 map 增加字段

`sandbox/tools.go` 的 `execResultToToolOutput` 改为同时返回：
```go
return map[string]any{
    "exitCode":  res.ExitCode,
    "stdout":    res.Stdout,
    "stderr":    res.Stderr,
    "truncated": res.Truncated,
    "reliable":  res.Reliable,    // 新增
    "aborted":   res.Aborted,     // 新增
    "oomKilled": res.OOMKilled,   // 新增
    "warnings":  res.Warnings,    // 新增
    "workdir":   ws.WorkDir(),
}, nil
```

并强化 `Description`（`sandbox/tools.go` 的 `buildExecTool`）：
> 增加：「若返回中 `reliable` 为 false，说明命令未完整/可信地执行（可能因 OOM、超时或被杀死），**请勿将其当作完整结果使用**，应换更高资源的方式重试或询问用户。」

---

## 6. 层 2：agent 机制门禁（关键）

拦截点：轻量版在**工具输出层**实现——`sandbox/tools.go` 的 `execResultToToolOutput` 在 `reliable:false` 时直接把 ⚠️ 警告前置到 `stdout`（LLM 无论如何都能看到）；强版在**工具执行层**实现——`sandbox/tools.go` 的 `Execute` 对验证型命令 OOM 时调用 `RetryOOMWithElevatedMemory`。`llmroute.go` 未改动，避免对全部工具的广泛耦合（核心诉求已由工具层覆盖）。

### 6.1 轻量版（必做，成本低）
- 当工具结果含 `reliable:false`（或 `aborted:true` / `oomKilled:true`）时，在回传给 LLM 的工具结果内容**前置一行显著警告**：
  ```
  ⚠️ 工具结果不完整/不可信：<Warnings 拼接>。请勿当作完整结果使用；如需完整结果，请提高资源(如增大沙箱内存)或更换执行方式后重试。
  ```
- 配合 §5.3 的 `Description` 指令约束，覆盖「LLM 盲信半份结果」的大部分场景。

### 6.2 强版（验证型工具，推荐做）
对**验证型命令**（`golangci-lint` / `go test` / `go build` / `go vet` / `grep -c` / `wc -l` 等可识别模式）做机制兜底：
- 检测到 `reliable:false` → **自动重试一次**：
  1. 先重写命令追加真实退出码探针：`... ; echo "RC=$?"`（对抗 `| head` 之类管道掩盖）；
  2. 若仍不可信，尝试**加大资源**后重试：通过 `WorkspaceManager` 临时提升该 bot 沙箱内存上限（如 2G→6G）后重跑；或（后续增强）走 host 执行路径。
- 重试 N 次（建议 N=2）仍不可信 → **强制 LLM 显式声明「无法获得完整结果」**，而不是直接拿半份去改代码：在工具结果里追加
  ```
  [可靠性门禁] 已尝试 N 次仍无法获得完整结果，请向用户说明并请求更多资源/权限，不要基于当前不完整输出下结论。
  ```

### 6.3 验证型工具识别
在 shell 工具或门禁层维护一个轻量启发式：命令首词/参数匹配 `golangci-lint|go test|go build|go vet|pytest|npm test|make test|grep -c|wc -l` 等即视为验证型。也可在 tool `Description` 之外单独标注。

---

## 7. 后续增强（本期不做）
- **走 host 执行路径**：当沙箱内存不足且验证型工具必须跑完时，提供「在物理机（bot 所在）临时执行」的受控通道（需审计/隔离，避免沙箱逃逸）。
- **资源自适应**：门禁根据历史 OOM 自动建议/设置合理内存上限，而非固定 2G。
- **输出完整性哈希**：长输出分块时带 chunk 序号+总数，缺失即知不完整（比 `Truncated` 更精确）。

---

## 8. 改动清单（文件 + 内容，已落地）

| 文件 | 改动 |
|------|------|
| `sandbox/sandbox.go` | `ExecResult` 增加 `Reliable/Aborted/OOMKilled/Warnings` 字段；新增 `StreamWorkspace` 可选接口（含 `ExecStream`） |
| `sandbox/reliability.go` | **新增**：完整性检测核心 `finalizeExecResult` / `scanFatalText`（尾部 4KB） / `parseOOMKill`（v2+v1） / `readCgroupOOMKill`（host） / `readContainerCgroupOOMKill`（容器内）；常量 `oomRetryElevatedMB=6144` |
| `sandbox/docker.go` | `runCommandWithStreaming` 签名加 `stdin []byte`，收尾统一调用 `finalizeExecResult`；`dockerWorkspace.ExecStream` 命令前后 cgroup 对比 |
| `sandbox/botcontainer.go` | `execInContainer` 复用统一收尾；`botContainer.ExecStream` 命令前后读容器内 cgroup `oom_kill` 计数，增加则置 `OOMKilled/Aborted/Warnings` |
| `sandbox/local.go` | `localWorkspace.ExecStream` 命令前后读宿主 cgroup 对比（local 无容器隔离） |
| `sandbox/botworkspace.go` | 新增 `RetryOOMWithElevatedMemory`：验证型命令 OOM 时临时升 6G 内存重建容器重试一次（不落库，仅一次） |
| `sandbox/tools.go` | `buildExecTool`(`sandbox_exec`) 强化 `Description`；`Execute` 支持流式 + OOM 自动重试；`execResultToToolOutput` 注入 reliable/aborted/oomKilled/warnings，不可信时前置 ⚠️ 警告到 stdout；`isVerificationCommand` / `verificationCommandMarkers` |
| `sandbox/reliability_test.go` | **新增**单测：`finalizeExecResult` / `scanFatalText` / `parseOOMKill` / `isVerificationCommand` / `execResultToToolOutput`（覆盖管道掩盖、Truncated 仍可信等场景） |
| `tools/shell.go` + `tools/shell_test.go` | **删除**：错误重复实现，与 `sandbox/tools.go` 路由同一后端产生双实现漂移 |
| `tools/tools.go` | `Config.WorkspaceResolver` 与对应注册分支删除（bot 工作空间工具统一由 sandbox 包 `RegisterBotWorkspaceTools` 注册） |
| `tools/datetime.go` | 删除静态 `list_files` 工具（已由 `sandbox_list_dir` 覆盖） |
| `tools/tools_test.go` | staticCount 期望 `12 → 11`（`list_files` 移除） |
| `api/botservice.go` | 删除 `workspaceExecAdapter` 结构体及 `WorkspaceResolver` 闭包 |

> 注：原文档担心的双 shell 实现问题已彻底解决——`tools/shell.go`（`WorkspaceExecutor` 路径）经排查为**错误实现**，agent 实际走 `sandbox/tools.go`（`sandbox_exec` 等，经 `bot.go → sandbox.RegisterBotWorkspaceTools` 注册，全路由到 `BotWorkspace.Exec`）。双实现已删除，代码层无 `WorkspaceResolver`/`WorkspaceExecutor`/`shell` 工具残留引用。

---

## 9. 测试计划
- 单测：在 cgroup 可读环境下构造 OOM（如 `stress-ng --vm` 触发），断言 `OOMKilled=true`、`Reliable=false`、`Warnings` 非空。
- 单测：管道掩盖场景 `cmd that gets SIGKILL | head` → 即便 exit=0，因 cgroup oom_kill 增加仍判定不可信。
- 单测：正常 `echo hi` → `Reliable=true`，无 Warnings。
- 集成：在 2G 容器跑 `golangci-lint run ./...` → 门禁识别不可信、自动重试/声明不完整。
- 回归：确保 `Truncated`（输出超长但执行成功）仍 `Reliable=true`。

---

## 10. 风险 / 权衡
- cgroup 路径在个别环境（无 cgroup v2 / 特权受限）读不到 → 退化为「文本扫描 + 退出码」检测，仍可用。
- 自动重试可能放大资源消耗 → 限制重试次数（N=2）与仅限验证型工具。
- `reliable` 误判（把正常结果标不可信）→ 仅当明确信号命中才置 false，正常路径保持 true，误判率低。
- 双 shell 实现风险（`tools/shell.go` vs `sandbox/tools.go`）→ **已实现时排查确认 `tools/shell.go` 为错误实现并删除**，现仅 `sandbox/tools.go` 单一事实来源，无漂移。

---

## 11. 开放问题（评审时确认）

1. **【已解答】** 当前 agent 实际使用的 shell 工具实现是 `tools/shell.go` 还是 `sandbox/tools.go`？
   → 经排查确认走 `sandbox/tools.go`（`sandbox_exec` 等，经 `bot.go → sandbox.RegisterBotWorkspaceTools` 注册）。`tools/shell.go` 为错误实现，已删除。`WorkspaceExecutor`/`WorkspaceResolver` 路径一并清除。
2. **【已解答】** 自动重试「加大沙箱内存」是否要落库？
   → 不落库。仅临时提升容器内内存上限（`oomRetryElevatedMB=6144`，6GB），重建容器重试一次；bot 重启后恢复默认，避免「无限内存」。
3. 强版门禁的「验证型工具」清单是否够用，是否需要用户可配置？
   → 当前内置 `verificationCommandMarkers`（golangci-lint / go test / go build / go vet / go run / pytest / npm test / npm run build / yarn test / yarn build / make test / make build / cargo test / cargo build / tox / mvn test / gradle test / grep -c / wc -l 等）。后续如需用户可配置，可作为增强项。
