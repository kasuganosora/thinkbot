package memory

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
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
