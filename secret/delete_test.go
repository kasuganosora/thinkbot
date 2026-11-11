package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ulog "github.com/kasuganosora/thinkbot/util/log"
)

func TestDeleteOwnEntryPersists(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vault.json")
	v, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-a", "svc.token", "value-owned-by-a", "A 的令牌"); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-b", "svc.token", "value-owned-by-b", "B 的令牌"); err != nil {
		t.Fatal(err)
	}
	res, err := v.Delete("bot-a", "svc.token", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != DeleteStatusDeleted || res.Scope != ScopeBot || res.RevealedGlobal {
		t.Fatalf("unexpected result: %+v", res)
	}
	if _, ok := v.Get("bot-a", "svc.token"); ok {
		t.Fatal("own entry still visible after delete")
	}
	reloaded, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := reloaded.Get("bot-a", "svc.token"); ok {
		t.Fatal("deleted entry came back after reload")
	}
	if got, ok := reloaded.Get("bot-b", "svc.token"); !ok || got != "value-owned-by-b" {
		t.Fatalf("other bot's entry damaged: %q %v", got, ok)
	}
}

func TestDeleteGlobalRequiresExplicitConfirm(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "shared.key", "global-shared-value", "全局"); err != nil {
		t.Fatal(err)
	}
	res, err := v.Delete("bot-a", "shared.key", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != DeleteStatusConfirmRequired || res.Scope != ScopeGlobal {
		t.Fatalf("global delete must ask for confirmation: %+v", res)
	}
	if _, ok := v.Get("bot-a", "shared.key"); !ok {
		t.Fatal("global entry was removed without confirmation")
	}
	res, err = v.Delete("bot-a", "shared.key", true)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != DeleteStatusDeleted || res.Scope != ScopeGlobal {
		t.Fatalf("confirmed global delete failed: %+v", res)
	}
	if _, ok := v.Get("bot-a", "shared.key"); ok {
		t.Fatal("global entry still visible after confirmed delete")
	}
}

func TestDeleteOwnEntryRevealsGlobal(t *testing.T) {
	v := newTestVault(t)
	if err := v.Register("", "k", "global-value-long-123", ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-a", "k", "own-value-long-456", ""); err != nil {
		t.Fatal(err)
	}
	res, err := v.Delete("bot-a", "k", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != DeleteStatusDeleted || !res.RevealedGlobal {
		t.Fatalf("shadowed global entry not reported: %+v", res)
	}
	if got, ok := v.Get("bot-a", "k"); !ok || got != "global-value-long-123" {
		t.Fatalf("global entry should become visible again: %q %v", got, ok)
	}
}

func TestDeleteRollsBackOnSaveFailure(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "vault.json")
	v, err := New(p)
	if err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-a", "k", "value-that-must-survive", ""); err != nil {
		t.Fatal(err)
	}
	// 把 save 用的临时路径占成目录，WriteFile 必然失败。
	if err := os.MkdirAll(p+".tmp", 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Delete("bot-a", "k", false); err == nil {
		t.Fatal("expected save error")
	}
	if got, ok := v.Get("bot-a", "k"); !ok || got != "value-that-must-survive" {
		t.Fatalf("entry lost after failed delete: %q %v", got, ok)
	}
}

func TestDeleteValidationAndNotFound(t *testing.T) {
	v := newTestVault(t)
	for _, name := range []string{"", "  ", "bad name", "{{secret:x}}"} {
		if _, err := v.Delete("bot-a", name, false); err != ErrInvalidName {
			t.Fatalf("name %q should be rejected with ErrInvalidName, got %v", name, err)
		}
	}
	res, err := v.Delete("bot-a", "missing", false)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != DeleteStatusNotFound {
		t.Fatalf("missing entry = %+v", res)
	}
	broken := &Vault{entries: map[entryKey]Entry{}, loadErr: os.ErrPermission}
	if _, err := broken.Delete("bot-a", "k", false); err != ErrUnavailable {
		t.Fatalf("delete on failed vault = %v, want ErrUnavailable", err)
	}
}

func TestDeleteKeepsMaskingWhileAnotherEntryUsesTheSameValue(t *testing.T) {
	v := newTestVault(t)
	const shared = "shared-plaintext-credential"
	if err := v.Register("bot-a", "k1", shared, ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot-b", "k2", shared, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Delete("bot-a", "k1", false); err != nil {
		t.Fatal(err)
	}
	if got := ulog.RedactSecrets("probe " + shared); !strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("masking dropped while another entry still uses the value: %q", got)
	}
	if _, err := v.Delete("bot-b", "k2", false); err != nil {
		t.Fatal(err)
	}
	if got := ulog.RedactSecrets("probe " + shared); strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("masking leaked after the last owner was deleted: %q", got)
	}
}

func TestRegisterSameValueTwiceDoesNotLeakMaskRef(t *testing.T) {
	v := newTestVault(t)
	const value = "repeated-credential-value"
	if err := v.Register("bot", "k", value, ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot", "k", value, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := v.Delete("bot", "k", false); err != nil {
		t.Fatal(err)
	}
	if got := ulog.RedactSecrets("probe " + value); strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("overwriting with the same value leaked a mask reference: %q", got)
	}
}

func TestDeleteAndOverwriteReleaseLogMasking(t *testing.T) {
	v := newTestVault(t)
	const deleted = "delete-mask-probe-value"
	if err := v.Register("bot", "k", deleted, ""); err != nil {
		t.Fatal(err)
	}
	if got := ulog.RedactSecrets("probe " + deleted); !strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("registered value not masked in logs: %q", got)
	}
	if _, err := v.Delete("bot", "k", false); err != nil {
		t.Fatal(err)
	}
	if got := ulog.RedactSecrets("probe " + deleted); strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("deleted value still masks logs: %q", got)
	}

	const oldValue = "overwrite-old-probe-value"
	const newValue = "overwrite-new-probe-value"
	if err := v.Register("bot", "k", oldValue, ""); err != nil {
		t.Fatal(err)
	}
	if err := v.Register("bot", "k", newValue, ""); err != nil {
		t.Fatal(err)
	}
	if got := ulog.RedactSecrets("probe " + newValue); !strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("new value not masked: %q", got)
	}
	if got := ulog.RedactSecrets("probe " + oldValue); strings.Contains(got, ulog.RedactedPlaceholder) {
		t.Fatalf("overwritten value still masks logs: %q", got)
	}
}
