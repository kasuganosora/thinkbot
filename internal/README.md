# internal

本目录集中存放**不对外导出的内部支撑包**。借助 Go 的 `internal` 机制，只有
`github.com/kasuganosora/thinkbot` 模块内的代码可以 import 这里的包，外部仓库无法依赖。

包内可以有导出的类型与函数（如 `interaction.Registry`、`searchproviders.Search`）——
"导出"只是为了让 `api/`、`tools/`、`channel/` 等兄弟目录使用，**不构成对外的 API 承诺**，
签名与行为可能随内部重构随时调整。

## 子包一览

| 子包 | 职责 | 主要使用方 |
| --- | --- | --- |
| `buildinfo` | 暴露运行二进制的构建信息（版本 / git / 构建时间） | `api`（/health、系统信息）、`singleinst` |
| `interaction` | 进程内「提问—等待应答」注册表，跨平台 `user_choice` 的核心枢纽 | `tools/user_choice`、`api`、`channel/{telegram,misskey}` |
| `searchproviders` | 搜索提供方配置存储 + 12 家真实搜索后端 + 串行回退与熔断 | `api/handler_search_provider`、`tools/web_search` |
| `singleinst` | 进程启动早期的单实例版本协商 | `cmd/main.go`（fx Module） |

---

## buildinfo

返回当前运行二进制的构建信息，供健康探针回报——在不登录机器、不 dump 二进制的情况下
确认「运行中的实例是不是包含某次修复的那版」。

**信息来源（按优先级）**：

1. 构建时 `-ldflags "-X ..."` 注入（最准，部署脚本应固化此步）；
2. 未注入时退回二进制文件 mtime（仍是真实编译时间）；
3. git revision 仍未取到时尝试 `git rev-parse HEAD`（仅仓库内运行有意义）。

**关键导出符号**：

- `Get() Info`：返回构建信息快照；解析与缓存只在首次调用（实际是 `init`）时发生一次。
- `Info`：`Version` / `GitRevision` / `GitShort` / `BuildTime`（RFC3339）/ `BuildTimeUnix` /
  `Source`（`ldflags` | `binary-mtime` | `unknown`）/ `GoVersion`。
- `GitRevision` / `BuildTime` / `Version`：包级变量，构建时注入点。

```bash
go build \
  -ldflags "-s -w \
    -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=$(git rev-parse HEAD) \
    -X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
    -X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=$(git describe --tags --always)" \
  -o thinkbot ./cmd
```

注意：包内 `init()` 在进程加载阶段即固定构建信息，而不是等首次 `/health` 才 lazy 解析。
原因是 `BuildTimeUnix` 被用作单实例协商的「谁更新」判据——若部署用「原地覆盖」替换二进制，
旧进程延迟解析会读到新二进制的 mtime，导致版本失真、协商误判。

---

## interaction

提供「向用户提问—等待用户应答」的进程内交互原语：

- 工具侧（`user_choice`）注册一个 `Question` 并用 `Wait` 阻塞等待；
- 各平台 channel（web / telegram / misskey）收到用户输入后，用统一的 `Answer` 调
  `Resolve` 回填并唤醒等待者；
- 两端只通过 questionID 解耦，不侵入彼此的消息主链路。

**状态机**（终态不可逆，再变更返回 `ErrAlreadyResolved`）：

```
pending --Resolve-->   answered
pending --deadline-->  timeout
pending --Cancel---->  cancelled
```

**关键导出符号**：

- `Default() *Registry`：进程级单例注册表，工具与 channel 都通过它交互；
  需要隔离时可用 `NewRegistry()`。
