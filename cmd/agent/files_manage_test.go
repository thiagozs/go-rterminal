package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

func TestFilesRm(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "a", "b") // deep enough not to be a protected top-level folder
	_ = os.MkdirAll(work, 0o755)
	_ = os.WriteFile(filepath.Join(work, "f.txt"), []byte("x"), 0o644)
	_ = os.MkdirAll(filepath.Join(work, "full", "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(work, "full", "sub", "g.txt"), nil, 0o644)
	_ = os.Mkdir(filepath.Join(work, "empty"), 0o755)
	outside := filepath.Join(root, "outside")
	_ = os.MkdirAll(outside, 0o755)
	_ = os.WriteFile(filepath.Join(outside, "keep.txt"), []byte("keep"), 0o644)
	_ = os.Symlink(outside, filepath.Join(work, "link"))
	c := newFileClient(t)

	rm := func(path string, recursive bool) protocol.FileMessage {
		c.send(protocol.FileMessage{Op: protocol.OpRm, Path: path, Recursive: recursive})
		return c.reply()
	}
	if m := rm(filepath.Join(work, "f.txt"), false); m.Op != protocol.OpDone {
		t.Fatalf("rm file = %+v", m)
	}
	if _, err := os.Stat(filepath.Join(work, "f.txt")); err == nil {
		t.Fatal("file still there")
	}
	if m := rm(filepath.Join(work, "empty"), false); m.Op != protocol.OpDone {
		t.Fatalf("rm empty folder = %+v", m)
	}
	// A folder with content needs the explicit recursive flag.
	if m := rm(filepath.Join(work, "full"), false); m.Op != protocol.OpError || !strings.Contains(m.Error, "não está vazia") {
		t.Fatalf("rm non-empty folder = %+v", m)
	}
	if _, err := os.Stat(filepath.Join(work, "full", "sub", "g.txt")); err != nil {
		t.Fatal("a non-recursive rm removed content")
	}
	if m := rm(filepath.Join(work, "full"), true); m.Op != protocol.OpDone {
		t.Fatalf("rm -r = %+v", m)
	}
	// A symbolic link is removed, and what it points to is left alone.
	if m := rm(filepath.Join(work, "link"), true); m.Op != protocol.OpDone {
		t.Fatalf("rm link = %+v", m)
	}
	if _, err := os.Stat(filepath.Join(outside, "keep.txt")); err != nil {
		t.Fatal("rm followed a symbolic link and deleted its target")
	}
	if m := rm(filepath.Join(work, "missing"), false); m.Op != protocol.OpError || !strings.Contains(m.Error, "não existe") {
		t.Fatalf("rm missing = %+v", m)
	}
	if m := rm("relative", false); m.Op != protocol.OpError {
		t.Fatalf("relative path = %+v", m)
	}
}

func TestFilesRmRefusesSystemFolders(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	for _, p := range []string{"/", "/etc", "/usr", "/home", filepath.Dir(home), home} {
		if !protectedPath(p, home) {
			t.Errorf("%s must be protected", p)
		}
	}
	for _, p := range []string{filepath.Join(home, "docs"), "/var/log/app", filepath.Join(t.TempDir(), "x")} {
		if protectedPath(p, home) {
			t.Errorf("%s should not be protected", p)
		}
	}
	c := newFileClientHome(t, home)
	for _, p := range []string{"/", "/etc", home} {
		c.send(protocol.FileMessage{Op: protocol.OpRm, Path: p, Recursive: true})
		c.wantError("protegida")
	}
	if _, err := os.Stat(home); err != nil {
		t.Fatal("the home folder was removed")
	}
}

func TestFilesMv(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "w")
	_ = os.MkdirAll(filepath.Join(dir, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "a.txt"), []byte("A"), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("B"), 0o644)
	c := newFileClient(t)
	mv := func(from, to string) protocol.FileMessage {
		c.send(protocol.FileMessage{Op: protocol.OpMv, Path: from, To: to})
		return c.reply()
	}
	if m := mv(filepath.Join(dir, "a.txt"), filepath.Join(dir, "renamed.txt")); m.Op != protocol.OpDone {
		t.Fatalf("rename = %+v", m)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "renamed.txt")); string(got) != "A" {
		t.Fatalf("content = %q", got)
	}
	// A move into another folder works too.
	if m := mv(filepath.Join(dir, "renamed.txt"), filepath.Join(dir, "sub", "renamed.txt")); m.Op != protocol.OpDone {
		t.Fatalf("move = %+v", m)
	}
	// Nothing is ever replaced.
	if m := mv(filepath.Join(dir, "b.txt"), filepath.Join(dir, "sub", "renamed.txt")); m.Op != protocol.OpError || m.Code != protocol.CodeExists {
		t.Fatalf("rename onto an existing file = %+v, want code %q", m, protocol.CodeExists)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "sub", "renamed.txt")); string(got) != "A" {
		t.Fatal("the existing file was replaced")
	}
	for name, args := range map[string][2]string{
		"same name":      {filepath.Join(dir, "b.txt"), filepath.Join(dir, "b.txt")},
		"into itself":    {filepath.Join(dir, "sub"), filepath.Join(dir, "sub", "in")},
		"missing source": {filepath.Join(dir, "nope"), filepath.Join(dir, "x")},
		"missing parent": {filepath.Join(dir, "b.txt"), filepath.Join(dir, "no", "such", "b.txt")},
		"relative":       {"b.txt", "c.txt"},
		"protected":      {"/etc", "/etc2"},
	} {
		if m := mv(args[0], args[1]); m.Op != protocol.OpError {
			t.Errorf("%s = %+v, want an error", name, m)
		}
	}
}

