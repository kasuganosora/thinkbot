# gh-aw vs thinkbot 对照分析

> 对照仓库：`github/gh-aw`（GitHub Agentic Workflows，Go），本地副本 `/tmp/gh-aw`（临时，可删）
> thinkbot 侧：`workflow/`、`subagent/`、`agent/`、`llm/`
> **复核状态**：已二次回源核对，含对首次结论的自我推翻。见文末「复核记录」。

---

## 0. 核心结论（含对首版结论的推翻）

**首版最大的错误对照**：我把「gh-aw 的静态 job 图」当成「编排模型」去对比 thinkbot 的动态 DAG，得出"thinkbot 编排更强、gh-aw 胜在工程纪律"。**这个对照本身是错的。**

gh-aw 的 job 图**根本不是编排器，它是权限与凭据的隔离边界**（`jobs.md:42-56`：agent / detection job 强制只读，所有写操作下沉到独立的 safe_outputs job）。它的作用是让"agent 永远拿不到写权限"成为**可证明的安全属性**，而不是用来做任务分解。

真正的任务分解，gh-aw **也放在 LLM 运行时**——`daily-arxiv-researcher.md:171` 那句 "For each paper in `papers`, invoke the `paper-screener` sub-agent"，N 由运行时抓到多少篇论文决定。这和 thinkbot 的 SubAgent 并行是**同一层**的东西。

所以正确的对照是：

| | 动态调度器是谁 | 有无并发控制 | 有无节点级重试/自愈 | 有无崩溃恢复 |
|---|---|---|---|---|
| gh-aw | **引擎 CLI**（黑盒，Copilot/Claude 自己的子智能体机制） | 无 | 无 | 无（job 超时即死） |
| thinkbot | **自己实现的 Scheduler** | 有（信号量，默认 3） | 有（review 循环 + LLM 自愈替换子图） | 有（节点级） |

**结论**：thinkbot 的显式 DAG 调度明显强于 gh-aw 交给引擎 CLI 的隐式调度。但 gh-aw 那套**权限分层**是 thinkbot 缺的——目前 thinkbot 的 SubAgent 无工具调用（纯推理）恰好绕过了这个问题，**一旦将来给 SubAgent 加工具，N 个并行 SubAgent 各自持写权限就是重大风险**，届时 gh-aw 的"agent 只读 + 写操作走类型化出口"是唯一正确答案。

---

## 1. 官方特性全景：哪些是真特性，哪些是 playbook

README 只是入口。真正的 reference corpus 在 `.github/aw/`（近 12000 行，78 个 md）。**里面有 4 份是设计稿，不是特性**——没有 schema、没有编译器代码、没有运行时。

| 文档 | 行数 | 性质 | 判定证据 |
|---|---|---|---|
| `safe-outputs-*.md` | 4 份，1200 行 | **真特性** | `safe_output_handlers.go:23` 注册表，~50 种输出类型 |
| `memory.md` | 333 | **真特性** | `cache_memory.go`、`repo_memory.go`、`setup_cache_memory_git.sh` |
| `experiments.md` | 331 | **真特性**（统计检验是真实现） | `experiments_grader_statistics.go:163/196` 有 welchTTest + mannWhitney |
| `evals.md` | 143 | **真特性** | `evals_job.go:24` + `run_evals.cjs` |
| `subagents.md` | 204 | **半特性**（语法有，调度外包给引擎 CLI） | `sub_agent_extractor.go:182`，但文件是运行时落盘给引擎发现的 |
| `jobs.md` | 96 | **真特性，但非编排** | `compiler_custom_jobs.go:24-58`，只能挂预处理步骤 |
| `reuse.md` / `imports` | 296 | **真特性** | `import_bfs.go:29-45`，编译期 BFS 展开 + 环检测 |
| `context.md`（`{{#if}}`） | 178 | **真特性** | `render_template.cjs:17-18`，纯正则无 AST |
| `token-optimization.md` | 384 | **真特性** | API 代理 + `token-usage.jsonl` |
| **`loop.md`** | 144 | **playbook** | schema 无 `loop` 字段（grep 空），无编译器代码 |
| **`campaign.md`** | 133 | **playbook** | 无新增 frontmatter key，KPI 只是约定写 `kpi.json` |
| **`multi-agent-research.md`** | 284 | **playbook** | 纯 prompt 方法论，**0 个**真实 workflow 实现 |
| **`intent.md`** | 84 | **近乎死代码** | 297 个 workflow 中 **0 个**使用 `intent:` |

