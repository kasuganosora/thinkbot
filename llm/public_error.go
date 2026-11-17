package llm

import (
	"context"
	"errors"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// PublicFailureText returns a fixed, sanitized end-user message for known LLM
// error categories without echoing upstream messages, parameters, URLs, or secrets.
func PublicFailureText(err error) string {
	var le *LLMError
	if !errors.As(err, &le) || le == nil {
		return ""
	}
	switch le.Reason {
	case ErrorReasonAuthentication:
		if le.Message == "provider denied access" {
			return "模型服务拒绝了访问。"
		}
		return "模型服务拒绝了当前密钥。"
	case ErrorReasonInvalidRequest:
		return "模型服务拒绝了本次请求参数。"
	case ErrorReasonContextOverflow:
		return "对话内容超出了模型上下文长度上限。"
	case ErrorReasonRateLimit:
		return "模型服务请求过于频繁，请稍后再试。"
	case ErrorReasonQuotaExceeded:
		return "模型服务额度不足或账户已欠费。"
	case ErrorReasonContentPolicy:
		return "请求或回复触发了模型服务的内容安全限制。"
	case ErrorReasonNoRoute:
		return "所选模型不存在或当前不可用。"
	case ErrorReasonProviderInternal:
		return "模型服务当前繁忙或内部异常，请稍后再试。"
	case ErrorReasonTransport:
		return "连接模型服务失败，请稍后再试。"
	default:
		return ""
	}
}

// UserFailureText 只描述模型调用失败。
// 401 和 403 各用一句固定说明。其它 LLM 错误用同一句，不带上游正文。
// 不是模型错误时返回空字符串，避免把磁盘、渠道或取消说成模型故障。
func UserFailureText(err error) string {
	if err == nil || errors.Is(err, context.Canceled) {
		return ""
	}
	var le *LLMError
	if !errors.As(err, &le) || le == nil {
		return ""
	}
	if text := PublicFailureText(err); text != "" {
		return text
	}
	return "模型服务暂时没有返回结果。"
}

// SafeErrorText 是可以放进日志和事件的错误说明。
// 不认识的错误不回显原文，避免上游正文里的密钥漏出去。
func SafeErrorText(err error) string {
	if err == nil {
		return ""
	}
	var le *LLMError
	if errors.As(err, &le) && le != nil {
		return le.Error()
	}
	var pub interface{ PublicError() string }
	if errors.As(err, &pub) && pub != nil {
		if text := pub.PublicError(); text != "" {
			return text
		}
	}
	var own *errs.Error
	if errors.As(err, &own) && own != nil {
		if text := own.PublicMessage(); text != "" {
			return text
		}
		return "request failed"
	}
	return "request failed"
}
