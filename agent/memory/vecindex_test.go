package memory

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/kasuganosora/thinkbot/db"
)

func TestHashEmbedStableAndNormalized(t *testing.T) {
	a := hashEmbed("只转发经典黑白女仆装")
	b := hashEmbed("只转发经典黑白女仆装")
	if len(a) != vecDims*4 || string(a) != string(b) {
		t.Fatalf("embed not stable: %d", len(a))
	}
	if string(hashEmbed("女仆服偏好")) == string(a) {
		t.Fatal("different text should not hash equal")
	}
}

func TestOpenVecIndexNilWithoutDB(t *testing.T) {
	if OpenVecIndex(nil) != nil {
		t.Fatal("nil db must not enable vec")
	}
}

func TestVecBlobStaysOneBoundValue(t *testing.T) {
	raw := make([]byte, 1024)
	blob := vecBlob(raw)
	rawN, rawSQL := boundInsert(t, raw)
	blobN, blobSQL := boundInsert(t, blob)
	if rawN != 1026 {
		t.Fatalf("raw []byte should expand to 1026 vars, got %d sql=%s", rawN, rawSQL)
	}
	if blobN != 3 {
		t.Fatalf("vecBlob should stay 3 vars, got %d sql=%s", blobN, blobSQL)
	}
	if strings.Count(blobSQL, "?") != 3 {
		t.Fatalf("vecBlob SQL placeholders = %d (%s)", strings.Count(blobSQL, "?"), blobSQL)
	}
}

func boundInsert(t *testing.T, blob any) (int, string) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{DryRun: true, Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	tx := db.Exec("INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)", blob, "bot:b1", "e1")
	if tx.Statement == nil {
		t.Fatal("no statement")
	}
	return len(tx.Statement.Vars), tx.Statement.SQL.String()
}

func requireVecExt(t *testing.T) {
	t.Helper()
	path := db.VecPath()
	if path == "" {
		t.Skip("sqlite-vec path is empty")
	}
	if _, err := os.Stat(path); err != nil {
		if _, err2 := os.Stat(path + ".so"); err2 != nil {
			t.Skipf("sqlite-vec extension not available at %s", path)
		}
	}
}

func openVecDB(t *testing.T) *gorm.DB {
	t.Helper()
	requireVecExt(t)
	gdb, err := db.OpenSQLite(filepath.Join(t.TempDir(), "vec.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		sqlDB, err := gdb.DB()
		if err == nil {
			_ = sqlDB.Close()
		}
	})
	if !db.TryEnableVec(gdb) {
		t.Skip("sqlite-vec did not load")
	}
	return gdb
}

func hitIDs(hits []Hit) []string {
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.EntryID
	}
	return out
}

func TestVecSearchFiltersByScope(t *testing.T) {
	gdb := openVecDB(t)
	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Fatal("OpenVecIndex did not enable sqlite-vec")
	}
	alpha := "only alpha scope likes black tea"
	beta := "only beta scope likes coffee beans"
	idx.Upsert("channel:alpha-long", "e-alpha", alpha)
	idx.Upsert("channel:beta-long", "e-beta", beta)

	hits := idx.Search("channel:alpha-long", alpha, 5)
	if len(hits) != 1 || hits[0].EntryID != "e-alpha" {
		t.Fatalf("alpha hits = %v", hitIDs(hits))
	}
	hits = idx.Search("channel:beta-long", beta, 5)
	if len(hits) != 1 || hits[0].EntryID != "e-beta" {
		t.Fatalf("beta hits = %v", hitIDs(hits))
	}

	again := OpenVecIndex(gdb)
	if !again.Enabled() {
		t.Fatal("second open disabled the index")
	}
	hits = again.Search("channel:alpha-long", alpha, 5)
	if len(hits) != 1 || hits[0].EntryID != "e-alpha" {
		t.Fatalf("second open hits = %v", hitIDs(hits))
	}
}

func TestVecMigratesAuxiliaryScopeKey(t *testing.T) {
	gdb := openVecDB(t)
	if err := gdb.Exec(vecCreateSQLAux(vecTableName)).Error; err != nil {
		t.Fatalf("create old vec table: %v", err)
	}
	alpha := "alpha bot remembers the red notebook"
	beta := "beta bot remembers the blue kettle"
	if err := gdb.Exec(
		"INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)",
		hashEmbed(alpha), "channel:alpha-long", "e-alpha",
	).Error; err != nil {
		t.Fatal(err)
	}
	if err := gdb.Exec(
		"INSERT INTO memory_vec(embedding, scope_key, entry_id) VALUES (?, ?, ?)",
		hashEmbed(beta), "bot:other", "e-beta",
	).Error; err != nil {
		t.Fatal(err)
	}

	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Fatal("migration did not leave a usable index")
	}
	createSQL, ok, err := lookupCreateSQL(gdb, vecTableName)
	if err != nil || !ok {
		t.Fatalf("lookup schema: ok=%v err=%v", ok, err)
	}
	if scopeKeyAuxiliary(createSQL) {
		t.Fatalf("scope_key still auxiliary: %s", createSQL)
	}
	if !strings.Contains(createSQL, "scope_key text") {
		t.Fatalf("scope_key is not a metadata column: %s", createSQL)
	}
	_, side, err := lookupCreateSQL(gdb, vecMigrateTable)
	if err != nil {
		t.Fatal(err)
	}
	if side {
		t.Fatal("migration left memory_vec_migrating behind")
	}
	n, err := idx.vecCount(vecTableName)
	if err != nil || n != 2 {
		t.Fatalf("row count after migration = %d err=%v", n, err)
	}
	hits := idx.Search("channel:alpha-long", alpha, 5)
	if len(hits) != 1 || hits[0].EntryID != "e-alpha" {
		t.Fatalf("migrated alpha hits = %v", hitIDs(hits))
	}
	hits = idx.Search("bot:other", beta, 5)
	if len(hits) != 1 || hits[0].EntryID != "e-beta" {
		t.Fatalf("migrated beta hits = %v", hitIDs(hits))
	}

	again := OpenVecIndex(gdb)
	if !again.Enabled() {
		t.Fatal("second open failed")
	}
	n2, err := again.vecCount(vecTableName)
	if err != nil || n2 != 2 {
		t.Fatalf("second open row count = %d err=%v", n2, err)
	}
	if err := again.DeleteEntryIDs([]string{"e-beta"}); err != nil {
		t.Fatal(err)
	}
	if hits := again.Search("bot:other", beta, 5); len(hits) != 0 {
		t.Fatalf("deleted entry still searchable: %v", hitIDs(hits))
	}
	hits = again.Search("channel:alpha-long", alpha, 5)
	if len(hits) != 1 || hits[0].EntryID != "e-alpha" {
		t.Fatalf("other bot row lost: %v", hitIDs(hits))
	}
}

func TestVecSearchLogsScopeOnError(t *testing.T) {
	gdb := openVecDB(t)
	idx := OpenVecIndex(gdb)
	if !idx.Enabled() {
		t.Fatal("OpenVecIndex did not enable sqlite-vec")
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	if err := sqlDB.Close(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if hits := idx.Search("channel:alpha-long", "query text", 5); hits != nil {
		t.Fatalf("closed db hits = %v", hitIDs(hits))
	}
	got := buf.String()
	if !strings.Contains(got, `memory vec search failed scope_key="channel:alpha-long"`) {
		t.Fatalf("search error was not logged: %q", got)
	}
}
