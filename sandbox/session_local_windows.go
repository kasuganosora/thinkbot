//go:build windows

package sandbox

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// Windows has no ConPTY support in the first release (docs §12). Keep the
// factory shape identical to the unix build so the registry stays portable.
type localSessionSpawner struct {
	cfg    SessionConfig
	logger *zap.SugaredLogger
	root   string
}

func newLocalSessionSpawner(cfg SessionConfig, logger *zap.SugaredLogger, root string) *localSessionSpawner {
	return &localSessionSpawner{cfg: cfg, logger: logger, root: root}
}

func (s *localSessionSpawner) Spawn(ctx context.Context, botID string, req SpawnRequest) (SessionHandle, error) {
	return nil, ErrSessionUnsupported
}

// keep time imported for the shared config type on this build
var _ = time.Second
