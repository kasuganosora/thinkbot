package api

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/prompt"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// Bot 人格（SOUL.md）Handler
//
// GET/PUT /api/bots/:id/soul
// 读写工作空间里的 SOUL.md（docker 落 named volume /data/SOUL.md，local 落宿主目录）。
// 系统提示词不走 Bot CRUD，以此文件为单一数据源。
// ============================================================================

// BotSoulResp 人格文件读写响应。
type BotSoulResp struct {
	Content     string           `json:"content"`
	Exists      bool             `json:"exists"`
	HotReloaded bool             `json:"hotReloaded"`
	Warning     string           `json:"warning,omitempty"`
	Findings    []BotSoulFinding `json:"findings,omitempty"`
}

// BotSoulFinding 是管理台扫描告警的一条发现。
type BotSoulFinding struct {
	PatternID string `json:"patternId"`
	Snippet   string `json:"snippet"`
}

// UpdateBotSoulReq 更新人格请求。
type UpdateBotSoulReq struct {
	Content string `json:"content"`
}

// handleGetBotSoul 读取 Bot 的 SOUL.md。
// GET /api/bots/:id/soul
func (s *Server) handleGetBotSoul(c *gin.Context) {
	botID := c.Param("id")
	resp, err := s.readBotSoul(c.Request.Context(), botID)
	if err != nil {
		Fail(c, err)
		return
	}
	OK(c, resp)
}

// handleUpdateBotSoul 写入 Bot 的 SOUL.md。
// PUT /api/bots/:id/soul
func (s *Server) handleUpdateBotSoul(c *gin.Context) {
	botID := c.Param("id")

	var req UpdateBotSoulReq
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}

	resp, err := s.writeBotSoul(c.Request.Context(), botID, req.Content)
	if err != nil {
		Fail(c, err)
		return
	}
	auditLog(c, s.logger, "update_bot_soul", "bot_id", botID, "bytes", len(req.Content))
	OK(c, resp)
}

func (s *Server) readBotSoul(ctx context.Context, botID string) (*BotSoulResp, error) {
	if s.botSvc == nil {
		return nil, errs.New("bot service unavailable")
	}
	loader, running, err := s.botSvc.openSoulLoader(botID)
	if err != nil {
		return nil, err
	}

	st := loader.Stat()
	if st.Err != nil {
		return nil, errs.Wrap(st.Err, "soul: stat")
	}
	if !st.Exists {
		return &BotSoulResp{
			Content:     prompt.DefaultSoulContent,
			Exists:      false,
			HotReloaded: running && loader.Loaded(),
		}, nil
	}

	raw, err := loader.ReadRaw(ctx)
	if err != nil {
		return nil, errs.Wrap(err, "soul: read")
	}
	return &BotSoulResp{
		Content:     string(raw),
		Exists:      true,
		HotReloaded: running && loader.Loaded(),
	}, nil
}

func (s *Server) writeBotSoul(ctx context.Context, botID, content string) (*BotSoulResp, error) {
	if s.botSvc == nil {
		return nil, errs.New("bot service unavailable")
	}
	if strings.TrimSpace(content) == "" {
		return nil, errs.BadRequest("content must not be empty")
	}
	max := prompt.DefaultSoulLoaderConfig().MaxContentBytes
	if max > 0 && len(content) > max {
		return nil, errs.BadRequest("content exceeds max size of " + strconv.Itoa(max) + " bytes")
	}

	loader, running, err := s.botSvc.openSoulLoader(botID)
	if err != nil {
		return nil, err
	}

	findings := prompt.ScanForThreats(content)
	warning := ""
	if len(findings) > 0 {
		warning = prompt.FindingsSummary(findings)
	}

	if err := loader.WriteRaw(ctx, []byte(content)); err != nil {
		return nil, errs.Wrap(err, "soul: write")
	}

	hot := false
	if running {
		if err := loader.Load(); err != nil {
			return nil, errs.Wrap(err, "soul: reload")
		}
		hot = loader.Loaded()
	}

	return &BotSoulResp{
		Content:     content,
		Exists:      true,
		HotReloaded: hot,
		Warning:     warning,
		Findings:    toSoulFindings(findings),
	}, nil
}

func toSoulFindings(in []prompt.ScanFinding) []BotSoulFinding {
	if len(in) == 0 {
		return nil
	}
	out := make([]BotSoulFinding, 0, len(in))
	for _, f := range in {
		snippet := strings.TrimSpace(f.Snippet)
		if utf8.RuneCountInString(snippet) > 80 {
			runes := []rune(snippet)
			snippet = string(runes[:80])
		}
		out = append(out, BotSoulFinding{PatternID: f.PatternID, Snippet: snippet})
	}
	return out
}
