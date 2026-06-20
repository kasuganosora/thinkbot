package tools

import (
	"crypto/rand"
	"fmt"
	"math"
	"math/big"
	mrand "math/rand"
	"strings"

	agenttools "github.com/kasuganosora/thinkbot/agent/tools"
	"github.com/kasuganosora/thinkbot/llm"
)

// ============================================================================
// calculate — 数学表达式计算
// ============================================================================

func calculateToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Category: "utility",
		Tool: llm.Tool{
			Name: "calculate",
			Description: "安全计算数学表达式。支持四则运算（+ - * /）、取余（%）、幂运算（^）、" +
				"括号和常用数学函数（sqrt, abs, round, floor, ceil, sin, cos, ln, log10, min, max）。" +
				"常量：pi, e。当需要精确数值计算时使用。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"expression": map[string]any{
						"type":        "string",
						"description": "数学表达式，如 \"(1 + 2) * 3\"、\"sqrt(16) + 2^3\"、\"sin(pi/2)\"",
					},
				},
				"required": []string{"expression"},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}
				expr, _ := m["expression"].(string)
				if expr == "" {
					return nil, fmt.Errorf("expression is required")
				}

				result, err := evalMath(expr)
				if err != nil {
					return map[string]any{
						"error":      err.Error(),
						"expression": expr,
					}, nil
				}

				return map[string]any{
					"result":      formatResult(result),
					"expression":  expr,
					"isInteger":   result == math.Trunc(result) && !math.IsInf(result, 0),
				}, nil
			}),
		},
	}
}

// formatResult 将 float64 格式化为易读的结果。
func formatResult(v float64) any {
	if math.IsNaN(v) {
		return "NaN"
	}
	if math.IsInf(v, 1) {
		return "Infinity"
	}
	if math.IsInf(v, -1) {
		return "-Infinity"
	}
	if v == math.Trunc(v) && math.Abs(v) < 1e15 {
		return int64(v)
	}
	return v
}

// ============================================================================
// 安全数学表达式求值器（递归下降解析器）
// ============================================================================

type mathParser struct {
	tokens []mathToken
	pos    int
}

type mathToken struct {
	kind  string // "num", "op", "func", "lparen", "rparen", "comma"
	value string
}

func evalMath(expr string) (float64, error) {
	tokens, err := tokenizeMath(expr)
	if err != nil {
		return 0, err
	}
	p := &mathParser{tokens: tokens}
	result, err := p.parseExpr()
	if err != nil {
		return 0, err
	}
	p.skipSpaces()
	if p.pos < len(p.tokens) {
		return 0, fmt.Errorf("unexpected token: %q", p.peek().value)
	}
	return result, nil
}

func tokenizeMath(expr string) ([]mathToken, error) {
	var tokens []mathToken
	i := 0
	for i < len(expr) {
		c := expr[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c >= '0' && c <= '9' || c == '.':
			j := i
			for j < len(expr) && (expr[j] >= '0' && expr[j] <= '9' || expr[j] == '.') {
				j++
			}
			tokens = append(tokens, mathToken{"num", expr[i:j]})
			i = j
		case c == '+' || c == '-' || c == '*' || c == '/' || c == '%' || c == '^':
			tokens = append(tokens, mathToken{"op", string(c)})
			i++
		case c == '(':
			tokens = append(tokens, mathToken{"lparen", "("})
			i++
		case c == ')':
			tokens = append(tokens, mathToken{"rparen", ")"})
			i++
		case c == ',':
			tokens = append(tokens, mathToken{"comma", ","})
			i++
		case isAlpha(c):
			j := i
			for j < len(expr) && (isAlpha(expr[j]) || (expr[j] >= '0' && expr[j] <= '9')) {
				j++
			}
			name := expr[i:j]
			i = j
			// 检查是否是函数调用（后面跟括号）
			if j < len(expr) && expr[j] == '(' {
				tokens = append(tokens, mathToken{"func", name})
			} else {
				// 常量
				tokens = append(tokens, mathToken{"const", name})
			}
		default:
			return nil, fmt.Errorf("unexpected character: %q", string(c))
		}
	}
	return tokens, nil
}

func isAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_'
}

func (p *mathParser) peek() mathToken {
	if p.pos >= len(p.tokens) {
		return mathToken{}
	}
	return p.tokens[p.pos]
}

func (p *mathParser) advance() mathToken {
	t := p.peek()
	p.pos++
	return t
}

func (p *mathParser) skipSpaces() {
	for p.pos < len(p.tokens) && p.tokens[p.pos].kind == "space" {
		p.pos++
	}
}

// parseExpr: expr → term (('+' | '-') term)*
func (p *mathParser) parseExpr() (float64, error) {
	left, err := p.parseTerm()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind == "op" && (t.value == "+" || t.value == "-") {
			p.advance()
			right, err := p.parseTerm()
			if err != nil {
				return 0, err
			}
			switch t.value {
			case "+":
				left += right
			case "-":
				left -= right
			}
		} else {
			break
		}
	}
	return left, nil
}

// parseTerm: term → factor (('*' | '/' | '%') factor)*
func (p *mathParser) parseTerm() (float64, error) {
	left, err := p.parseFactor()
	if err != nil {
		return 0, err
	}
	for {
		t := p.peek()
		if t.kind == "op" && (t.value == "*" || t.value == "/" || t.value == "%") {
			p.advance()
			right, err := p.parseFactor()
			if err != nil {
				return 0, err
			}
			switch t.value {
			case "*":
				left *= right
			case "/":
				if right == 0 {
					return 0, fmt.Errorf("division by zero")
				}
				left /= right
			case "%":
				if right == 0 {
					return 0, fmt.Errorf("modulo by zero")
				}
				left = math.Mod(left, right)
			}
		} else {
			break
		}
	}
	return left, nil
}

// parseFactor: factor → power ('^' factor)?  (右结合)
func (p *mathParser) parseFactor() (float64, error) {
	base, err := p.parseUnary()
	if err != nil {
		return 0, err
	}
	t := p.peek()
	if t.kind == "op" && t.value == "^" {
		p.advance()
		exp, err := p.parseFactor() // 右结合：递归调用自身
		if err != nil {
			return 0, err
		}
		return math.Pow(base, exp), nil
	}
	return base, nil
}

// parseUnary: unary → ('-' | '+') unary | primary
func (p *mathParser) parseUnary() (float64, error) {
	t := p.peek()
	if t.kind == "op" && (t.value == "-" || t.value == "+") {
		p.advance()
		val, err := p.parseUnary()
		if err != nil {
			return 0, err
		}
		if t.value == "-" {
			return -val, nil
		}
		return val, nil
	}
	return p.parsePrimary()
}

// parsePrimary: primary → num | const | func '(' args ')' | '(' expr ')'
func (p *mathParser) parsePrimary() (float64, error) {
	t := p.peek()
	switch t.kind {
	case "num":
		p.advance()
		v := 0.0
		if _, err := fmt.Sscanf(t.value, "%g", &v); err != nil {
			return 0, fmt.Errorf("invalid number: %q", t.value)
		}
		return v, nil

	case "const":
		p.advance()
		switch strings.ToLower(t.value) {
		case "pi":
			return math.Pi, nil
		case "e":
			return math.E, nil
		default:
			return 0, fmt.Errorf("unknown constant: %q", t.value)
		}

	case "func":
		p.advance()
		if p.peek().kind != "lparen" {
			return 0, fmt.Errorf("expected '(' after function %q", t.value)
		}
		p.advance() // consume '('
		args := []float64{}
		if p.peek().kind != "rparen" {
			for {
				val, err := p.parseExpr()
				if err != nil {
					return 0, err
				}
				args = append(args, val)
				if p.peek().kind == "comma" {
					p.advance()
					continue
				}
				break
			}
		}
		if p.peek().kind != "rparen" {
			return 0, fmt.Errorf("expected ')' after function arguments")
		}
		p.advance() // consume ')'
		return applyFunc(t.value, args)

	case "lparen":
		p.advance()
		val, err := p.parseExpr()
		if err != nil {
			return 0, err
		}
		if p.peek().kind != "rparen" {
			return 0, fmt.Errorf("expected ')'")
		}
		p.advance()
		return val, nil

	default:
		return 0, fmt.Errorf("unexpected token: %q", t.value)
	}
}