**教训**：一个项目的文档体量不等于能力体量。gh-aw 用 560 行文档描述的东西（loop + campaign + multi-agent-research），在 thinkbot 里是引擎代码级的不变量。

---

## 2. 逐特性对照

### A 类：gh-aw 独有，值得抄

**A1. memory 的 integrity 分级**（最值得抄的安全创新）

`actions/setup/sh/setup_cache_memory_git.sh:258-283`：cache 目录内建 git repo，四个 integrity 分支 `merged / approved / unapproved / none`，**merge-down 语义**——低完整性 run 能读到高完整性数据，**高完整性 run 永远看不到低完整性数据**（`-X theirs`）。

价值：从机制上阻断"被污染的 run 反向污染高信任记忆"这条提示注入链路。thinkbot **完全没有这层**——记忆就是记忆，不区分来源信任度。

配套还有：改 guard 策略即 cache miss（`cache_integrity.go:29-40`，policyHash 进 cache key）、agent 前清洗（删 hooks / 删 symlink / 去执行位 / 按扩展名删文件，sh:179-341）。

**A2. safe-outputs 权限推导**
`safe_output_handlers.go:23` 注册表，每种输出类型自带 `PermissionBuilder`，最终权限 = 已启用类型的并集（`safe_outputs_permissions.go:112`）。**写权限不是用户写的，是从"你声明要做什么"反推的**。`staged: true` 让构造器返回 nil，完全不申请写权限。

**A3. 确定性 graders**
`graders_config.go:36-45`，8 个指标。只有前四个带 `Threshold`：

| 指标 | 方向 / 阈值 | 说明 |
|---|---|---|
| `tool-success-rate` | higher / **0.8** | 工具调用成功率 |
| `tool-failure-count` | lower / **5** | |
| `retries` | lower / **10** | 网关日志中的重试事件数 |
| `loops` | lower / **3** | 连续相同工具调用（名称+参数都相同） |
| `working-set-rebuild-factor` | lower / 无阈值（Min 1.0） | 累计 input token ÷ 峰值 |
| `trajectory-efficiency` | higher / 无阈值 | 不同工具名数 ÷ 总调用数（多样性，非"路径效率"） |
| `context-growth` | lower / 无阈值 | 总 token ÷ 首轮 token |
| `artifact-production` | higher / 无阈值 | 产出物数量 |

别把 `Min: 1.0` 误读成阈值——它是理论下界。

**A4. 成本统一计量**
在 LLM 与 agent 之间放 API 代理，吐 `api-proxy-logs/token-usage.jsonl`（`pkg/cli/token_usage_types.go:145`）。不论底层引擎是谁，计数口径统一，最后折算成单一可比单位 AIC（`model_costs.go:144` `usdToAIC`，1 AIC = $0.01）。

**A5. 结构化上报 + 溯源坐标**
`logs_models.go:123-165`：`MissingToolReport`(132) / `NoopReport`(140) / `MissingDataReport`(146) / `MCPFailureReport`(155)，每条带 `ReportProvenance`(123)（timestamp / workflow / run / experiment / variant）。**"agent 卡住了"变成可查询事件，而不是翻日志。**

### B 类：双方都有，思路对比

**B1. Memory —— 互补，不是二选一**