- `Registry` 方法：
  - `RegisterQuestion(q Question) (Question, error)`：注册并返回规范化快照（补齐选项 ID、
    缺省 `TimeoutSecs`、置 `StatusPending`）；
  - `Wait(ctx, questionID)`：阻塞到终态，返回终态快照与应答；`ctx` 取消或到达问题超时上限
    都会把问题落到对应终态（timeout / cancelled），不留悬挂的 pending 条目；
  - `Resolve(questionID, ans)` / `ResolveFrom(questionID, chatID, ans)`：回填应答。
    **对外接口必须用 `ResolveFrom` 并传 chatID**，否则任何登录态请求都能替别的会话作答；
  - `IndicesForOptionIDs(questionID, ids)`：把渲染层回填的选项 ID 翻译成内部下标，
    未知 ID 直接报错而非静默丢弃；
  - `Cancel` / `CleanupFinal` / `AbortPending` / `PendingCount`：取消；移除终态条目
    （pending 不可直接删，否则 `Wait` 永远阻塞在无人关闭的 channel 上）；
    「先取消再清理」（投票创建失败时用）；当前 pending 数（测试与监控）。
- 类型与常量：`Question`、`Option`、`Answer`、`Status` / `Mode`（single/multi）/ `Via`
  及其常量；`MinOptions=1`、`MaxOptions=8`（三平台交集约束）、`DefaultTimeoutSecs=600`。
- 错误哨兵：`ErrQuestionNotFound`、`ErrDuplicateID`、`ErrInvalid*`、`ErrAlreadyResolved`、
  `ErrTimeout`、`ErrCancelled`——一律用 `errors.Is` 判断。
- `RegisterPollCreator(platform, fn)` / `GetPollCreator(platform)` / `PollCreator`：
  各 channel 启动时注册平台原生投票创建函数（同 platform 后注册胜出），让非 web
  平台也能创建原生投票帖；签名与 Misskey 的 `CreatePollNote` 一致。
- `Lookup(questionID)`：返回问题只读快照（telegram 侧 `PollCreator` 由此取目标会话）。

工具侧典型用法：

```go
reg := interaction.Default()
snap, err := reg.RegisterQuestion(interaction.Question{
    ID: questionID, BotID: botID, ChatID: chatID,
    Question: "选一个方案", Mode: interaction.ModeSingle,
    Options: []interaction.Option{{Label: "A"}, {Label: "B"}},
    TimeoutSecs: 300,
})
// ……把 snap 渲染给用户（web 卡片 / telegram 按钮 / misskey 投票）……
q, ans, err := reg.Wait(ctx, questionID) // ans.Via / ans.Selected / ans.CustomInput
reg.CleanupFinal(questionID)
```

channel 侧回填：

```go
idx, err := reg.IndicesForOptionIDs(questionID, []string{"o1"})
if err != nil { /* 未知选项 ID，向用户报错 */ }
err = reg.ResolveFrom(questionID, chatID, interaction.Answer{
    Selected: idx, Via: interaction.ViaTelegram,
})
```

---

## searchproviders

管理 Web UI 中「Settings → Search Providers」配置的搜索提供方，并执行真实网页搜索。
文件大致分工：`types.go`（类型与元数据）、`store.go`（providers.json 读写）、`search.go`
（入口与调度）、`circuit.go`（进程内熔断）、`backends.go`（各家 API 适配）、
`duckduckgo.go` / `yandex.go`（免密钥的 HTML/XML 抓取解析）。

**关键导出符号**：

- `Store` / `NewStore(path)` / `DefaultStore()` / `DefaultFile`
  （`data/search/providers.json`）：API 与 `web_search` 工具共用同一份文件，工具每次调用
  都能看到 UI 刚改过的配置；`List` / `Save` / `EnabledList`（顺序与文件一致，UI 顺序即
  优先级）/ `Enabled`（取第一个启用项，无启用项返回明确错误）。
- `Search(ctx, p Provider, query, count)`：用指定提供方执行一次搜索。
  **空结果、鉴权失败、超时一律返回 error，绝不伪造命中**（例如拿搜索引擎首页链接冒充结果）。
- `SearchEnabled(ctx, store, query, count) (*EnabledResult, error)`：按文件顺序（UI 顺序即
  优先级）串行尝试所有启用项，失败立即回退到下一个；`EnabledResult` 携带命中的
  `Results` / `Provider` / `Fallback` 标记 / `Attempted` 回退路径（供 LLM 观测）。
