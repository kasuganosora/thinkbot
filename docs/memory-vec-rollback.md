# memory_vec: embed scheme change and rollback

`hashEmbed` (local character-bigram hash, 256 dims, no embedding model) now
normalizes text before hashing: @mentions, note/status ids, dates/times,
`[Reply to …]` prefixes and URLs are stripped. Vectors written before and after
this change are not comparable, so the scheme is versioned.

## Upgrade (automatic)

- `memory_vec_meta(key, value)` stores `embed_version` (currently
  `bigram-norm-v3`).
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
old-scheme vectors everywhere, rebuild `memory_vec` with the **old** binary
before the old image starts.

Why one command is enough:

- Old `dedup-dryrun -in-place -reindex` runs `VecIndex.Rebuild`, which does
  `DROP TABLE IF EXISTS memory_vec` and recreates it, then writes every active
  `memory_entries` row with the old hash. No separate drop/delete is needed.
- The old image ignores `memory_vec_meta`. It is still dropped in the same
  command so that a later re-upgrade sees no `embed_version` and rebuilds;
  otherwise the new image would trust old-scheme vectors stamped as current.
  It is a plain table, so `sqlite3` drops it without the extension.
- On start, the old image's per-entry backfill then adds the missing
  `tiered_memories` vectors.

### The helper image

Images built from 57044ba / master 40b0b8e do **not** contain `dedup-dryrun`
(only newer images ship `/app/dedup-dryrun`). Build a throwaway helper from the
57044ba source. The source is cloned inside the build, so nothing is written
on the host:

```sh
docker build -t thinkbot-rollback:57044ba - <<'EOF'
FROM golang:1.27-bookworm
ARG SQLITE_VEC_VERSION=0.1.6
RUN apt-get update && apt-get install -y --no-install-recommends sqlite3 \
    && rm -rf /var/lib/apt/lists/*
ADD https://github.com/asg017/sqlite-vec/releases/download/v${SQLITE_VEC_VERSION}/sqlite-vec-${SQLITE_VEC_VERSION}-loadable-linux-x86_64.tar.gz /tmp/vec.tgz
RUN mkdir -p /usr/lib/sqlite-vec && tar -xzf /tmp/vec.tgz -C /usr/lib/sqlite-vec && rm /tmp/vec.tgz
RUN git clone https://github.com/kasuganosora/thinkbot.git /src \
    && cd /src && git checkout 57044ba \
    && CGO_ENABLED=1 go build -ldflags="-s -w" -o /usr/local/bin/dedup-dryrun ./cmd/dedup-dryrun \
    && rm -rf /src /root/go/pkg /root/.cache/go-build
ENV THINKBOT_SQLITE_VEC=/usr/lib/sqlite-vec/vec0
EOF
```

### Steps

1. Stop the bot and back up the DB (host path is an example; compose mounts
   `./data/container` at `/app/data`):

   ```sh
   docker compose stop thinkbot
   cp data/container/thinkbot.db data/container/thinkbot.db.bak
   ```

2. Drop the version table and rebuild `memory_vec` with the old hash in one
   throwaway container (`--rm`, data dir mounted, no LLM calls; the dry-run
   report is discarded):

   ```sh
   docker run --rm --user 1000:1000 -v "$PWD/data/container:/data" thinkbot-rollback:57044ba \
     sh -c 'sqlite3 /data/thinkbot.db "DROP TABLE IF EXISTS memory_vec_meta;" &&
            dedup-dryrun -db /data/thinkbot.db -in-place -all -reindex >/dev/null'
   ```

3. Start the old image. Its startup backfill adds the missing
   `tiered_memories` vectors (old scheme), entry by entry.

4. Remove the helper: `docker rmi thinkbot-rollback:57044ba`.

If step 2 is skipped, `tiered_memories` are restored only for entries without
a vector, and `memory_entries` keep new-scheme vectors until rewritten.

Re-upgrading later needs nothing: the new image sees no `embed_version`,
clears `memory_vec` and refills both tables. Newer images can also run
`/app/dedup-dryrun` directly (copy mode by default; `-in-place` writes).
