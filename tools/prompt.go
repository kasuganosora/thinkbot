package tools

import (
	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
)

// commonToolsPromptSection 是通用工具的提示词段落。
var commonToolsPromptSection = &agenttools.ToolPromptSection{
	Name:  "common_tools",
	Order: 320,
	Content: `# Common Tools

You have access to the following general-purpose tools. They work in any context and do not depend on a workspace.

## Time
- **now** — Get the current date and time, already adjusted to the user's timezone.

## Network
- **web_fetch** — Fetch a URL or send an HTTP request. Defaults to GET; set method/headers/body for other verbs. Returns the status code, Content-Type and a truncated response body.

## Math
- **calculate** — Safely evaluate a math expression. Supports + - * / % ^, parentheses, functions (sqrt/abs/round/floor/ceil/sin/cos/tan/ln/log10/exp/min/max) and the constants pi and e.

## Randomness
- **random** — Generate random integers or floats, or pick randomly from a list.
- **uuid** — Generate UUID v4 identifiers.

## Usage rules
- ALWAYS call **now** when the answer depends on the current date or time. NEVER guess it.
- ALWAYS call **calculate** for arithmetic that must be exact. NEVER compute it in your head.
- Use **web_fetch** to retrieve web pages or API data. For a plain GET pass only url; set method/headers/body only when the request requires it.
- IMPORTANT: You must respond to the user in Chinese (中文), even though these instructions are in English.

<example>
user: 现在几点了？
assistant: [calls now]
现在是 2026 年 8 月 4 日 14:32（周二）。
</example>

<example>
user: 帮我算一下 (1234 * 5678) / 91
assistant: [calls calculate with expression "(1234 * 5678) / 91"]
结果是 76996.42。
</example>`,
	Enabled: true,
}
