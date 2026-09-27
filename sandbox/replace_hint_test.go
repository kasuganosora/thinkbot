package sandbox

import (
	"fmt"
	"strings"
	"testing"
)

func TestOldStrNotFound_WhitespaceOnly(t *testing.T) {
	content := "func main() {\n\tfmt.Println(\"hi\")\n\treturn\n}\n"
	err := oldStrNotFoundError("main.go", content, "    fmt.Println(\"hi\")\n    return")
	msg := err.Error()
	if !strings.Contains(msg, "different whitespace") || !strings.Contains(msg, "lines 2-3") || !strings.Contains(msg, "\tfmt.Println") {
		t.Fatalf("unexpected: %s", msg)
	}
}

func TestOldStrNotFound_ClosestSnippet(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 500; i++ {
		fmt.Fprintf(&b, "line number %d of filler text\n", i)
	}
	b.WriteString("const apiBase = \"https://api.odpt.org/api/v4\"\n")
	b.WriteString("const timeoutSeconds = 30\n")
	content := b.String()
	old := "const apiBase = \"https://api.odpt.org/api/v3\"\nconst timeoutSeconds = 20"
	msg := oldStrNotFoundError("cfg.go", content, old).Error()
	if !strings.Contains(msg, "Closest match (lines 501-502") || !strings.Contains(msg, "api/v4") {
		t.Fatalf("unexpected: %s", msg)
	}
	if !strings.Contains(msg, "Do not retry with the same old_str") {
		t.Error("should forbid blind retries")
	}
}

func TestOldStrNotFound_NothingSimilar(t *testing.T) {
	msg := oldStrNotFoundError("a.txt", "alpha\nbeta\n", "completely unrelated sentence here").Error()
	if !strings.Contains(msg, "Read the file again") {
		t.Fatalf("unexpected: %s", msg)
	}
}
