package api

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/agent/memory"
)

// vecRebuildJob 是一个 bot 最近一次向量重建。同一 bot 同时只跑一个。
type vecRebuildJob struct {
	Running    bool
	Phase      string
	Total      int
	Indexed    int
	Error      string
	StartedAt  time.Time
	FinishedAt time.Time
}

// vecRebuildView 是给页面轮询的进度。没有任务时 phase=idle。
type vecRebuildView struct {
	Running    bool   `json:"running"`
	Phase      string `json:"phase"`
	Total      int    `json:"total"`
	Indexed    int    `json:"indexed"`
	Error      string `json:"error,omitempty"`
	StartedAt  string `json:"startedAt,omitempty"`
	FinishedAt string `json:"finishedAt,omitempty"`
}

func (j *vecRebuildJob) view() vecRebuildView {
	if j == nil {
		return vecRebuildView{Phase: "idle"}
	}
	v := vecRebuildView{
		Running: j.Running,
		Phase:   j.Phase,
		Total:   j.Total,
		Indexed: j.Indexed,
		Error:   j.Error,
	}
	if !j.StartedAt.IsZero() {
		v.StartedAt = j.StartedAt.Format(time.RFC3339)
	}
	if !j.FinishedAt.IsZero() {
		v.FinishedAt = j.FinishedAt.Format(time.RFC3339)
	}
	return v
}

func (s *Server) vecRebuildSnapshot(botID string) vecRebuildView {
	s.vecRebuildMu.Lock()
	defer s.vecRebuildMu.Unlock()
	if s.vecRebuildJobs == nil {
		return vecRebuildView{Phase: "idle"}
	}
	job := s.vecRebuildJobs[botID]
	if job == nil {
		return vecRebuildView{Phase: "idle"}
	}
	return job.view()
}

// claimVecRebuild 占住这个 bot 的重建名额。已经在跑时 started=false，调用方不得再起一个。
func (s *Server) claimVecRebuild(botID string) (vecRebuildView, bool) {
	s.vecRebuildMu.Lock()
	defer s.vecRebuildMu.Unlock()
	if s.vecRebuildJobs == nil {
		s.vecRebuildJobs = map[string]*vecRebuildJob{}
	}
	if cur := s.vecRebuildJobs[botID]; cur != nil && cur.Running {
		return cur.view(), false
	}
	job := &vecRebuildJob{
		Running:   true,
		Phase:     memory.VecRebuildSelect,
		StartedAt: time.Now(),
	}
	s.vecRebuildJobs[botID] = job
	return job.view(), true
}

func (s *Server) noteVecRebuild(botID string, p memory.VecRebuildProgress) {
	s.vecRebuildMu.Lock()
	defer s.vecRebuildMu.Unlock()
	job := s.vecRebuildJobs[botID]
	if job == nil || !job.Running {
		return
	}
	if p.Phase != "" {
		job.Phase = p.Phase
	}
	job.Total = p.Total
	job.Indexed = p.Indexed
}

func (s *Server) finishVecRebuild(botID string, indexed int, err error) {
	s.vecRebuildMu.Lock()
	defer s.vecRebuildMu.Unlock()
	job := s.vecRebuildJobs[botID]
	if job == nil {
		return
	}
	if indexed > job.Indexed {
		job.Indexed = indexed
	}
	job.Running = false
	job.FinishedAt = time.Now()
	if err != nil {
		job.Phase = memory.VecRebuildError
		job.Error = err.Error()
		return
	}
	job.Phase = memory.VecRebuildDone
	job.Error = ""
}

// vecRebuildLiveStore 优先用正在跑的梦境 store（stop 为空，不能关）。
// 按需构建的 bundle 由调用方在任务结束时 stop。
func (s *Server) vecRebuildLiveStore(botID string) (*memory.TieredStore, func(), error) {
	if s.botSvc == nil {
		return nil, nil, nil
	}
	if bundle, ok := s.botSvc.GetDreamingBundle(botID); ok && bundle != nil {
		return bundle.TieredStore, nil, nil
	}
	bundle, err := s.botSvc.BuildDreamingBundleOnDemand(botID)
	if err != nil {
		return nil, nil, err
	}
	if bundle == nil {
		return nil, nil, nil
	}
	return bundle.TieredStore, bundle.Stop, nil
}

// startDreamingVecRebuild 立刻返回。真正的重建在 goroutine 里跑，用 Background，不跟请求一起取消。
func (s *Server) startDreamingVecRebuild(botID string) (vecRebuildView, bool, error) {
	store, stop, err := s.vecRebuildLiveStore(botID)
	if err != nil {
		return vecRebuildView{}, false, err
	}
	view, started := s.claimVecRebuild(botID)
	if !started {
		if stop != nil {
			stop()
		}
		return view, false, nil
	}
	go s.runVecRebuild(botID, store, stop)
	return s.vecRebuildSnapshot(botID), true, nil
}

func (s *Server) runVecRebuild(botID string, store *memory.TieredStore, stop func()) {
	if stop != nil {
		defer stop()
	}
	n, err := memory.RebuildBotVectors(context.Background(), s.db, botID, store, func(p memory.VecRebuildProgress) {
		s.noteVecRebuild(botID, p)
	})
	s.finishVecRebuild(botID, n, err)
	if s.logger == nil {
		return
	}
	if err != nil {
		s.logger.Warnw("rebuild vec failed", "bot_id", botID, "indexed", n, "error", err)
		return
	}
	s.logger.Infow("rebuild vec done", "bot_id", botID, "indexed", n)
}
