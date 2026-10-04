package llm

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Sandbox paths have to survive compaction. A prose summary drops file paths
// first, and the model then rewrites files it already wrote. The paths are
// copied out of tool calls and pinned back onto the summary. At most
// maxTrackedFilePaths paths are kept.

const (
	readFilesTag        = "read-files"
	modifiedFilesTag    = "modified-files"
	maxTrackedFilePaths = 50
)

func AttachSandboxPaths(summary string, groups ...[]Message) string {
	ops := collectFileOps(summary)
	for _, group := range groups {
		for _, msg := range group {
			ops.absorb(TextFromParts(msg.Content))
			for _, part := range msg.Content {
				tc, ok := part.(ToolCallPart)
				if !ok {
					continue
				}
				ops.note(tc.ToolName, toolPath(tc.Input))
			}
		}
	}
	if len(ops.read) == 0 && len(ops.modified) == 0 {
		return summary
	}
	stripped := stripFileOps(summary)
	return strings.TrimRight(stripped, "\n") + ops.format()
}

type fileOps struct {
	read     []string
	modified []string
	n        int
}

func collectFileOps(summary string) fileOps {
	var ops fileOps
	ops.absorb(summary)
	return ops
}

func (o *fileOps) absorb(content string) {
	if content == "" {
		return
	}
	for _, p := range parseTagged(content, readFilesTag) {
		o.add(&o.read, p)
	}
	for _, p := range parseTagged(content, modifiedFilesTag) {
		o.add(&o.modified, p)
	}
}

func (o *fileOps) note(tool, path string) {
	path = strings.TrimSpace(path)
	if path == "" {
		return
	}
	switch tool {
	case "sandbox_write_file", "sandbox_replace_in_file", "sandbox_delete_file":
		o.add(&o.modified, path)
	case "sandbox_move_file":
		o.add(&o.modified, path)
	case "sandbox_read_file":
		if !containsPath(o.modified, path) {
			o.add(&o.read, path)
		}
	}
}

func (o *fileOps) add(list *[]string, path string) {
	if path == "" || o.n >= maxTrackedFilePaths {
		return
	}
	if containsPath(*list, path) || containsPath(o.read, path) && list == &o.read {
		return
	}
	if containsPath(o.modified, path) {
		return
	}
	*list = append(*list, path)
	o.n++
}

func (o fileOps) format() string {
	if len(o.read) == 0 && len(o.modified) == 0 {
		return ""
	}
	var sb strings.Builder
	writeTagged(&sb, readFilesTag, o.read)
	writeTagged(&sb, modifiedFilesTag, o.modified)
	return sb.String()
}

func writeTagged(sb *strings.Builder, tag string, paths []string) {
	if len(paths) == 0 {
		return
	}
	fmt.Fprintf(sb, "\n\n<%s>\n%s\n</%s>", tag, strings.Join(paths, "\n"), tag)
}

func parseTagged(content, tag string) []string {
	open, closeTag := "<"+tag+">", "</"+tag+">"
	start := strings.Index(content, open)
	if start < 0 {
		return nil
	}
	start += len(open)
	end := strings.Index(content[start:], closeTag)
	if end < 0 {
		return nil
	}
	var paths []string
	for _, line := range strings.Split(content[start:start+end], "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			paths = append(paths, line)
		}
	}
	return paths
}

func stripFileOps(content string) string {
	for _, tag := range []string{readFilesTag, modifiedFilesTag} {
		open, closeTag := "<"+tag+">", "</"+tag+">"
		for {
			start := strings.Index(content, open)
			if start < 0 {
				break
			}
			end := strings.Index(content[start:], closeTag)
			if end < 0 {
				break
			}
			end = start + end + len(closeTag)
			content = content[:start] + content[end:]
		}
	}
	return strings.TrimRight(content, "\n")
}

func containsPath(list []string, path string) bool {
	for _, p := range list {
		if p == path {
			return true
		}
	}
	return false
}

func toolPath(input any) string {
	switch v := input.(type) {
	case map[string]any:
		if p, ok := v["path"].(string); ok && strings.TrimSpace(p) != "" {
			return p
		}
		if p, ok := v["dst"].(string); ok {
			return p
		}
		if p, ok := v["src"].(string); ok {
			return p
		}
	case string:
		var m map[string]any
		if json.Unmarshal([]byte(v), &m) == nil {
			return toolPath(m)
		}
	}
	return ""
}

// nearestSafeBoundary moves a split so it does not fall between a tool call
// and its result. idx <= 0 or idx >= len stays put.
func nearestSafeBoundary(messages []Message, idx int) int {
	if idx <= 0 || idx >= len(messages) {
		return idx
	}
	if IsSafeCompactionBoundary(messages, idx) {
		return idx
	}
	for b := idx - 1; b > 0; b-- {
		if IsSafeCompactionBoundary(messages, b) {
			return b
		}
	}
	for b := idx + 1; b < len(messages); b++ {
		if IsSafeCompactionBoundary(messages, b) {
			return b
		}
	}
	return 0
}