- **gh-aw 强在存储安全**：Git 后端、可审计、diff 可见、OCC 并发控制 + 服务端签名提交（`push_repo_memory.cjs:637-699`，失败重试 3 次，每轮 `ls-remote` 刷新 baseRef）、integrity 分级、配额清洗（patch 净增量上限 10KB、扩展名白名单、去执行位）。
- **gh-aw 弱在语义层**：记忆就是"一个文件夹，LLM 自己读写"——无召回机制（谁去读？靠 LLM 想起来）、无压缩整理（超限就**整个 run 失败**，`push_repo_memory.cjs:594-598`）、无画像。
- **thinkbot 强在语义层**：`agent/memory/` 的 dreaming / tiered_recall / consolidator / profiler / compressor 恰好补这一层。
- **正确形态**：thinkbot 的 tiered_recall + consolidator 跑在 gh-aw 的 Git-backed + integrity-branching 存储之上。gh-aw 给了"怎么安全地存"，thinkbot 给了"存什么、怎么想起、怎么忘"。

**B2. LLM 裁判 —— thinkbot 有裁判但零聚合**

这是最意外的发现：**thinkbot 有 `agent/engagement/judge.go`（8805 字节），而且比 gh-aw 更细**——支持 YES/NO 与 **0-100 评分**双模式（`BuildScoredJudgePrompt:99`，引用 Houde et al. 2025）。

但它被用作**运行时闸门**（`engagement.go:227-256` 决定参与/拒绝），**判定结果没有落库**——`stats/` 数据模型只有 token/tool/steps 维度，无任何 judge 分数列。**有裁判，零聚合，每次判定用完即弃。**

gh-aw 的 evals 相反：编译成独立 job，结果写 `evals.jsonl` 提交到 `evals/<id>` 分支持久化，CLI 聚合为 YES/NO/UNKNOWN 计数。**gh-aw 赢在工程闭环。**

**B3. 复用机制 —— 对象不同**

- **gh-aw `imports` 复用「配置」**：白名单字段深合并（`reuse.md:11-24`），带类型化参数（`import-schema`，`:52-88`，支持 string/number/boolean/choice/array/object），编译期静态展开。版本治理完善（`source:` / `redirect:` / `gh aw add|update`）。本质是 **YAML mixin**。
- **thinkbot `skill/` 复用「能力」**：`SKILL.md` 的 front matter 是元数据，正文才是领域知识，由 loader 运行时按需选取。本质是**可寻址的知识单元**。
- 有意思的是 gh-aw 自己也做了 skill（`interpolate_prompt.cjs:164-181` 的 `## skill:`，与 subagent 完全平行的运行时落盘机制）——它正在往 thinkbot 的方向补。
- **值得抄**：`import-schema` 的类型化参数 + 加载期校验。SKILL.md 若能声明 inputs 类型，可把"拼装错误"从运行时提前到加载期。

### C 类：gh-aw 有，但对 thinkbot 是过度工程

**experiments（A/B + 统计检验）**

真实现，不是配置项：`mann_whitney` 含并列秩修正 + 正态近似（`experiments_grader_statistics.go:196`）、Welch t 检验（`:163`）、两比例检验、Beta-Binomial 贝叶斯 P(优于)（4096 点数值积分）、卡方平衡检验、guardrail 判定（`:316`/`:376`）。

**但不要做。** 全部价值建立在"每天几十次同源重复 run"上：gh-aw 有 297 个定时 workflow，`min_samples` 默认 20/变体，2 变体即 40 次 run 才出一个结论。thinkbot 是交互式会话系统，40 次 run 可能要几周，届时 prompt 早已变了。**在这里做统计检验不是严谨，是用小样本制造虚假确定性。**

**更值得的替代**：thinkbot 的真实痛点不是"哪个变体更好"，而是"**改 prompt 后有没有退化**"。**黄金集回归**（固定 20-30 个 case，改 prompt 时全量重放比通过率）在小样本下信号远强于 A/B——不要求线上流量，一次跑完就有结论。