// readZip collects a zip stream sent for a zip request.
func readZip(t *testing.T, c *fileClient, paths ...string) (map[string][]byte, map[string]os.FileMode, protocol.FileMessage) {
	t.Helper()
	c.send(protocol.FileMessage{Op: protocol.OpZip, Paths: paths})
	if m := c.reply(); m.Op != protocol.OpZip {
		t.Fatalf("zip start = %+v", m)
	}
	var data []byte
	chunks := 0
	var end protocol.FileMessage
	for {
		kind, frame := c.frame()
		if kind == websocket.BinaryMessage {
			data = append(data, frame...)
			chunks++
			continue
		}
		_ = jsonUnmarshal(frame, &end)
		break
	}
	if end.Op != protocol.OpDone || end.Bytes != int64(len(data)) {
		t.Fatalf("end = %+v for %d bytes", end, len(data))
	}
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("not a valid zip: %v", err)
	}
	files := map[string][]byte{}
	modes := map[string]os.FileMode{}
	for _, f := range zr.File {
		modes[f.Name] = f.Mode()
		if strings.HasSuffix(f.Name, "/") {
			files[f.Name] = nil
			continue
		}
		rc, _ := f.Open()
		body, _ := io.ReadAll(rc)
		rc.Close()
		files[f.Name] = body
	}
	return files, modes, end
}

