package selfhost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kasuganosora/thinkbot/llm"
)

// gitDir 读取 loader.git_dir（源码树根，自部署在此编译）。默认 /app/src。
func (p *Provider) gitDir() string {
	if v, ok := p.store.Get("loader.git_dir"); ok && v != "" {
		return v
	}
	return "/app/src"
}

// safePath 把相对路径解析到 gitDir 内并做穿越防护。
// 返回绝对路径；若越界或非法返回错误。写操作额外禁止进入 .git 目录。
func (p *Provider) safePath(rel string, allowGit bool) (string, error) {
	root := p.gitDir()
	abs, err := filepath.Abs(filepath.Join(root, rel))
	if err != nil {
		return "", fmt.Errorf("invalid path %q: %w", rel, err)
	}
	clean := filepath.Clean(abs)
	// 必须落在 root 之下（处理符号链接略，loader 场景源码树无外部 symlink）
	if clean != root && !strings.HasPrefix(clean, root+string(os.PathSeparator)) {
		return "", fmt.Errorf("path escapes root; access outside %s is forbidden", root)
	}
	if !allowGit {
		// 禁止改写 .git 内部，避免破坏版本库导致后续部署失败
		relToRoot := strings.TrimPrefix(clean, root+string(os.PathSeparator))
		parts := strings.Split(relToRoot, string(os.PathSeparator))
		for _, part := range parts {
			if part == ".git" {
				return "", fmt.Errorf("access to .git directory is forbidden")
			}
		}
	}
	return clean, nil
}

func (p *Provider) readSourceTool() llm.Tool {
	return llm.Tool{
		Name: "tb_read_source",
		Description: "Read a file inside the thinkbot source tree (loader.git_dir, default /app/src) " +
			"to inspect code during self-bootstrapping. path is relative to the source tree root (e.g., agent/bot/llm_factory.go).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the source tree root",
				},
			},
			"required": []string{"path"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tb_read_source: invalid input")
			}
			rel, _ := args["path"].(string)
			if rel == "" {
				return nil, fmt.Errorf("tb_read_source: path is required")
			}
			fp, err := p.safePath(rel, true)
			if err != nil {
				return nil, err
			}
			data, err := os.ReadFile(fp)
			if err != nil {
				return nil, fmt.Errorf("failed to read source file: %w", err)
			}
			return map[string]any{
				"path":    rel,
				"content": string(data),
			}, nil
		}),
	}
}

func (p *Provider) writeSourceTool() llm.Tool {
	return llm.Tool{
		Name: "tb_write_source",
		Description: "Write a file inside the thinkbot source tree (loader.git_dir) to modify code during self-bootstrapping. " +
			"path is relative to the source tree root; content is the full file content (overwrite). " +
			"Call tb_deploy afterwards to build and switch. Writing inside .git is forbidden.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "File path relative to the source tree root",
				},
				"content": map[string]any{
					"type":        "string",
					"description": "Full file content to write",
				},
			},
			"required": []string{"path", "content"},
		},
		Execute: llm.ToolExecuteFunc(func(ctx *llm.ToolExecContext, input any) (any, error) {
			args, ok := input.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("tb_write_source: invalid input")
			}
			rel, _ := args["path"].(string)
			content, _ := args["content"].(string)
			if rel == "" {
				return nil, fmt.Errorf("tb_write_source: path is required")
			}
			fp, err := p.safePath(rel, false)
			if err != nil {
				return nil, err
			}
			if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
				return nil, fmt.Errorf("failed to create parent directory: %w", err)
			}
			if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
				return nil, fmt.Errorf("failed to write source file: %w", err)
			}
			return map[string]any{
				"path":    rel,
				"bytes":   len(content),
				"written": true,
			}, nil
		}),
	}
}
