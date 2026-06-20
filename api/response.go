package api

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// ============================================================================
// 统一响应格式
//
// 所有 API 返回统一 JSON 结构：
//
//	成功：{"code": 0, "message": "ok", "data": {...}}
//	失败：{"code": <非零>, "message": "错误描述", "data": null}
//
// HTTP 状态码由 errs.GetCode() 从错误链中提取，默认 500。
// ============================================================================

// Response 统一响应结构。
type Response struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data"`
}

// OK 返回成功响应。
func OK(c *gin.Context, data any) {
	c.JSON(http.StatusOK, Response{
		Code:    0,
		Message: "ok",
		Data:    data,
	})
}

// OKMsg 返回带自定义消息的成功响应。
func OKMsg(c *gin.Context, message string, data any) {
	c.JSON(http.StatusOK, Response{
		Code:    0,
		Message: message,
		Data:    data,
	})
}

// Fail 从 error 返回失败响应，自动提取 HTTP 状态码。
func Fail(c *gin.Context, err error) {
	if err == nil {
		c.JSON(http.StatusOK, Response{Code: 0, Message: "ok"})
		return
	}

	httpCode := errs.GetCode(err)
	if httpCode == 0 {
		httpCode = http.StatusInternalServerError
	}

	c.JSON(httpCode, Response{
		Code:    httpCode,
		Message: err.Error(),
		Data:    nil,
	})
}

// FailMsg 返回带指定 HTTP 状态码的失败响应。
func FailMsg(c *gin.Context, httpCode int, message string) {
	c.JSON(httpCode, Response{
		Code:    httpCode,
		Message: message,
		Data:    nil,
	})
}