func TestFilesZip(t *testing.T) {
	root := t.TempDir()
	proj := filepath.Join(root, "proj")
	_ = os.MkdirAll(filepath.Join(proj, "docs", "img"), 0o755)
	_ = os.MkdirAll(filepath.Join(proj, "vazia"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "README.md"), []byte("leia-me"), 0o644)
	_ = os.WriteFile(filepath.Join(proj, ".env"), []byte("SEGREDO=1"), 0o600) // hidden files go too
	_ = os.WriteFile(filepath.Join(proj, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(proj, "relatório final.txt"), []byte("acentuado"), 0o644)
	big := bytes.Repeat([]byte("0123456789"), 40000) // 400 KB: several frames, compressible
	_ = os.WriteFile(filepath.Join(proj, "docs", "img", "big.bin"), big, 0o644)
	_ = os.Symlink("README.md", filepath.Join(proj, "link"))
	single := filepath.Join(root, "solo.txt")
	_ = os.WriteFile(single, []byte("solo"), 0o644)
	c := newFileClient(t)

	files, modes, end := readZip(t, c, proj, single)
	var names []string
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	want := "proj/,proj/.env,proj/README.md,proj/docs/,proj/docs/img/,proj/docs/img/big.bin,proj/relatório final.txt,proj/run.sh,proj/vazia/,solo.txt"
	if got := strings.Join(names, ","); got != want {
		t.Fatalf("entries =\n%s\nwant\n%s", got, want)
	}
	if !bytes.Equal(files["proj/docs/img/big.bin"], big) || string(files["proj/.env"]) != "SEGREDO=1" || string(files["proj/relatório final.txt"]) != "acentuado" || string(files["solo.txt"]) != "solo" {
		t.Fatal("file contents differ")
	}
	if modes["proj/run.sh"]&0o111 == 0 {
		t.Errorf("the executable bit was lost: %v", modes["proj/run.sh"])
	}
	if modes["proj/.env"].Perm() != 0o600 {
		t.Errorf(".env mode = %v", modes["proj/.env"])
	}
	if end.Skipped != 1 { // the symbolic link
		t.Errorf("skipped = %d, want 1 (the link)", end.Skipped)
	}

	// What cannot be read is left out and counted; the rest still arrives.
	if os.Geteuid() != 0 {
		locked := filepath.Join(proj, "secret.txt")
		_ = os.WriteFile(locked, []byte("no"), 0o000)
		lockedDir := filepath.Join(proj, "locked")
		_ = os.MkdirAll(filepath.Join(lockedDir, "inside"), 0o755)
		_ = os.Chmod(lockedDir, 0o000)
		t.Cleanup(func() { _ = os.Chmod(lockedDir, 0o755) })
		files, _, end = readZip(t, c, proj)
		if _, ok := files["proj/secret.txt"]; ok {
			t.Error("an unreadable file was archived")
		}
		if string(files["proj/README.md"]) != "leia-me" || end.Skipped < 3 {
			t.Errorf("zip with unreadable items: skipped=%d", end.Skipped)
		}
	}

	c.send(protocol.FileMessage{Op: protocol.OpZip})
	c.wantError("1 a 1000")
	c.send(protocol.FileMessage{Op: protocol.OpZip, Paths: []string{filepath.Join(root, "missing")}})
	c.wantError("não existe")
	c.send(protocol.FileMessage{Op: protocol.OpZip, Paths: []string{"relative"}})
	c.wantError("absoluto")
}

func TestFilesPutKeepsTimeAndMode(t *testing.T) {
	dir := t.TempDir()
	c := newFileClient(t)
	put := func(name string, mode uint32, mtime int64) {
		t.Helper()
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: filepath.Join(dir, name), Size: 2, Mode: mode, Mtime: mtime})
		if m := c.reply(); m.Op != protocol.OpReady {
			t.Fatalf("put %s = %+v", name, m)
		}
		_ = c.ws.WriteMessage(websocket.BinaryMessage, []byte("ok"))
		c.send(protocol.FileMessage{Op: protocol.OpEnd})
		if m := c.reply(); m.Op != protocol.OpDone {
			t.Fatalf("put %s end = %+v", name, m)
		}
	}
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	put("script.sh", 0o755, old.Unix())
	st, _ := os.Stat(filepath.Join(dir, "script.sh"))
	if st.Mode().Perm() != 0o755 || !st.ModTime().Equal(old) {
		t.Fatalf("script.sh = %v %v, want 0755 and %v", st.Mode().Perm(), st.ModTime(), old)
	}
	put("plain.txt", 0, 0)
	st, _ = os.Stat(filepath.Join(dir, "plain.txt"))
	if st.Mode().Perm() != 0o644 || time.Since(st.ModTime()) > time.Minute {
		t.Fatalf("plain.txt = %v %v, want 0644 and now", st.Mode().Perm(), st.ModTime())
	}
	// Only permission bits are honoured, and a file is never made unusable for its owner.
	put("suid.sh", 0o4777, 0)
	st, _ = os.Stat(filepath.Join(dir, "suid.sh"))
	if st.Mode()&os.ModeSetuid != 0 || st.Mode().Perm() != 0o777 {
		t.Fatalf("suid.sh = %v, setuid must be ignored", st.Mode())
	}
	put("locked.txt", 0o004, 0)
	st, _ = os.Stat(filepath.Join(dir, "locked.txt"))
	if st.Mode().Perm()&0o600 != 0o600 {
		t.Fatalf("locked.txt = %v, the owner must keep read and write", st.Mode().Perm())
	}
}

func TestFilesCp(t *testing.T) {
	root := t.TempDir()
	src := filepath.Join(root, "src")
	_ = os.MkdirAll(filepath.Join(src, "sub"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	_ = os.WriteFile(filepath.Join(src, "sub", "a.txt"), []byte("hello"), 0o640)
	_ = os.Symlink("run.sh", filepath.Join(src, "link"))
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	_ = os.Chtimes(filepath.Join(src, "sub", "a.txt"), old, old)
	c := newFileClient(t)
	cp := func(from, to string) protocol.FileMessage {
		c.send(protocol.FileMessage{Op: protocol.OpCp, Path: from, To: to})
		return c.reply()
	}

	dst := filepath.Join(root, "dst")
	if m := cp(src, dst); m.Op != protocol.OpDone || m.Skipped != 1 {
		t.Fatalf("cp = %+v (want done, 1 skipped: the symlink)", m)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "sub", "a.txt")); string(b) != "hello" {
		t.Fatalf("copied content = %q", b)
	}
	if st, _ := os.Stat(filepath.Join(dst, "run.sh")); st == nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("run.sh = %v", st)
	}
	if st, _ := os.Stat(filepath.Join(dst, "sub", "a.txt")); st == nil || !st.ModTime().Equal(old) {
		t.Fatalf("mtime not kept: %v", st)
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); err == nil {
		t.Fatal("symlink was copied")
	}
	if m := cp(src, dst); m.Code != protocol.CodeExists {
		t.Fatalf("copy over existing = %+v, want exists", m)
	}
	if m := cp(src, filepath.Join(src, "sub", "x")); m.Op != protocol.OpError {
		t.Fatalf("copy into itself = %+v", m)
	}
	if m := cp(src, filepath.Join(root, "nope", "x")); m.Op != protocol.OpError {
		t.Fatalf("copy to missing folder = %+v", m)
	}
	if m := cp(filepath.Join(root, "missing"), filepath.Join(root, "y")); m.Op != protocol.OpError {
		t.Fatalf("copy of missing source = %+v", m)
	}
}
