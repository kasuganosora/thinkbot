//go:build unix

package channel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenNoFollowRejectsSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if _, err := openFileNoFollow(root, link); err == nil {
		t.Fatal("opened a symlink")
	}
	got, err := os.ReadFile(secret)
	if err != nil || string(got) != "nope" {
		t.Fatalf("outside file changed: %q %v", got, err)
	}
}

func TestRemoveNoFollowDoesNotEscapeParentSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	secret := filepath.Join(outside, "secret")
	if err := os.WriteFile(secret, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	err := removeNoFollow(root, filepath.Join(root, "sub", "secret"), false)
	if err == nil {
		t.Fatal("delete through a parent symlink succeeded")
	}
	got, rerr := os.ReadFile(secret)
	if rerr != nil || string(got) != "keep" {
		t.Fatalf("outside file = %q %v", got, rerr)
	}
}

func TestRemoveNoFollowRecursiveInsideRoot(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := removeNoFollow(root, dir, true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("dir still present: %v", err)
	}
}
