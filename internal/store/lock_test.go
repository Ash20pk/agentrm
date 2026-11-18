package store

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteThroughSymlinkKeepsLinkAndMode(t *testing.T) {
	dir := t.TempDir()
	dots := filepath.Join(dir, "dotfiles")
	_ = os.MkdirAll(dots, 0o755)
	real := filepath.Join(dots, "zshenv")
	if err := os.WriteFile(real, []byte("old\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, ".zshenv")
	if err := os.Symlink("dotfiles/zshenv", link); err != nil { // relative, as dotfile managers write them
		t.Fatal(err)
	}
	// A chain: a link to the link.
	link2 := filepath.Join(dir, ".zshenv2")
	if err := os.Symlink(link, link2); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(link2, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{link, link2} {
		if fi, err := os.Lstat(l); err != nil || fi.Mode()&os.ModeSymlink == 0 {
			t.Fatalf("%s was replaced by a plain file", l)
		}
	}
	b, _ := os.ReadFile(real)
	fi, _ := os.Stat(real)
	if string(b) != "new\n" || fi.Mode().Perm() != 0o640 {
		t.Fatalf("target not written in place with its mode: %q %v", b, fi.Mode())
	}
	if left, _ := filepath.Glob(filepath.Join(dots, ".zshenv.tmp-*")); len(left) > 0 {
		t.Fatalf("temp files left: %v", left)
	}

	// A new file gets the mode asked for; an existing plain file keeps its own.
	p := filepath.Join(dir, "plain.json")
	if err := WriteAtomic(p, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o600 {
		t.Fatalf("new file mode: %v", fi.Mode())
	}
	_ = os.Chmod(p, 0o644)
	if err := WriteAtomic(p, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(p); fi.Mode().Perm() != 0o644 {
		t.Fatalf("mode changed: %v", fi.Mode())
	}

	// A link to a file that does not exist yet is written through.
	dangling := filepath.Join(dir, ".gemini-settings")
	if err := os.Symlink(filepath.Join(dots, "gemini.json"), dangling); err != nil {
		t.Fatal(err)
	}
	if err := WriteAtomic(dangling, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dots, "gemini.json")); err != nil || string(b) != "{}" {
		t.Fatalf("dangling link target not written: %v", err)
	}
	if fi, err := os.Lstat(dangling); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the dangling link was replaced")
	}
}

func TestDaemonLockIsExclusive(t *testing.T) {
	t.Setenv("AGENTRM_HOME", t.TempDir())
	a, err := LockDaemon()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LockDaemon(); !errors.Is(err, ErrDaemonRunning) {
		t.Fatalf("second lock: %v", err)
	}
	a.Release()
	a.Release() // twice is harmless
	b, err := LockDaemon()
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	b.Release()
}
