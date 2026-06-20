# sandbox — Bot 沙箱工作空间

为每个 Bot 提供隔离的持久化工作空间，支持 Docker 容器沙箱和本地文件系统两种模式。

## 功能

- **Docker 沙箱**：每个 Bot 一个容器实例，完全隔离的执行环境
- **本地模式**：基于文件系统的简单工作空间（开发/测试用）
- **工具注册**：自动注册文件操作和命令执行工具（read/write/edit/exec/search）
- **生命周期管理**：容器启动/停止/清理，工作空间文件持久化
- **安全隔离**：禁止目录遍历（`..`），路径限制在工作空间根目录内

## 关键类型

| 类型 | 说明 |
|------|------|
| `BotWorkspaceManager` | 工作空间管理器（多 Bot） |
| `Workspace` | 单个 Bot 的工作空间接口 |
| `DockerWorkspace` | Docker 容器实现 |
| `LocalWorkspace` | 本地文件系统实现 |

## 工具列表

| 工具 | 说明 |
|------|------|
| `exec` | 执行 shell 命令 |
| `read_file` | 读取文件（支持 offset/limit） |
| `write_file` | 写入文件 |
| `replace_in_file` | 精确替换（支持 replace_all） |
| `delete_file` | 删除文件/目录 |
| `move_file` | 移动/重命名 |
| `list_dir` | 列出目录 |
| `search_content` | 内容搜索（grep） |
| `health` | 健康检查 |
