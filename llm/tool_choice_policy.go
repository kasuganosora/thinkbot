package llm

import "strings"

// ============================================================================
// tool_choice 兼容性
//
// 部分推理优先模型在思考模式下会拒绝 tool_choice="required"（HTTP 400），
// 且这类模型默认即为 thinking mode，与是否显式传 reasoning_effort 无关。
// 项目的防偷懒门禁（verify.required）与 L1 路由在未决时都会把首步
// tool_choice 设为 required，直接打到这类模型会让整轮请求失败、用户收不到回复。
//
// 这里按模型族集中声明该限制，并在请求参数层面把 required 降级为 auto：
// 保留 tools 让模型仍可自主决定是否调用；none、auto、具体函数对象原样透传。
// 新增供应商只改 requiredToolChoiceUnsupported，别在调用点散厂商判断。
// ============================================================================

// NormalizeToolChoice 把目标模型不接受的 "required" 降级为 "auto"，
// 其余取值原样返回。
func NormalizeToolChoice(modelID string, choice any) any {
	if choice == nil {
		return nil
	}
	s, ok := choice.(string)
	if !ok || s != "required" {
		return choice
	}
	if !requiredToolChoiceUnsupported(modelID) {
		return choice
	}
	return "auto"
}

// requiredToolChoiceUnsupported 集中声明「思考模式不接受 tool_choice=required」
// 的模型族。按去掉供应商前缀后的模型 ID 前缀匹配（例如 "<vendor>/deepseek-flash"）。
func requiredToolChoiceUnsupported(modelID string) bool {
	m := strings.ToLower(strings.TrimSpace(modelID))
	if i := strings.LastIndex(m, "/"); i >= 0 {
		m = m[i+1:]
	}
	return strings.HasPrefix(m, "deepseek-")
}
