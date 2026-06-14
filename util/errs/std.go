package errs

import (
	"errors"
)

// 使用标准库 errors 的 Is 和 As，避免与包内函数重名冲突。
// 这两个变量在 errs.go 中被 Is() / As() 调用。

var (
	errorIs = errors.Is
	errorAs = errors.As
)
