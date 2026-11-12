package llm

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestNoHardcodedSamplingLiterals fails when a chat call site grows a new
// numeric WithTemperature / WithTopP, or a local temp := <number> assignment,
// outside the provider-config resolver.
func TestNoHardcodedSamplingLiterals(t *testing.T) {
	root := moduleRoot(t)
	call := regexp.MustCompile(`With(?:Temperature|TopP)\(\s*-?(?:0|[1-9])`)
	assign := regexp.MustCompile(`(?m)(?:^|[^.\w])temp\s*(?::=|=)\s*-?(?:0(?:\.\d+)?|[1-9]\d*(?:\.\d+)?)`)
	allowAssign := map[string]bool{
		filepath.Join("config", "module.go"): true, // provider default when the model config leaves temperature unset
	}
	var hits []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "vendor", "web", "node_modules":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			rel = path
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		text := string(body)
		if loc := call.FindStringIndex(text); loc != nil {
			hits = append(hits, rel+": "+oneLine(text, loc[0]))
		}
		if !allowAssign[rel] {
			if loc := assign.FindStringIndex(text); loc != nil {
				hits = append(hits, rel+": "+oneLine(text, loc[0]))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) > 0 {
		t.Fatalf("hardcoded sampling outside the config resolver:\n%s", strings.Join(hits, "\n"))
	}
}

func oneLine(text string, at int) string {
	start := strings.LastIndex(text[:at], "\n") + 1
	end := strings.Index(text[at:], "\n")
	if end < 0 {
		end = len(text)
	} else {
		end += at
	}
	line := strings.TrimSpace(text[start:end])
	if len(line) > 160 {
		line = line[:160]
	}
	return line
}

func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
