package dao

// messageCountRecount2Name re-runs the full per-session recount after the
// counter-drift fix (2026-10-20).
//
// Background: before the fix, /compact deleted k rows without decrementing
// message_count and inserted its summary without incrementing it (net +k-1
// per compaction), and /clear deleted rows without resetting the counter.
// E.g. session 22 recorded 195 vs 191 actual rows. Runtime writes now derive
// the counter from rows (RecountSession); this migration repairs the existing
// drift once. It reuses recountSessionStats, which is UPDATE-only and
// idempotent (re-running yields the same values).
const messageCountRecount2Name = "2026-10-20-message-count-recount"
