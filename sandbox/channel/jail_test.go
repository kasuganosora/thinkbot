package channel

import (
	"os"
	"path/filepath"
	"testing"
)

func TestJailPathConfinesTraversal(t *testing.T) {
	root := t.TempDir()
	cases := []string{"../etc/passwd", "..", "foo/../../etc/passwd", `foo\..\x`}
	for _, path := range cases {
		if _, err := JailPath(root, path); err == nil {
			t.Errorf("JailPath(%q) succeeded", path)
		}
	}
}

func TestJailPathStripsVirtualRoot(t *testing.T) {
	got, err := JailPath("/data", "/data/foo/bar.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/data/foo/bar.txt" {
		t.Fatalf("got %q", got)
	}
	got, err = JailPath("/host/ws/bot-1", "/data/foo/bar.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/host/ws/bot-1/foo/bar.txt" {
		t.Fatalf("local root: got %q", got)
	}
	got, err = JailPath("/host/ws/bot-1", "/data")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/host/ws/bot-1" {
		t.Fatalf("bare /data: got %q", got)
	}
	got, err = JailPath("/host/ws/bot-1", "/datax/y")
	if err != nil {
		t.Fatal(err)
	}
	if got != "/host/ws/bot-1/datax/y" {
		t.Fatalf("/datax stripped: got %q", got)
	}
}

func TestJailPathRejectsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret"), []byte("nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := JailPath(root, "link"); err == nil {
		t.Fatal("symlink to outside file was accepted")
	}
	if err := os.Mkdir(filepath.Join(outside, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "dir"), filepath.Join(root, "sub")); err != nil {
		t.Fatal(err)
	}
	if _, err := JailPath(root, "sub/file.txt"); err == nil {
		t.Fatal("symlink parent escape was accepted")
	}
}

func TestJailPathAllowsNewFileInside(t *testing.T) {
	root := t.TempDir()
	got, err := JailPath(root, "nested/new.txt")
	if err != nil {
		t.Fatal(err)
	}
	if got != filepath.Join(root, "nested", "new.txt") {
		t.Fatalf("got %q", got)
	}
}
