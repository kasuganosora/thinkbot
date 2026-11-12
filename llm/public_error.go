package llm

import (
	"context"
	"errors"

	"github.com/kasuganosora/thinkbot/util/errs"
)

// PublicFailureText 是给终端用户的固定说明。
// 认证失败和拒绝访问分开写，都不包含上游正文。
// 不是这两类时返回空字符串，调用方继续用自己的通用收尾。
func PublicFailureText(err error) string {
	var le *LLMError
	if !errors.As(err, &le) || le == nil {
		return ""
	}
	if le.Reason != ErrorReasonAuthentication {
		return ""
	}
	if le.Message == "provider denied access" {
		return "模型服务拒绝了访问。"
	}
	return "模型服务拒绝了当前密钥。"
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