### D 类：纸面特性（已验证无实现）

`loop.md` / `campaign.md` / `multi-agent-research.md` / `intent.md`。判定证据见第 1 节表格。

其中 `multi-agent-research.md` 尤其值得注意：它描述的多智能体研究（fan-out、去重、对抗审计）**0 个真实 workflow 实现**，且硬性禁止级联（`multi-agent-research.md:170`、`subagents.md:159`）——因为 gh-aw 没有编排器能保证收敛，只能用 prompt 纪律代偿。而 thinkbot 的 LLM 自愈（失败节点运行时替换为子图）正是这条禁令要防的东西，**但 thinkbot 有真正的调度器，风险可控**。

### E 类：thinkbot 独有，gh-aw 结构上拿不到

| 能力 | 为什么 gh-aw 拿不到 |
|---|---|
| 节点级崩溃恢复 | 单个 `agent` job 是一次性进程，超时即死无检查点。`multi-agent-research.md:179` 只能靠 cache-memory 在下一次 run 恢复，粒度是天 |
| 运行时子图替换 | 硬性禁止递归 fan-out |
| 节点级失败传播 | 子智能体失败不传播——它只是父模型上下文里的一段文本 |
| 配额熔断 | 只有 `max-ai-credits` 这种预算上限，无熔断-恢复循环 |
| 目标模式闭环 | 无回退边概念，重试靠 prompt 约定 |

---

## 3. fan-out 能力实测（推翻一个常见误解）

**gh-aw 做不到「一个任务动态拆成 N 个并行 job」。** 三条路径全部证伪：

| 路径 | N 的性质 | 证据 |
|---|---|---|
| `jobs.<id>.strategy.matrix` | 编译期常量，且**实际无人使用** | 全仓 `.lock.yml` 里出现的 7 处 `strategy:` **全是 `sub_agent_strategy:` 这类实验变量名**，无一是 `strategy.matrix` |
| `safe-outputs.call-workflow` | N 个 job 编译期生成，运行时只跑 1 个 | `compiler_safe_output_jobs.go:197-280`，`if: needs.safe_outputs.outputs.call_workflow_name == '<name>'` 是运行时 switch。默认 `max: 1`，硬顶 50 |
| `safe-outputs.dispatch-workflow` | **唯一真·运行时 N**，但脱离本 run | 走 Actions API 触发独立 run，父 run **不 join、不聚合、失败不传播** |

真正的 fan-out 在 LLM 层（引擎 CLI 并发执行子智能体），与 thinkbot 的 SubAgent 并行同层，但 gh-aw 那边是黑盒——无并发控制、无重试、无级联跳过、无崩溃恢复。

---

## 4. 落地建议（按性价比）

| 优先级 | 事项 | 涉及文件 | 工作量 |
|---|---|---|---|
| P0 | **目标模式反馈回注过 PromptScan** | `workflow/scheduler.go:753`、`agent/prompt/prompt_scan.go` | 小 |
| P0 | **judge 结果落库**（裁判已存在，只差持久化） | `agent/engagement/judge.go` → `stats/recorder.go` | 小 |
| P1 | 成本/token 统一计量 | `llm/` 调用层 + `workflow/` 归因 | 中 |
| P1 | 确定性 graders（loops / retry / tool-success） | 新 `workflow/graders.go` | 小 |
| P1 | 节点结构化上报（noop / missing_tool） | `workflow/types.go:54` 加字段 | 小 |
| P1 | 节点能力声明 + 调度前校验 | `workflow/types.go`、`manager.go` | 中 |
| P2 | **黄金集回归**（替代 A/B 实验） | 新测试设施 | 中 |
| P2 | 补 analyzer **分解质量** fixture 测试（当前零覆盖） | `workflow/analyzer_test.go` | 中 |
| P2 | 高价值写操作拆 apply 节点 | `workflow/dag.go`、`executor.go` | 中 |
| P3 | memory 来源信任度分级 | `agent/memory/` | 大 |
| — | ~~A/B 实验框架~~ | — | **不做** |

