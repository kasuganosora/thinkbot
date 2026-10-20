# memory_vec: embed scheme change and rollback

`hashEmbed` (local character-bigram hash, 256 dims, no embedding model)
normalizes text before hashing: @handles, note/status id references, dates and
times, `[Reply to …]` quote segments, author prefixes, short `【…】` tags and
URLs are stripped. Vectors written by different schemes are not comparable, so
the scheme is versioned.

## Upgrade (automatic)

- `memory_vec_meta(key, value)` stores `embed_version` (currently
  `bigram-norm-v3`).
- On startup `OpenVecIndex` compares it. On mismatch (or a missing table) it
  drops and recreates `memory_vec` (empty), stamps the current version and logs
  `memory_vec_scheme_reset`.
- At app init (`NewBotService` → `storage.StartAppVecBackfill`), **independent
  of bot state** (also when every bot is stopped), one background job refills
  it, writing only `entry_id`s that are missing (no model calls, purely local):
  1. non-expired rows of `tiered_memories`;
  2. active rows of `memory_entries`.

  It runs once per DB per process. When a bot starts later, its own backfills
  (TieredStore for dreaming bots, `SQLiteRepository.StartVecBackfill`) find
  the rows already present.
- `dedup-dryrun -reindex` (offline) rebuilds `memory_vec` from active
  `memory_entries` and stamps the version as well.

### Logs to watch

Plain stdlib line when the scheme check empties the table:

```
INFO memory_vec_scheme_reset table=memory_vec old="bigram-norm-v2" new="bigram-norm-v3" cleared=5895
```

Plain stdlib line when the first `OpenVecIndex` of a process finds several
rows for one `entry_id` (left by the concurrent-backfill bug fixed after
de18e9c); extras are deleted, the lowest rowid is kept:

```
INFO memory_vec_dedup_rows removed=1231 rows_before=3847 distinct_ids=2616
```

All `VecIndex` instances of one database share one write lock, and backfill
runs on one database are serialized (a bot's TieredStore backfill waits for the
app-level run and then finds the rows present).

zap (JSON in production) for each table, `table` = `tiered_memories` or
`memory_entries`:

```
{"level":"info","msg":"memory_vec_backfill_start","table":"tiered_memories","total":2329,"present":0,"missing":2329}
{"level":"info","msg":"memory_vec_backfill_done","table":"tiered_memories","written":2329,"failed":0,"missing":2329,"took":"9.736s"}
{"level":"error","msg":"memory_vec_backfill_error","table":"memory_entries","stage":"upsert","entry_id":"…","err":"…"}
```

- `total`: indexable rows (id and content set; tiered: not expired).
- `present` / `missing`: rows that already have / lack a vector.
- `written`, `failed`, `took`: result of the run. `stage` is `load`, `list`
  or `upsert`; at most 5 per-entry upsert errors are logged per run.

After an upgrade or rollback, `memory_vec` is complete only once both
`memory_vec_backfill_done` lines have appeared with `failed` 0. Rows whose
entries no longer exist (deleted, archived, expired) are not re-created: the
row count after a rebuild is lower than before, which is normal.

`tiered_memories` and `memory_entries` can share an `entry_id` (memory_vec is
keyed by `entry_id` only); such an id counts as `present` for the second table.

## What the old image does (57044ba / master 40b0b8e)

Checked in the code of both revisions (`agent/memory/tier.go`
`startVecBackfill` / `backfillVec`, `agent/bot/dream_setup.go`,
`agent/storage/repository.go`):

- It does **not** know `memory_vec_meta` and never compares schemes. Vectors
  already present stay as they are, whatever scheme wrote them.
- Startup backfill is **per entry, not "only when empty"**: `backfillVec` lists
  the `entry_id`s already in `memory_vec` and upserts every loaded
  `tiered_memories` row whose id is missing.
- That backfill runs **only when a bot starts with dreaming enabled** (the
  TieredStore is created in `NewDreamingBundle` from `StartBot`). With all bots
  stopped nothing is backfilled.