func applyFunc(name string, args []float64) (float64, error) {
	name = strings.ToLower(name)
	switch name {
	case "sqrt":
		if len(args) != 1 {
			return 0, fmt.Errorf("sqrt expects 1 argument, got %d", len(args))
		}
		return math.Sqrt(args[0]), nil
	case "abs":
		if len(args) != 1 {
			return 0, fmt.Errorf("abs expects 1 argument, got %d", len(args))
		}
		return math.Abs(args[0]), nil
	case "round":
		if len(args) != 1 {
			return 0, fmt.Errorf("round expects 1 argument, got %d", len(args))
		}
		return math.Round(args[0]), nil
	case "floor":
		if len(args) != 1 {
			return 0, fmt.Errorf("floor expects 1 argument, got %d", len(args))
		}
		return math.Floor(args[0]), nil
	case "ceil":
		if len(args) != 1 {
			return 0, fmt.Errorf("ceil expects 1 argument, got %d", len(args))
		}
		return math.Ceil(args[0]), nil
	case "sin":
		if len(args) != 1 {
			return 0, fmt.Errorf("sin expects 1 argument, got %d", len(args))
		}
		return math.Sin(args[0]), nil
	case "cos":
		if len(args) != 1 {
			return 0, fmt.Errorf("cos expects 1 argument, got %d", len(args))
		}
		return math.Cos(args[0]), nil
	case "tan":
		if len(args) != 1 {
			return 0, fmt.Errorf("tan expects 1 argument, got %d", len(args))
		}
		return math.Tan(args[0]), nil
	case "ln":
		if len(args) != 1 {
			return 0, fmt.Errorf("ln expects 1 argument, got %d", len(args))
		}
		return math.Log(args[0]), nil
	case "log", "log10":
		if len(args) != 1 {
			return 0, fmt.Errorf("log expects 1 argument, got %d", len(args))
		}
		return math.Log10(args[0]), nil
	case "exp":
		if len(args) != 1 {
			return 0, fmt.Errorf("exp expects 1 argument, got %d", len(args))
		}
		return math.Exp(args[0]), nil
	case "min":
		if len(args) < 1 {
			return 0, fmt.Errorf("min expects at least 1 argument")
		}
		min := args[0]
		for _, v := range args[1:] {
			if v < min {
				min = v
			}
		}
		return min, nil
	case "max":
		if len(args) < 1 {
			return 0, fmt.Errorf("max expects at least 1 argument")
		}
		max := args[0]
		for _, v := range args[1:] {
			if v > max {
				max = v
			}
		}
		return max, nil
	default:
		return 0, fmt.Errorf("unknown function: %q", name)
	}
}

// ============================================================================
// random — 生成随机数
// ============================================================================

func randomToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Category: "utility",
		Tool: llm.Tool{
			Name: "random",
			Description: "生成随机数。可以生成指定范围内的随机整数或浮点数，" +
				"也可以随机选择列表中的元素。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"min": map[string]any{
						"type":        "number",
						"description": "最小值（含），默认 0",
						"default":     0,
					},
					"max": map[string]any{
						"type":        "number",
						"description": "最大值（含）",
					},
					"type": map[string]any{
						"type":        "string",
						"enum":        []string{"int", "float"},
						"description": "随机数类型：int（整数，默认）或 float（浮点数）",
						"default":     "int",
					},
					"count": map[string]any{
						"type":        "integer",
						"description": "生成数量（默认 1，大于 1 时返回数组）",
						"default":     1,
					},
					"choices": map[string]any{
						"type":        "array",
						"items":       map[string]any{"type": "string"},
						"description": "从中随机选择的列表（设置后忽略 min/max/type/count）",
					},
				},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					return nil, fmt.Errorf("invalid input: expected object")
				}

				// 随机选择模式
				if choices, ok := m["choices"].([]any); ok && len(choices) > 0 {
					idx := mrand.Intn(len(choices))
					pick := choices[idx]
					return map[string]any{
						"result":   pick,
						"index":    idx,
						"fromCount": len(choices),
					}, nil
				}

				minVal, maxVal := 0.0, 1.0
				if v, ok := m["min"]; ok {
					minVal = toFloat(v)
				}
				if v, ok := m["max"]; ok {
					maxVal = toFloat(v)
				}
				if maxVal < minVal {
					minVal, maxVal = maxVal, minVal
				}

				randType, _ := m["type"].(string)
				if randType == "" {
					randType = "int"
				}

				count := 1
				if v, ok := m["count"]; ok {
					count = toInt(v)
				}
				if count < 1 {
					count = 1
				}
				if count > 1000 {
					count = 1000
				}

				results := make([]any, count)
				for i := 0; i < count; i++ {
					if randType == "float" {
						results[i] = minVal + mrand.Float64()*(maxVal-minVal)
					} else {
						// 整数范围 [minVal, maxVal]
						lo := int64(math.Ceil(minVal))
						hi := int64(math.Floor(maxVal))
						if hi < lo {
							results[i] = lo
							continue
						}
						delta := big.NewInt(hi - lo + 1)
						n, err := rand.Int(rand.Reader, delta)
						if err != nil {
							results[i] = lo
							continue
						}
						results[i] = n.Int64() + lo
					}
				}

				if count == 1 {
					return map[string]any{
						"result": results[0],
						"type":   randType,
					}, nil
				}
				return map[string]any{
					"results": results,
					"count":   count,
					"type":    randType,
				}, nil
			}),
		},
	}
}

func toFloat(v any) float64 {
	switch n := v.(type) {
	case float64:
		return n
	case float32:
		return float64(n)
	case int:
		return float64(n)
	case int64:
		return float64(n)
	default:
		return 0
	}
}

func toInt(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case float32:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	default:
		return 0
	}
}

// ============================================================================
// uuid — 生成 UUID
// ============================================================================

func uuidToolDef() agenttools.ToolDef {
	return agenttools.ToolDef{
		Category: "utility",
		Tool: llm.Tool{
			Name: "uuid",
			Description: "生成 UUID（Universally Unique Identifier）。生成基于加密安全的随机 UUID v4。" +
				"适用于生成唯一标识符、会话 ID、临时令牌等场景。",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"count": map[string]any{
						"type":        "integer",
						"description": "生成数量（默认 1，最大 100）",
						"default":     1,
					},
				},
			},
			Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
				m, ok := input.(map[string]any)
				if !ok {
					m = map[string]any{}
				}

				count := 1
				if v, ok := m["count"]; ok {
					count = toInt(v)
				}
				if count < 1 {
					count = 1
				}
				if count > 100 {
					count = 100
				}

				uuids := make([]string, count)
				for i := 0; i < count; i++ {
					u, err := generateUUIDv4()
					if err != nil {
						return nil, fmt.Errorf("failed to generate uuid: %w", err)
					}
					uuids[i] = u
				}

				if count == 1 {
					return map[string]any{"uuid": uuids[0]}, nil
				}
				return map[string]any{"uuids": uuids, "count": count}, nil
			}),
		},
	}
}

// generateUUIDv4 生成一个 UUID v4 字符串（基于 crypto/rand）。
func generateUUIDv4() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	// 设置 version 4 和 variant
	b[6] = (b[6] & 0x0f) | 0x40 // version 4
	b[8] = (b[8] & 0x3f) | 0x80 // variant 10
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]), nil
}
