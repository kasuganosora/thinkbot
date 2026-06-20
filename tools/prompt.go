package tools

import (
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
)

// commonToolsPromptSection 是通用工具的提示词段落。
var commonToolsPromptSection = &agenttools.ToolPromptSection{
	Name:  "common_tools",
	Order: 320,
	Content: `# 通用工具

你可以使用以下通用工具来辅助回答：

## 时间
- **now** — 获取当前日期和时间（已按你的时区自动调整）

## 网络
- **web_fetch** — 获取网页内容或发送 HTTP 请求（默认 GET，可通过 method/headers/body 发送自定义请求），返回状态码和截断后的响应正文

## 计算
- **calculate** — 安全计算数学表达式（支持 + - * / % ^、括号、sqrt/sin/cos/ln 等函数、pi/e 常量）

## 随机
- **random** — 生成随机数（整数/浮点数/从列表中随机选择）
- **uuid** — 生成 UUID v4

## 使用提示
- 当用户问"现在几点"时，调用 **now**
- 当需要精确数值计算时，调用 **calculate** 而不是自己算
- 当需要获取网页或 API 数据时，调用 **web_fetch**（简单 GET 只传 url，复杂请求可设置 method/headers/body）`,
	Enabled: true,
}
