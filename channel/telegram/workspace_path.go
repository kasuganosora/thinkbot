package telegram

import (
	"context"
	"fmt"
	"path"
	"strings"
)

// workspaceRoot is the bot workspace root as the model sees it (docker
// sandbox: the per-bot container's /data volume). sandbox.validatePath maps
// "/data/x" and "x" to the same file.
const workspaceRoot = "/data"

// workspacePathCandidates returns the paths to try for a model-supplied
// workspace file path, in order.
//
// 2026-09-27: the model wrote "/data/bili_qr.png" in exec, then passed
// "data/bili_qr.png" (the absolute path without its leading slash) to
// telegram_send_document. As a relative path that resolves to
// /data/data/bili_qr.png and failed 5 times in a row. A relative path that
// starts with the root's own name is therefore also tried with that segment
// removed — after the literal path, so a real workspace subdirectory named
// "data" keeps working.
func workspacePathCandidates(p string) []string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	cands := []string{p}
	rel := strings.TrimPrefix(p, "./")
	rootName := strings.TrimPrefix(workspaceRoot, "/") // "data"
	if strings.HasPrefix(rel, rootName+"/") {
		if alt := path.Clean(strings.TrimPrefix(rel, rootName+"/")); alt != "." && alt != p {
			cands = append(cands, alt)
		}
	}
	return cands
}

// readWorkspaceFile reads a workspace file through fileSource, tolerating the
// "data/..." form (see workspacePathCandidates). It returns the path that
// worked.
func (c *TelegramChannel) readWorkspaceFile(ctx context.Context, p string) ([]byte, string, error) {
	var firstErr error
	for _, cand := range workspacePathCandidates(p) {
		data, err := c.fileSource(ctx, c.botID, cand)
		if err == nil {
			return data, cand, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	return nil, "", fmt.Errorf("read workspace file %q failed: %w (paths are relative to the workspace root %s: use \"reports/a.pdf\" or \"%s/reports/a.pdf\", not \"data/reports/a.pdf\")",
		p, firstErr, workspaceRoot, workspaceRoot)
}
