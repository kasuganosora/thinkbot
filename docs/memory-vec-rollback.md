# memory_vec: embed scheme change and rollback

`hashEmbed` (local character-bigram hash, 256 dims, no embedding model) now
normalizes text before hashing: @mentions, note/status ids, dates/times,
`[Reply to …]` prefixes and URLs are stripped. Vectors written before and after
this change are not comparable, so the scheme is versioned.

## Upgrade (automatic)

- `memory_vec_meta(key, value)` stores `embed_version` (currently
  `bigram-norm-v2`).
- On startup `OpenVecIndex` compares it. On mismatch (or a missing table) it
  drops and recreates `memory_vec` (empty) and stamps the current version.
- Two background backfills then refill it, writing only `entry_id`s that are
  missing (no model calls, purely local):
  - `TieredStore` backfill: non-expired rows of `tiered_memories`.
  - `SQLiteRepository.StartVecBackfill`: active rows of `memory_entries`
    (new; once per DB per process).
- `dedup-dryrun -reindex` (offline) rebuilds `memory_vec` from active
  `memory_entries` and stamps the version as well.

## What the old image does (57044ba / master 40b0b8e)

Checked in the code of both revisions (`agent/memory/tier.go`
`startVecBackfill` / `backfillVec`, `agent/storage/repository.go`):

- It does **not** know `memory_vec_meta` and never compares schemes. Vectors
  already present stay as they are, whatever scheme wrote them.
- Startup backfill is **per entry, not "only when empty"**: `backfillVec` lists
  the `entry_id`s already in `memory_vec` and upserts every loaded
  `tiered_memories` row whose id is missing. An empty table therefore gets all
  tiered rows; a partially filled one gets only the missing ones.
- It covers **only `tiered_memories`**. `memory_entries` vectors are written
  only on Append/Replace (or by `dedup-dryrun -reindex`); missing
  `memory_entries` vectors are never backfilled at startup. Retrieval then
  simply does not see those entries via KNN (substring fallback is used only
  when the KNN finds nothing at all).
- The admin "rebuild vectors" job (`RebuildBotVectors`) also covers only
  `tiered_memories`, per bot.

## Rollback to the old image

Without action the old image keeps the new-scheme vectors (no crash, but the
old `hashEmbed` queries compare against normalized vectors). To restore
old-scheme vectors everywhere:

1. Stop the bot and back up the DB (paths are examples):

   ```sh
   cp data/thinkbot.db data/thinkbot.db.bak
   ```

2. Drop the version table and the vector table. `memory_vec` is a vec0 virtual
   table, so the extension must be loaded to drop it:

   ```sh
   sqlite3 data/thinkbot.db \
     ".load /usr/lib/sqlite-vec/vec0" \
     "DROP TABLE IF EXISTS memory_vec_meta;" \
     "DROP TABLE IF EXISTS memory_vec;"
   ```

   (`DELETE FROM memory_vec;` instead of the DROP works too.)

3. Rebuild `memory_entries` vectors with the **old** hash, offline, before the
   old image starts (old `-reindex` drops and recreates the whole table, so it
   must run first):

   ```sh
   git checkout 57044ba
   THINKBOT_SQLITE_VEC=/usr/lib/sqlite-vec/vec0 \
     go run ./cmd/dedup-dryrun -db data/thinkbot.db -in-place -all -reindex
   ```

   This makes no LLM calls; the dry-run report it prints can be ignored.

4. Start the old image. Its startup backfill adds the missing
   `tiered_memories` vectors (old scheme), entry by entry.

If step 3 is skipped, `tiered_memories` are fully restored by step 4, and
`memory_entries` vectors come back only as entries are rewritten.

Re-upgrading later needs nothing: the new image sees no `embed_version`,
clears `memory_vec` and refills both tables.
