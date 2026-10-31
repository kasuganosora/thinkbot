package api

import (
	"context"

	"github.com/kasuganosora/thinkbot/util/errs"
)

type botStartAttempt struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// reserveBotStartLocked installs an identity distinct from the nil placeholder.
// Caller holds s.mu; stop and publication use the same lock.
func (s *BotService) reserveBotStartLocked(ctx context.Context, id string) *botStartAttempt {
	startupCtx, cancel := context.WithCancel(ctx)
	attempt := &botStartAttempt{ctx: startupCtx, cancel: cancel}
	if s.botStarts == nil {
		s.botStarts = make(map[string]*botStartAttempt)
	}
	s.botStarts[id] = attempt
	s.botInstances[id] = nil
	return attempt
}

func (s *BotService) rollbackBotStart(id string, attempt *botStartAttempt) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.botStarts[id] != attempt {
		return
	}
	delete(s.botStarts, id)
	if s.botInstances[id] == nil {
		delete(s.botInstances, id)
	}
}

// publishBotStart is the cancellation/publication linearization point. Checking
// after Ready is essential: Ready and cancellation can both win a select.
func (s *BotService) publishBotStart(id string, attempt *botStartAttempt, publish func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := attempt.ctx.Err(); err != nil {
		return err
	}
	if s.botStarts[id] != attempt {
		return errs.Conflict("bot startup was superseded or stopped")
	}
	if err := publish(); err != nil {
		return err
	}
	delete(s.botStarts, id)
	return nil
}
