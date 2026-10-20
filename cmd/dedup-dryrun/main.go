// Command dedup-dryrun prints the groups the memory_dedup pass would send to
// the LLM for a thinkbot database, without any LLM or network call. Use it to
// validate memory.dedup.neighbour_min_similarity (default 0.45) against real
// sqlite-vec data offline.
//
// By default the database (and its -wal/-shm files) is copied to a temp dir
// first, because migrations, memory_vec and -reindex write to the DB:
//
//	THINKBOT_SQLITE_VEC=/usr/lib/sqlite-vec/vec0 \
//	  go run ./cmd/dedup-dryrun -db thinkbot.db -all -reindex -min-sim 0.45
//
// Without a loadable sqlite-vec it reports mode=recent (the fallback the
// production pass would use).
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kasuganosora/thinkbot/agent/core"
	"github.com/kasuganosora/thinkbot/agent/memory"
	"github.com/kasuganosora/thinkbot/agent/storage"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/db"
	gormlogger "gorm.io/gorm/logger"
)

func main() {
	dbPath := flag.String("db", "", "thinkbot sqlite database (required)")
	inPlace := flag.Bool("in-place", false, "open -db directly instead of a temp copy (writes memory_vec!)")
	scopeArg := flag.String("scope", "", "comma-separated scope keys (e.g. channel:misskey:timeline,bot:x); default all")
	listen := flag.String("listen", "misskey:timeline", "comma-separated listen channel ids (skipped like in production)")
	minSim := flag.Float64("min-sim", storage.DefaultDedupMinSimilarity, "neighbour min similarity")
	k := flag.Int("k", storage.DefaultDedupNeighbourTopK, "neighbours per seed")
	maxSeeds := flag.Int("max-seeds", storage.DefaultDedupMaxSeeds, "seeds per scope (-1 = unlimited)")
	maxGroup := flag.Int("max-group", 10, "max entries per group")
	maxChars := flag.Int("max-chars", storage.DefaultDedupMaxMergedChars, "entries longer than this are excluded (oversized)")
	all := flag.Bool("all", false, "use every active entry as seed, ignoring dedup_checked_at")
	reindex := flag.Bool("reindex", false, "rebuild memory_vec from active entries first")
	snippet := flag.Int("snippet", 80, "content snippet length (runes)")
	flag.Parse()
	if *dbPath == "" {
		flag.Usage()
		os.Exit(2)
	}

	path := *dbPath
	if !*inPlace {
		dir, err := os.MkdirTemp("", "dedup-dryrun-")
		if err != nil {
			fail(err)
		}
		defer os.RemoveAll(dir)
		dst := filepath.Join(dir, filepath.Base(path))
		for _, suf := range []string{"", "-wal", "-shm"} {
			if err := copyFile(path+suf, dst+suf); err != nil && !(suf != "" && os.IsNotExist(err)) {
				fail(err)
			}
		}
		path = dst
	}
	for _, id := range splitList(*listen) {
		core.RegisterListenChannel(id)
	}

	gdb, err := db.OpenSQLiteWithLogger(path, gormlogger.Discard)
	if err != nil {
		fail(err)
	}
	// Same schema as a running bot (adds dedup_checked_at + backfill on old DBs).
	if err := dao.Migrate(gdb); err != nil {
		fail(err)
	}
	repo := storage.NewSQLiteRepository(gdb)
	vecState := "vec unavailable (THINKBOT_SQLITE_VEC=" + db.VecPath() + "): fallback mode"
	if repo.Vec().Enabled() {
		vecState = "sqlite-vec enabled (" + db.VecPath() + ")"
	}
	fmt.Printf("db=%s copy=%v %s\nmin_sim=%.2f k=%d max_seeds=%d max_group=%d max_chars=%d all=%v reindex=%v\n\n",
		*dbPath, !*inPlace, vecState, *minSim, *k, *maxSeeds, *maxGroup, *maxChars, *all, *reindex)

	var scopes []memory.Scope
	for _, key := range splitList(*scopeArg) {
		kind, id, _ := strings.Cut(key, ":")
		scopes = append(scopes, memory.Scope{Kind: memory.ScopeKind(kind), ID: id})
	}
	res, err := repo.DryRunDedupGroups(context.Background(), storage.DedupDryRunOptions{
		Scopes: scopes, MinSimilarity: *minSim, TopK: *k, MaxSeeds: *maxSeeds,
		MaxGroup: *maxGroup, MaxMergedChars: *maxChars, AllSeeds: *all, Reindex: *reindex,
	})
	if err != nil {
		fail(err)
	}
	var nGroups, nEntries, nLonely, nOver int
	for _, s := range res {
		fmt.Printf("== scope %s mode=%s seeds=%d groups=%d lonely=%d oversized=%d\n",
			s.Scope.Key(), s.Mode, s.Seeds, len(s.Groups), len(s.Lonely), len(s.Oversized))
		for i, g := range s.Groups {
			fmt.Printf("  group %d (%d entries)\n", i+1, len(g))
			for _, n := range g {
				fmt.Printf("    %-40s sim=%.3f  %s\n", n.Entry.ID, n.Similarity, clip(n.Entry.Content, *snippet))
			}
			nEntries += len(g)
		}
		nGroups += len(s.Groups)
		nLonely += len(s.Lonely)
		nOver += len(s.Oversized)
	}
	fmt.Printf("\nTOTAL scopes=%d groups(llm_calls)=%d grouped_entries=%d lonely=%d oversized=%d\n",
		len(res), nGroups, nEntries, nLonely, nOver)
}

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "dedup-dryrun:", err)
	os.Exit(1)
}