- `Provider` / `Result` / `Attempt` / `EnabledResult`：数据结构。
- `TypeBrave` / `TypeBing` / `TypeGoogle` / `TypeTavily` / `TypeSogou` / `TypeSerper` /
  `TypeSearXNG` / `TypeJina` / `TypeExa` / `TypeBocha` / `TypeDuckDuckGo` / `TypeYandex`：
  与 Web UI 下拉框一致的 12 种类型；`TypeMeta(t)` 返回展示名/字母/颜色（未知类型给占位），
  `KnownType(t)` 判定合法性。
- `OverrideDDGEndpoints(htmlURL, liteURL)`：单测覆盖 DDG 端点，返回恢复函数；
  `InstantAnswerHost`（`api.duckduckgo.com`）是严禁调用的旧 Instant Answer API host，
  重定向拦截与测试断言都用它做判据。

**回退与熔断要点**：

- 串行尝试、不并行，以免打爆配额；超时/5xx/网络错误立即回退、不加长熔断；
- 401/403 熔断 10 分钟，429 按 `Retry-After`（缺省 60 秒）；`Store.Save` 成功后清空熔断
  （UI 更新了 key 就该立刻重试）；熔断按 provider ID 记忆，进程内有效；
- 所有启用项都失败后，再用免密钥的 DuckDuckGo HTML 端点兜底试一次
  （启用列表里已有 duckduckgo 类型时不重复）。

```go
store := searchproviders.DefaultStore()
outcome, err := searchproviders.SearchEnabled(ctx, store, query, 5)
if err != nil { /* err 中已串联每个提供方的失败原因 */ }
engine := outcome.Provider.Type // 回报实际命中的引擎
```

集成测试需要真实凭据（目前仅覆盖 Brave，读 `THINKBOT_TEST_BRAVE_API_KEY`），见 `.env.test.example`：

```bash
cp internal/searchproviders/.env.test.example internal/searchproviders/.env.test
go test -v -run TestIntegration ./internal/searchproviders/ -timeout 60s
```

---

## singleinst

解决守护进程重启时「旧实例未死、新实例已起，两个 bot engine 同时消费消息导致重复回复」
的问题。在 HTTP server 监听前探测同端口的运行中实例，取其 `/health` 的
`buildTimeUnix`（版本号，复用 `buildinfo`）与 PID 后协商：

| 探测结果 | 动作 |
| --- | --- |
| 连不上 / 响应格式异常 / PID 是自己 | 正常启动（保守起见绝不误杀） |
| 对方版本 **更新或相同** | 本实例让位：调用 `selfExit`（`os.Exit(0)`），运行中实例继续服务 |
| 对方版本 **更旧** | 向对方发 `SIGTERM`，轮询等待端口释放（10 秒上限）后接管 |

**关键导出符号**：

- `Acquire(ctx, addr, logger, selfExit) error`：执行一次协商；返回 `nil` 表示应继续启动。
- `ErrYield`：让位路径的保险返回值（`selfExit` 通常已终止进程）。
- `Module`：fx 模块，在依赖构造阶段（`fx.Invoke`，而非并发执行的 `OnStart`）同步完成协商，
  保证在 bot engine 启动消费消息之前出结果。`cmd/main.go` 中必须注册在 `bot.Module` 之前；
  协商失败（如等待端口释放超时）会 `os.Exit(1)`——宁可启动失败，也不留下双实例。

地址取环境变量 `API_ADDR`，否则默认 `:8080`；探测统一走 `127.0.0.1` 回环。

---

## 维护约定

- 新增子包或调整包职责时，同步更新本 README；
- 各子包均带单元测试：`go test ./internal/...`；
- 修改 `buildinfo` 的注入变量名、`interaction` 的错误哨兵、`searchproviders` 的
  `Provider` JSON 字段时，注意同步部署脚本（`scripts/redeploy.sh`、`Dockerfile`）、
  Web UI 与既有调用方。
