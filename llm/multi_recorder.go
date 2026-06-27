package llm

import "context"

// MultiUsageRecorder fans out RecordUsage calls to multiple UsageRecorder
// instances. Used to simultaneously record to stats.Recorder (stats_usage_daily)
// and pipeline.RunJournalRecorder (run_journal).
type MultiUsageRecorder struct {
	recorders []UsageRecorder
}

// NewMultiUsageRecorder creates a composite recorder. Nil entries are silently
// skipped at record time.
func NewMultiUsageRecorder(recorders ...UsageRecorder) *MultiUsageRecorder {
	return &MultiUsageRecorder{recorders: recorders}
}

// RecordUsage forwards the metric to every underlying recorder.
func (m *MultiUsageRecorder) RecordUsage(ctx context.Context, metric UsageMetric) {
	for _, r := range m.recorders {
		if r != nil {
			r.RecordUsage(ctx, metric)
		}
	}
}
