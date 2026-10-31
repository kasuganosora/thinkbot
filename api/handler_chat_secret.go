package api

import (
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/secret"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// handleSubmitChatSecret accepts only explicit browser secret requests. Neither
// the request value nor error details are passed to chat persistence, logs or LLMs.
func (s *Server) handleSubmitChatSecret(c *gin.Context) {
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("未登录"))
		return
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var req struct {
		BotID     string `json:"botId"`
		SessionID string `json:"sessionId"`
		Value     string `json:"value"`
		Cancel    bool   `json:"cancel"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("密钥提交格式错误"))
		return
	}
	if req.BotID == "" || req.SessionID == "" || c.Param("questionId") == "" {
		Fail(c, errs.BadRequest("缺少密钥请求归属"))
		return
	}
	if !req.Cancel && (len(strings.TrimSpace(req.Value)) < secret.MinValueLen || len(req.Value) > 16384) {
		Fail(c, errs.BadRequest("密钥长度不符合要求，请使用 12 到 16384 字节的值"))
		return
	}
	owner := interaction.SecretReplyChatID(webChatID(user.ID), req.SessionID)
	q, err := interaction.Default().ClaimSecretForm(c.Param("questionId"), req.BotID, owner, req.Cancel)
	if err != nil {
		if errors.Is(err, interaction.ErrQuestionNotFound) {
			Fail(c, errs.NotFound("密钥请求不存在或不属于当前会话，请重新请求"))
		} else {
			Fail(c, errs.Conflict("密钥请求已结束或过期，请重新请求"))
		}
		return
	}
	if req.Cancel {
		OK(c, gin.H{"status": "cancelled"})
		return
	}
	if err := secret.Default().Register(q.BotID, q.SecretName, req.Value, "用户通过密钥输入卡提供"); err != nil {
		Fail(c, errs.Internal("密钥未能保存，请重新发起请求后重试"))
		return
	}
	// The browser may continue the turn using this safe acknowledgement only.
	OK(c, gin.H{"status": "stored", "userText": fmt.Sprintf("[用户已通过密钥输入卡提供 %s；此后使用占位符 %s 继续任务，不要索要或显示原值。]", q.SecretName, secret.Placeholder(q.SecretName))})
}