- It covers **only `tiered_memories`**. `memory_entries` vectors are written
  only on Append/Replace (or by `dedup-dryrun -reindex`). Retrieval simply does
  not see entries without a vector via KNN (substring fallback is used only
  when the KNN finds nothing at all).
- The admin "rebuild vectors" job (`RebuildBotVectors`) also covers only
  `tiered_memories`, per bot. The old image logs only
  `tiered_store: vec backfill finished already_present=… written=…`.

## Rollback to the old image

Without action the old image keeps the new-scheme vectors (no crash, but the
old `hashEmbed` queries compare against normalized vectors). To restore
old-scheme vectors, rebuild `memory_vec` with the **old** binary before the old
image starts.

Why one command is enough:

- Old `dedup-dryrun -in-place -reindex` runs `VecIndex.Rebuild`, which does
  `DROP TABLE IF EXISTS memory_vec` and recreates it, then writes every active
  `memory_entries` row with the old hash.
- The old image ignores `memory_vec_meta`. It is still dropped in the same
  command so that a later re-upgrade sees no `embed_version` and rebuilds;
  otherwise the new image would trust old-scheme vectors stamped as current.
- `tiered_memories` vectors are left to the old image's startup backfill (see
  above: only for bots that start with dreaming enabled).

### The helper image

The app images contain **no `sqlite3`**, and images built from 57044ba /
master 40b0b8e do not contain `dedup-dryrun` either (newer images ship
`/app/dedup-dryrun`). The rollback therefore must use a throwaway helper image
built from the 57044ba source. The source is cloned inside the build, so
nothing is written on the host:

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

When building the app image itself, use `docker build --target thinkbot-slim`
for the slim image; a plain `docker build` ends at the last stage (`selfhost`,
with Go toolchain and full source, about 2.1 GB).

### Steps

1. Stop the bot and back up the DB (host path is an example; compose mounts
   `./data/container` at `/app/data`):

   ```sh
   docker compose stop thinkbot
   cp data/container/thinkbot.db data/container/thinkbot.db.bak
   ```

2. Check the owner of the data dir. The command below runs as uid 1000 (the
   `thinkbot` user of the app image) and must be able to write the DB and its
   `-wal`/`-shm` files:

   ```sh
   stat -c %u data/container data/container/thinkbot.db   # expect 1000
   ```

   If it prints another uid, either pass that uid to `--user` (e.g.
   `--user "$(stat -c %u data/container):$(stat -c %g data/container)"`) or
   fix ownership with `sudo chown -R 1000:1000 data/container`.

3. Drop the version table and rebuild `memory_vec` with the old hash in one
   throwaway container (`--rm`, data dir mounted, no LLM calls; the dry-run
   report is discarded). `test -s` guards against a wrong path: `sqlite3`
   would otherwise create an empty DB file there.

   ```sh
   docker run --rm --user 1000:1000 -v "$PWD/data/container:/data" thinkbot-rollback:57044ba \
     sh -c 'test -s /data/thinkbot.db &&
            sqlite3 /data/thinkbot.db "DROP TABLE IF EXISTS memory_vec_meta;" &&
            dedup-dryrun -db /data/thinkbot.db -in-place -all -reindex >/dev/null'
   ```

   A non-zero exit with no output means the DB path was wrong; nothing was
   written.

4. Start the old image. Its startup backfill adds the missing
   `tiered_memories` vectors (old scheme), entry by entry, for bots that start
   with dreaming enabled; watch for
   `tiered_store: vec backfill finished … written=…`.

5. Remove the helper: `docker rmi thinkbot-rollback:57044ba`.

If step 3 is skipped, `tiered_memories` are restored only for entries without
a vector, and `memory_entries` keep new-scheme vectors until rewritten.

Re-upgrading later needs nothing: the new image sees no `embed_version`, logs
`memory_vec_scheme_reset`, clears `memory_vec` and the app-level backfill
refills both tables (watch for the two `memory_vec_backfill_done` lines).
Newer images can also run `/app/dedup-dryrun` directly (copy mode by default;
`-in-place` writes).
