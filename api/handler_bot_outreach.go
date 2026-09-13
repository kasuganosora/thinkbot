package api

import (
	"context"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/agent/heartbeat"
	"github.com/kasuganosora/thinkbot/agent/outreach"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

func (s *BotService) outreachFallback(botID string) outreach.FallbackSender {
	return func(ctx context.Context, c dao.OutreachCommitment, content string) error {
		switch strings.ToLower(c.ChannelType) {
		case "web":
			if s.chatHistory == nil {
				return errs.New("outreach fallback: chat history unavailable")
			}
			return s.chatHistory.UpsertAssistantByTrace(botID, c.UserID, content, idgen.New("ou"), "", "", c.SessionID, false)
		default:
			return s.heartbeatChannelPoster(botID)(ctx, heartbeat.ChannelTarget{
				Channel:        c.Channel,
				Type:           c.ChannelType,
				ConversationID: c.ConversationID,
			}, content)
		}
	}
}

func (s *Server) outreachStore() *outreach.ConfigStore {
	if s.botSvc != nil {
		if st := s.botSvc.OutreachStore(); st != nil {
			return st
		}
	}
	return outreach.NewConfigStore("data/outreach")
}

func (s *Server) outreachRepo() *outreach.Repo {
	if s.botSvc != nil {
		if r := s.botSvc.OutreachRepo(); r != nil {
			return r
		}
	}
	return outreach.NewRepo(s.db)
}

func (s *Server) handleGetOutreachConfig(c *gin.Context) {
	botID := c.Param("id")
	cfg, err := s.outreachStore().Load(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load outreach config"))
		return
	}
	cfg.Normalize()
	OK(c, cfg)
}

type outreachConfigPatch struct {
	Enabled     *bool                              `json:"enabled"`
	IntervalMin *int                               `json:"interval_min"`
	Platforms   map[string]outreach.PlatformConfig `json:"platforms"`
}

func (s *Server) handleUpdateOutreachConfig(c *gin.Context) {
	botID := c.Param("id")
	var req outreachConfigPatch
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body"))
		return
	}
	store := s.outreachStore()
	cfg, err := store.Load(botID)
	if err != nil {
		Fail(c, errs.Wrap(err, "load outreach config"))
		return
	}
	cfg.Normalize()
	if req.Enabled != nil {
		cfg.Enabled = *req.Enabled
	}
	if req.IntervalMin != nil {
		cfg.IntervalMin = clampInt(*req.IntervalMin, 1, 1440)
	}
	if req.Platforms != nil {
		outreach.ApplyPlatformPatch(&cfg, req.Platforms)
	}
	if err := store.Save(botID, cfg); err != nil {
		Fail(c, errs.Wrap(err, "save outreach config"))
		return
	}
	auditLog(c, s.logger, "update_outreach_config", "bot_id", botID, "enabled", cfg.Enabled, "interval_min", cfg.IntervalMin)
	OK(c, cfg)
}

func (s *Server) handleListOutreachLogs(c *gin.Context) {
	botID := c.Param("id")
	status := c.DefaultQuery("status", "all")
	platform := c.Query("platform")
	rows, err := s.outreachRepo().ListRecords(c.Request.Context(), botID, status, platform, 100)
	if err != nil {
		Fail(c, errs.Wrap(err, "list outreach logs"))
		return
	}
	OK(c, gin.H{"logs": rows, "total": len(rows)})
}

func (s *Server) handleListOutreachCommitments(c *gin.Context) {
	botID := c.Param("id")
	platform := c.Query("platform")
	rows, err := s.outreachRepo().ListPending(c.Request.Context(), botID, platform, "")
	if err != nil {
		Fail(c, errs.Wrap(err, "list outreach commitments"))
		return
	}
	OK(c, gin.H{"commitments": rows, "total": len(rows)})
}

func (s *Server) handleCancelOutreachCommitment(c *gin.Context) {
	botID := c.Param("id")
	cid := c.Param("cid")
	if err := s.outreachRepo().Cancel(c.Request.Context(), botID, cid); err != nil {
		Fail(c, errs.Newf("commitment %q not found", cid))
		return
	}
	auditLog(c, s.logger, "cancel_outreach_commitment", "bot_id", botID, "id", cid)
	OK(c, gin.H{"id": cid, "status": "cancelled"})
}