**关于 P0 第一项**：`scheduler.go:780` 直接 `fn.LoopFeedback = feedback`，无任何净化。而 `agent/prompt/prompt_scan.go` 就在项目里（检测 "ignore previous instructions"、角色劫持、零宽字符、数据渗出），但 `workflow/` 非测试代码**未 import `agent/prompt`**——该包只出现在 `review_verdict_test.go:10` 且仅用 `prompt.NewRegistry()` 构造 ToolManager。

---

## 5. 复核记录

### 首版 4 处硬错误（已修正）

1. `pkg/workflow/token_usage_types.go:146` → 包路径不存在，实际 **`pkg/cli/token_usage_types.go:145`**。
2. CTR 规则数 26 → **25**。
3. `working-set-rebuild-factor` 阈值 "≤1.0" → **无 Threshold**，`Min: 1.0` 是理论下界。
4. `ci.yml:538-560` → job 在 **539**，脚本在 **566**。

### 首版 4 处论断收紧（已修正）

5. ~~"gh-aw DAG 编译期写死"~~ → 有 `dispatch-workflow` / `call-workflow` 做运行时跨 workflow 触发。
6. ~~"workflow 完全没引用 PromptScan"~~ → 精确说法：非测试代码未 import `agent/prompt`。
7. ~~"analyzer_test.go 是硬编码 case"~~ → 它是 5 个**行为测试**，对分解质量**零覆盖**。问题是"没网"不是"难维护"。
8. ~~"thinkbot 有 hitl.go 但没接到 workflow"~~ → `agent/stages/hitl.go` 是主链路**工具级审批**，非 workflow 审批门。

### 首版结论性错误（本次推翻）

9. **对照框架错了**：把 gh-aw 的 job 图当编排模型。它其实是**安全模型**（权限隔离边界）。正确对照是"引擎 CLI 隐式调度" vs "thinkbot 显式 DAG 调度"。
10. **误以为 thinkbot 无 LLM 裁判**：实际有 `agent/engagement/judge.go`，且支持 0-100 评分比 gh-aw 更细。真问题是**判定结果没落库**。

### 本次亲自核对无误的关键引用

`safe_output_handlers.go:23` / `safe_outputs_permissions.go:112` / `graders_config.go:36-45`（8 个指标逐个比对）/ `concurrency.go:195-230` / `logs_models.go:123-165` / `model_costs.go:16,144,182` / `sanitize_content.cjs:93-137` / schema 15262 行 / 297 个 workflow md / `pkg/linters/README.md`（确认 ~70 个 linter 检查 gh-aw 自身源码）/ **schema 无 `loop` 字段** / **`experiments_grader_statistics.go:163,196` 有 welchTTest + mannWhitney** / **`intent:` 使用数 0/297** / **7 处 `strategy:` 全是实验变量名、无 matrix** / **`agent/engagement/judge.go` 存在且有 0-100 评分**。

### 仍未逐条核行号（二手转述，取用前请自行确认）

编译流水线（`compile_orchestrator.go:20`、`compile_pipeline.go:48`、`compiler.go:114-127/147/162-165/286-300/376-397/438`）、默认值注入（`tools.go:22/76-79/188`）、agent job 只读校验（`compiler_main_job_helpers.go:355-385`）、引擎能力位图（`agentic_engine.go:113-159`）、golden 测试（`wasm_golden_test.go:73-100/163-234`）、memory 系列（`cache_memory.go`、`repo_memory.go`、`setup_cache_memory_git.sh`、`push_repo_memory.cjs` 的具体行号）、subagent 系列（`sub_agent_extractor.go:182`、`extract_inline_sub_agents.cjs`）、imports 系列（`import_bfs.go:29-45`）。**这些文件均确认存在**，只是行号未经回源验证。
