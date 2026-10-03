package api

import (
	"errors"
	"testing"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

func TestClaimVecRebuildWhileRunningIsNoop(t *testing.T) {
	s := &Server{}
	first, started := s.claimVecRebuild("bot")
	if !started || !first.Running || first.Phase != memory.VecRebuildSelect {
		t.Fatalf("first=%+v started=%v", first, started)
	}
	second, started := s.claimVecRebuild("bot")
	if started {
		t.Fatal("second start should be a no-op")
	}
	if !second.Running || second.Phase != memory.VecRebuildSelect {
		t.Fatalf("second=%+v", second)
	}
	s.noteVecRebuild("bot", memory.VecRebuildProgress{Phase: memory.VecRebuildWrite, Total: 3, Indexed: 2})
	mid := s.vecRebuildSnapshot("bot")
	if !mid.Running || mid.Indexed != 2 || mid.Total != 3 || mid.Phase != memory.VecRebuildWrite {
		t.Fatalf("mid=%+v", mid)
	}
	s.finishVecRebuild("bot", 3, nil)
	done := s.vecRebuildSnapshot("bot")
	if done.Running || done.Phase != memory.VecRebuildDone || done.Indexed != 3 {
		t.Fatalf("done=%+v", done)
	}
	idle := s.vecRebuildSnapshot("missing")
	if idle.Running || idle.Phase != "idle" || idle.Total != 0 || idle.Indexed != 0 {
		t.Fatalf("idle=%+v", idle)
	}
	if _, started := s.claimVecRebuild("bot"); !started {
		t.Fatal("finished job should allow a new start")
	}
	s.finishVecRebuild("bot", 1, errors.New("boom"))
	failed := s.vecRebuildSnapshot("bot")
	if failed.Running || failed.Phase != memory.VecRebuildError || failed.Error != "boom" {
		t.Fatalf("failed=%+v", failed)
	}
}
