package api

import (
	"errors"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/internal/interaction"
	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// user_choice 回填 Handler — Web 端「用户作答」的下行通路
//
// 全链路：
//  1. LLM 调 user_choice 工具 → 工具注册问题到 interaction 注册表并阻塞等待；
//  2. 工具发 tool_progress 事件（携带 questionId/options/…）→ SSE → 前端
//     渲染 ChoiceCard 内联卡片；
//  3. 用户点选/输入 → 本接口 → interaction.Resolve → 唤醒步骤 1 的等待者，
//     工具返回结果，LLM 继续本轮编排。
//
// 缺了本接口，第 3 步断链：卡片能显示但一点就 404，工具白等到超时（默认
// 600s）才恢复，对用户就是「bot 卡住不说话」。
// ============================================================================

// userChoiceAnswerRequest 是回填请求体。
//
// 字段名对齐前端 web/src/api/services.js 的 userChoiceApi.answer：
//   - SelectedIds 是选项 id（不是下标）。用 id 线传是为了不依赖两侧选项顺序
//     一致，翻译成下标由 interaction 包负责。
//   - FreeText 是自由输入框内容，可与选项同时提交。
//
// 归属校验**不走请求体**：客户端自报的会话 ID 谁都能伪造，起不到隔离作用。
// 正确的归属键是登录态推导出来的 chatID（见 handler 内 webChatID）。
type userChoiceAnswerRequest struct {
	SelectedIds []string `json:"selectedIds"`
	FreeText    string   `json:"freeText"`
}

// webChatID 复刻 WebChannel.Inject 里的会话空间命名（api/webchannel.go:
// `Channel: "web:" + userID`）。interaction.Question.ChatID 存的就是这个值，
// 所以「谁能替这道题作答」等价于「问题的 ChatID 是否等于当前登录用户的 web 会话空间」。
//
// 两处必须同步：webchannel.go 改了 Channel 拼法，这里也要改，否则归属校验
// 会把**所有** web 作答判成越权（表现为点了卡片报 404）。
func webChatID(userID uint) string {
	return "web:" + strconv.FormatUint(uint64(userID), 10)
}

// handleAnswerUserChoice 回填用户对 user_choice 问题的作答。
// POST /api/user-choice/:questionId/answer
//
// @Summary      提交选择卡作答
// @Description  Web 端用户点选/输入后回填 user_choice 工具正在等待的问题
// @Tags         交互
// @Accept       json
// @Produce      json
// @Param        questionId  path      string                   true  "问题 ID"
// @Param        body        body      userChoiceAnswerRequest  true  "作答内容"
// @Success      200         {object}  Response
// @Security     CookieAuth
// @Router       /api/user-choice/{questionId}/answer [post]
func (s *Server) handleAnswerUserChoice(c *gin.Context) {
	questionID := strings.TrimSpace(c.Param("questionId"))
	if questionID == "" {
		Fail(c, errs.BadRequest("questionId is required"))
		return
	}

	var req userChoiceAnswerRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	req.FreeText = strings.TrimSpace(req.FreeText)
	if len(req.SelectedIds) == 0 && req.FreeText == "" {
		Fail(c, errs.BadRequest("selectedIds 与 freeText 不能同时为空"))
		return
	}

	// 归属键：登录用户 → web 会话空间。取不到用户视为未登录（路由挂在 authed
	// 组下，正常不会发生），此时给空串退化为不校验会显得更危险，直接拒。
	user := currentUser(c)
	if user == nil {
		Fail(c, errs.Unauthorized("未登录"))
		return
	}
	owner := webChatID(user.ID)

	reg := interaction.Default()

	// 先取问题快照：一是把「问题不存在/已终态」这两类客户端可理解的情况
	// 用准确状态码回出去（而不是笼统 500），二是拿到选项表做 id→下标翻译。
	q, err := reg.Lookup(questionID)
	if err != nil {
		Fail(c, errs.NotFound("问题不存在或已过期："+questionID))
		return
	}
	// 越权拦在状态检查之前：先回 Conflict("已作答") 再回 NotFound 会把
	// 「这个 questionId 存在且已被答过」泄漏给非归属者。
	if q.ChatID != "" && q.ChatID != owner {
		Fail(c, errs.NotFound("问题不存在或不属于当前用户"))
		return
	}
	if q.Status != interaction.StatusPending {
		Fail(c, errs.Conflict("该问题已结束（"+string(q.Status)+"），无法再次作答"))
		return
	}

	indices, err := reg.IndicesForOptionIDs(questionID, req.SelectedIds)
	if err != nil {
		Fail(c, errs.BadRequest(err.Error()))
		return
	}

	ans := interaction.Answer{
		Selected:    indices,
		CustomInput: req.FreeText,
		Via:         interaction.ViaWeb,
	}
	// 再走一次带归属的 Resolve：上面的快照校验与这里之间存在窗口期，
	// ResolveFrom 在持锁状态下复核一次，防止并发下的越权穿透。
	if err := reg.ResolveFrom(questionID, owner, ans); err != nil {
		switch {
		case errors.Is(err, interaction.ErrQuestionNotFound):
			// 含「会话不匹配」：不暴露归属细节，统一按不存在处理。
			Fail(c, errs.NotFound("问题不存在或不属于当前会话"))
		case errors.Is(err, interaction.ErrAlreadyResolved):
			Fail(c, errs.Conflict("该问题已被作答或已结束"))
		case errors.Is(err, interaction.ErrInvalidSelected), errors.Is(err, interaction.ErrInvalidVia):
			Fail(c, errs.BadRequest(err.Error()))
		default:
			Fail(c, errs.Wrap(err, "回填选择失败"))
		}
		return
	}

	if s.logger != nil {
		s.logger.Infow("user_choice answered via web",
			"question_id", questionID,
			"bot_id", q.BotID,
			"chat_id", q.ChatID,
			"selected_ids", req.SelectedIds,
			"has_free_text", req.FreeText != "")
	}

	OK(c, gin.H{"accepted": true, "questionId": questionID})
}
