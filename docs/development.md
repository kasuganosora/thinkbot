# 开发与裸机运行

> 相关文档：[架构](architecture.md) · [配置](configuration.md) · [部署与运维](deploy.md)

## 不使用 Docker（裸机运行）

```bash
cp .env.example .env
# 重新生成 swagger 文档（如需，见下文）后编译
go build -ldflags="-s -w" -o thinkbot ./cmd && ./thinkbot
```

> 提示：默认 `go build` 产物约 50MB；加上 `-ldflags="-s -w"` 可剥离调试符号与 DWARF 信息，体积显著减小（与 Docker 构建路径一致）。二进制需能访问宿主 `docker` 才能使用 `sandbox.backend=docker`，否则请将 `sandbox.backend` 改为 `local`（注意 `local` 没有容器隔离，命令直接跑在宿主上）。

> ⚠️ **不要设置 `CGO_ENABLED=0`**。数据库驱动 `gorm.io/driver/sqlite` 底层为 `mattn/go-sqlite3`（C 实现），`CGO_ENABLED=0` 下仍能编译通过，但驱动会被替换成 stub，运行期打开数据库时报 `go-sqlite3 requires cgo to work. This is a stub` 而无法启动。交叉编译时需配置对应平台的 C 工具链。

首位管理员同样需要 `AUTH_BOOTSTRAP_ADMIN` / `AUTH_BOOTSTRAP_PASSWORD`（或 `AUTH_BOOTSTRAP_TOKEN`）环境变量，见 [configuration.md › 首位管理员](configuration.md#首位管理员)：

```bash
AUTH_BOOTSTRAP_ADMIN=admin AUTH_BOOTSTRAP_PASSWORD=change-me-please ./thinkbot
```

注入版本信息（供 `/health` 展示，`scripts/redeploy.sh` 用的就是这种写法）：

```bash
go build -ldflags="-s -w \
  -X github.com/kasuganosora/thinkbot/internal/buildinfo.GitRevision=$(git rev-parse HEAD) \
  -X github.com/kasuganosora/thinkbot/internal/buildinfo.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ) \
  -X github.com/kasuganosora/thinkbot/internal/buildinfo.Version=$(git describe --tags --always 2>/dev/null || echo dev)" \
  -o thinkbot ./cmd
```

辅助脚本（`scripts/`）：`redeploy.sh` 重部署（停旧实例 → 构建 → 启动 → 健康检查；脚本内 `REPO` 路径为作者本机路径，使用前请修改）、`run_thinkbot.py` 守护启动、`watch.sh` 日志观察。

## 前端

源码在 `web/`（Vue 3 + Vite + TDesign），构建产物输出到项目根 `static/`，由后端托管（`static/` 勿手工编辑）。

```bash
cd web
npm ci
npm run dev     # 开发服务器
npm run build   # 构建到 ../static
npm run test:regression
```

详见 [web/README.md](../web/README.md)。

## API 文档（Swagger）

服务启动后访问 `http://localhost:8080/swagger/index.html`。handler 注释变更后重新生成：

```bash
go install github.com/swaggo/swag/cmd/swag@latest   # 仅需一次
swag init -g cmd/main.go -o docs --parseDependency --parseInternal
```

`docs/docs.go`、`docs/swagger.json`、`docs/swagger.yaml` 是 `go build` 的编译依赖，不要删除。详见 [api/README.md](../api/README.md)。

## 测试

```bash
go test ./...
```

部分集成测试需要真实 API Key，已在 `.gitignore` 中排除（如 `agent/bot/integration_test.go`），凭据模板为 `*.env.test.example`。

## 各包文档

多数目录下都有 `README.md`，说明该包的职责与关键类型，例如：
[agent/memory](../agent/memory/README.md) · [channel](../channel/README.md) · [sandbox](../sandbox/README.md) · [notify](../notify/README.md) · [loader](../loader/README.md) · [identity](../identity/README.md) · [auth](../auth/README.md) · [llm](../llm/README.md) · [tools](../tools/README.md) · [cmd](../cmd/README.md)
