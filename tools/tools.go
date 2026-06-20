// Package tools 提供通用的 LLM 工具集（now、web_fetch、calculate、random、uuid）。
//
// 这些工具是全局性的，不依赖特定 bot 的工作空间，适用于所有场景。
//
// 注册示例：
//
//	import (
//	    agenttools "github.com/kasuganosora/thinkbot/agent/tools"
//	    "github.com/kasuganosora/thinkbot/tools"
//	)
//
//	err := tools.RegisterTools(toolMgr, tools.Config{
//	    TimezoneResolver: func(botID string) string {
//	        return cfgBuilder.GetBotTimezone(botID)
//	    },
//	})
package tools

import (
	"time"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// Config 是通用工具的配置。
type Config struct {
	// TimezoneResolver 根据 botID 返回时区标识符（IANA 格式）。
	// 用于 now 工具的 per-bot 时区支持。
	// 为 nil 时使用 time.Local。
	TimezoneResolver func(botID string) string

	// HTTPTimeout HTTP 请求超时时间（默认 30 秒）。
	HTTPTimeout time.Duration

	// MaxFetchSize web_fetch 返回的最大 body 字节数（默认 32768）。
	MaxFetchSize int

	// UserAgent HTTP 请求的 User-Agent 头（默认 "ThinkbotBot/1.0"）。
	UserAgent string

	// SearchConfig 搜索工具配置（可选）。
	// 为空时使用默认配置。
	SearchConfig *SearchConfig
}

// defaults 填充零值字段。
func (c Config) defaults() Config {
	if c.HTTPTimeout == 0 {
		c.HTTPTimeout = 30 * time.Second
	}
	if c.MaxFetchSize == 0 {
		c.MaxFetchSize = 32768
	}
	if c.UserAgent == "" {
		c.UserAgent = "ThinkbotBot/1.0"
	}
	return c
}

// ============================================================================
// 注册
// ============================================================================

// RegisterTools 将所有通用工具注册到 ToolManager。
//
// 注册的工具：
//   - now:          获取当前时间（支持 per-bot 时区）
//   - web_fetch:    获取网页内容 / 发送 HTTP 请求
//   - web_search:   搜索互联网获取信息
//   - calculate:    计算数学表达式
//   - random:       生成随机数
//   - uuid:         生成 UUID
//   - datetime_calc: 日期时间计算
//   - list_files:   列出目录内容
//   - text_hash:    计算文本哈希
//   - text_encode:  Base64 编解码
//   - text_diff:    文本差异比较
//   - text_stats:   文本统计
//
// now 工具通过 ToolProvider 动态提供（per-bot 时区），
// 其余工具为静态注册。
func RegisterTools(mgr *agenttools.ToolManager, cfg Config) error {
	cfg = cfg.defaults()

	// 静态工具
	staticDefs := []agenttools.ToolDef{
		webFetchToolDef(cfg),
		calculateToolDef(),
		randomToolDef(),
		uuidToolDef(),
		datetimeCalcToolDef(),
		listFilesToolDef(),
		textHashToolDef(),
		textEncodeToolDef(),
		textDiffToolDef(),
		textStatsToolDef(),
	}

	if err := mgr.RegisterMany(staticDefs...); err != nil {
		return err
	}

	// 搜索工具（可选）
	if cfg.SearchConfig != nil {
		if err := RegisterSearchTools(mgr, *cfg.SearchConfig); err != nil {
			return err
		}
	} else {
		// 默认使用 DuckDuckGo
		if err := RegisterSearchTools(mgr, DefaultSearchConfig()); err != nil {
			return err
		}
	}

	// now 工具：动态提供（per-bot 时区）
	mgr.AddProvider(&nowToolProvider{
		resolveTimezone: cfg.TimezoneResolver,
	})

	// 提示词段落（隐藏占位工具，永不出现在工具列表）
	_ = mgr.Register(agenttools.ToolDef{
		Tool:          llm.Tool{Name: "__common_tools_meta", Description: "internal: common tools prompt section"},
		Category:      "utility",
		Scopes:        []string{"__never__"},
		PromptSection: commonToolsPromptSection,
	})

	return nil
}
