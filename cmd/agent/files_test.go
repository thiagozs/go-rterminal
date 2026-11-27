package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

var fileUpgrader = websocket.Upgrader{}

// fileClient talks to serveFiles over a real WebSocket, like the browser does.
type fileClient struct {
	t  *testing.T
	ws *websocket.Conn
}

func newFileClient(t *testing.T) *fileClient { return newFileClientHome(t, "") }

// newFileClientHome is newFileClient with the folder an empty path starts in.
func newFileClientHome(t *testing.T, home string) *fileClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := fileUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = serveFiles(ws, home)
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ws.Close() })
	return &fileClient{t: t, ws: ws}
}

func (c *fileClient) send(m protocol.FileMessage) {
	c.t.Helper()
	if err := c.ws.WriteJSON(m); err != nil {
		c.t.Fatal(err)
	}
}

func (c *fileClient) frame() (int, []byte) {
	c.t.Helper()
	_ = c.ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	kind, data, err := c.ws.ReadMessage()
	if err != nil {
		c.t.Fatalf("read: %v", err)
	}
	return kind, data
}

func (c *fileClient) reply() protocol.FileMessage {
	c.t.Helper()
	kind, data := c.frame()
	if kind != websocket.TextMessage {
		c.t.Fatalf("expected a text frame, got binary %d bytes", len(data))
	}
	var m protocol.FileMessage
	if err := jsonUnmarshal(data, &m); err != nil {
		c.t.Fatal(err)
	}
	return m
}

func (c *fileClient) wantError(contains string) {
	c.t.Helper()
	m := c.reply()
	if m.Op != protocol.OpError || !strings.Contains(m.Error, contains) {
		c.t.Fatalf("reply = %+v, want an error containing %q", m, contains)
	}
}

func TestFilesList(t *testing.T) {
	dir := t.TempDir()
	_ = os.Mkdir(filepath.Join(dir, "Zeta"), 0o755)
	_ = os.Mkdir(filepath.Join(dir, "alpha"), 0o755)
	_ = os.WriteFile(filepath.Join(dir, "b.txt"), []byte("hello"), 0o600)
	_ = os.WriteFile(filepath.Join(dir, "A.txt"), nil, 0o644)
	_ = os.Symlink(filepath.Join(dir, "alpha"), filepath.Join(dir, "link-to-dir"))
	c := newFileClient(t)

	c.send(protocol.FileMessage{Op: protocol.OpList, Path: dir})
	m := c.reply()
	if m.Op != protocol.OpList || m.Path != dir {
		t.Fatalf("reply = %+v", m)
	}
	var names []string
	for _, e := range m.Entries {
		names = append(names, e.Name)
	}
	if got := strings.Join(names, ","); got != "alpha,link-to-dir,Zeta,A.txt,b.txt" {
		t.Fatalf("order = %s (folders first, then by name ignoring case)", got)
	}
	for _, e := range m.Entries {
		if e.Name == "link-to-dir" && (!e.Link || !e.Dir) {
			t.Errorf("symlink to a folder = %+v", e)
		}
		if e.Name == "b.txt" && (e.Size != 5 || e.Dir || e.ModTime == 0) {
			t.Errorf("b.txt = %+v", e)
		}
	}

	c.send(protocol.FileMessage{Op: protocol.OpList, Path: "relative/dir"})
	c.wantError("absoluto")
	c.send(protocol.FileMessage{Op: protocol.OpList, Path: filepath.Join(dir, "missing")})
	c.wantError("não existe")
	c.send(protocol.FileMessage{Op: protocol.OpList, Path: filepath.Join(dir, "b.txt")})
	c.wantError("não é uma pasta")

	// An empty path starts in the home folder.
	home := t.TempDir()
	t.Setenv("HOME", home)
	c.send(protocol.FileMessage{Op: protocol.OpList})
	if m := c.reply(); m.Op != protocol.OpList || m.Path != home {
		t.Fatalf("home listing = %+v, want %s", m, home)
	}
}

func TestFilesGet(t *testing.T) {
	dir := t.TempDir()
	content := bytes.Repeat([]byte("0123456789"), 30000) // 300 KB: several chunks
	_ = os.WriteFile(filepath.Join(dir, "big.bin"), content, 0o644)
	c := newFileClient(t)

	c.send(protocol.FileMessage{Op: protocol.OpGet, Path: filepath.Join(dir, "big.bin")})
	head := c.reply()
	if head.Op != protocol.OpGet || head.Size != int64(len(content)) {
		t.Fatalf("head = %+v", head)
	}
	var got []byte
	chunks := 0
	for {
		kind, data := c.frame()
		if kind == websocket.BinaryMessage {
			got = append(got, data...)
			chunks++
			continue
		}
		var m protocol.FileMessage
		_ = jsonUnmarshal(data, &m)
		if m.Op != protocol.OpDone || m.Bytes != int64(len(content)) {
			t.Fatalf("end = %+v", m)
		}
		break
	}
	if !bytes.Equal(got, content) || chunks < 4 {
		t.Fatalf("content differs or was not chunked (chunks=%d)", chunks)
	}

	c.send(protocol.FileMessage{Op: protocol.OpGet, Path: dir})
	c.wantError("arquivos comuns")
	c.send(protocol.FileMessage{Op: protocol.OpGet, Path: filepath.Join(dir, "nope")})
	c.wantError("não existe")
	// The channel still works after errors.
	c.send(protocol.FileMessage{Op: protocol.OpList, Path: dir})
	if m := c.reply(); m.Op != protocol.OpList {
		t.Fatalf("after errors = %+v", m)
	}
}

func upload(c *fileClient, path string, content []byte, overwrite bool) protocol.FileMessage {
	c.t.Helper()
	c.send(protocol.FileMessage{Op: protocol.OpPut, Path: path, Size: int64(len(content)), Overwrite: overwrite})
	if m := c.reply(); m.Op != protocol.OpReady {
		return m
	}
	for off := 0; off < len(content); off += fileChunk {
		end := min(off+fileChunk, len(content))
		if err := c.ws.WriteMessage(websocket.BinaryMessage, content[off:end]); err != nil {
			c.t.Fatal(err)
		}
	}
	c.send(protocol.FileMessage{Op: protocol.OpEnd})
	return c.reply()
}

func partFiles(t *testing.T, dir string) []string {
	t.Helper()
	m, _ := filepath.Glob(filepath.Join(dir, ".*.part"))
	return m
}

func TestFilesPut(t *testing.T) {
	dir := t.TempDir()
	c := newFileClient(t)
	target := filepath.Join(dir, "up.bin")
	content := bytes.Repeat([]byte("abc"), 100000)

	if m := upload(c, target, content, false); m.Op != protocol.OpDone || m.Bytes != int64(len(content)) {
		t.Fatalf("upload = %+v", m)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, content) {
		t.Fatal("content differs")
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o644 {
		t.Errorf("new file mode = %v, want 0644", st.Mode().Perm())
	}
	if len(partFiles(t, dir)) != 0 {
		t.Fatal("temporary file left behind")
	}

	// An existing file is not replaced unless asked to, and keeps its mode.
	if m := upload(c, target, []byte("new"), false); m.Op != protocol.OpError || !strings.Contains(m.Error, "já existe") {
		t.Fatalf("overwrite without permission = %+v", m)
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, content) {
		t.Fatal("the file changed although overwrite was refused")
	}
	_ = os.Chmod(target, 0o600)
	if m := upload(c, target, []byte("new"), true); m.Op != protocol.OpDone {
		t.Fatalf("overwrite = %+v", m)
	}
	if got, _ := os.ReadFile(target); string(got) != "new" {
		t.Fatalf("content = %q", got)
	}
	if st, _ := os.Stat(target); st.Mode().Perm() != 0o600 {
		t.Errorf("overwritten file mode = %v, want the original 0600", st.Mode().Perm())
	}

	// An empty file is fine.
	if m := upload(c, filepath.Join(dir, "empty"), nil, false); m.Op != protocol.OpDone {
		t.Fatalf("empty upload = %+v", m)
	}

	// Refusals before any data is accepted.
	for name, path := range map[string]string{
		"folder as target": dir,
		"missing parent":   filepath.Join(dir, "no", "such", "x"),
		"relative path":    "x.txt",
		"dot-dot as name":  dir + "/..",
	} {
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: path, Size: 1})
		if m := c.reply(); m.Op != protocol.OpError {
			t.Errorf("%s: reply = %+v, want an error", name, m)
		}
	}
}

func TestFilesPutFailuresLeaveNothingBehind(t *testing.T) {
	t.Run("fewer bytes than announced", func(t *testing.T) {
		dir := t.TempDir()
		c := newFileClient(t)
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: filepath.Join(dir, "x"), Size: 100})
		c.reply()
		_ = c.ws.WriteMessage(websocket.BinaryMessage, []byte("only ten b"))
		c.send(protocol.FileMessage{Op: protocol.OpEnd})
		c.wantError("faltaram dados")
		waitGone(t, dir)
	})
	t.Run("more bytes than announced", func(t *testing.T) {
		dir := t.TempDir()
		c := newFileClient(t)
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: filepath.Join(dir, "x"), Size: 3})
		c.reply()
		_ = c.ws.WriteMessage(websocket.BinaryMessage, []byte("too many bytes"))
		c.wantError("mais dados")
		waitGone(t, dir)
	})
	t.Run("connection dropped mid-upload", func(t *testing.T) {
		dir := t.TempDir()
		c := newFileClient(t)
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: filepath.Join(dir, "x"), Size: 1000})
		c.reply()
		_ = c.ws.WriteMessage(websocket.BinaryMessage, []byte("partial"))
		c.ws.Close()
		waitGone(t, dir)
	})
	t.Run("another operation in the middle", func(t *testing.T) {
		dir := t.TempDir()
		c := newFileClient(t)
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: filepath.Join(dir, "x"), Size: 10})
		c.reply()
		c.send(protocol.FileMessage{Op: protocol.OpList, Path: dir})
		c.wantError("interrompido")
		waitGone(t, dir)
	})
}

// waitGone checks that neither the file nor its temporary file exist.
func waitGone(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		entries, _ := os.ReadDir(dir)
		if len(entries) == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("files left in %s: %v", dir, entries)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestFilesMkdirAndMisuse(t *testing.T) {
	dir := t.TempDir()
	c := newFileClient(t)
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: filepath.Join(dir, "new")})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("mkdir = %+v", m)
	}
	if st, err := os.Stat(filepath.Join(dir, "new")); err != nil || !st.IsDir() {
		t.Fatal("folder not created")
	}
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: filepath.Join(dir, "new")})
	c.wantError("já existe")

	_ = c.ws.WriteMessage(websocket.BinaryMessage, []byte("stray"))
	c.wantError("binários")
	_ = c.ws.WriteMessage(websocket.TextMessage, []byte("not json"))
	c.wantError("inválida")
	c.send(protocol.FileMessage{Op: "rm", Path: dir})
	c.wantError("desconhecida")
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("an unknown operation must do nothing")
	}
}

func jsonUnmarshal(data []byte, v any) error { return json.Unmarshal(data, v) }

// With a switched user the first listing must open that user's home, not the
// home of the account the agent itself runs as.
func TestFilesStartInTheConfiguredHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", t.TempDir()) // the agent's own home is somewhere else
	c := newFileClientHome(t, home)
	c.send(protocol.FileMessage{Op: protocol.OpList})
	if m := c.reply(); m.Op != protocol.OpList || m.Path != home {
		t.Fatalf("first listing = %+v, want %s", m, home)
	}
	// An explicit path still wins.
	other := t.TempDir()
	c.send(protocol.FileMessage{Op: protocol.OpList, Path: other})
	if m := c.reply(); m.Path != other {
		t.Fatalf("explicit path = %+v", m)
	}
}

func TestFilesPutCreatesTheFolderTree(t *testing.T) {
	dir := t.TempDir()
	c := newFileClient(t)
	target := filepath.Join(dir, "photos", "2026", "trip", "a.txt")

	// Without parents the destination folder must exist.
	if m := upload(c, target, []byte("x"), false); m.Op != protocol.OpError || !strings.Contains(m.Error, "não existe") {
		t.Fatalf("without parents = %+v", m)
	}

	put := func(path string, content []byte, parents bool) protocol.FileMessage {
		c.send(protocol.FileMessage{Op: protocol.OpPut, Path: path, Size: int64(len(content)), Parents: parents})
		if m := c.reply(); m.Op != protocol.OpReady {
			return m
		}
		if len(content) > 0 {
			_ = c.ws.WriteMessage(websocket.BinaryMessage, content)
		}
		c.send(protocol.FileMessage{Op: protocol.OpEnd})
		return c.reply()
	}
	if m := put(target, []byte("hello"), true); m.Op != protocol.OpDone {
		t.Fatalf("with parents = %+v", m)
	}
	if got, _ := os.ReadFile(target); string(got) != "hello" {
		t.Fatalf("content = %q", got)
	}
	if st, err := os.Stat(filepath.Join(dir, "photos", "2026")); err != nil || !st.IsDir() || st.Mode().Perm() != 0o755 {
		t.Fatalf("intermediate folder = %v %v", st, err)
	}
	// A second file in the same tree reuses the folders.
	if m := put(filepath.Join(dir, "photos", "2026", "trip", "b.txt"), nil, true); m.Op != protocol.OpDone {
		t.Fatalf("second file = %+v", m)
	}
	// A path component that is a file is refused, and nothing is left behind.
	if m := put(filepath.Join(target, "inside.txt"), []byte("x"), true); m.Op != protocol.OpError {
		t.Fatalf("a file in the middle of the path = %+v, want an error", m)
	}
	if left := partFiles(t, filepath.Join(dir, "photos", "2026", "trip")); len(left) != 0 {
		t.Fatalf("temporary files left: %v", left)
	}
}

func TestFilesMkdirParents(t *testing.T) {
	dir := t.TempDir()
	c := newFileClient(t)
	deep := filepath.Join(dir, "a", "b", "c")
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: deep})
	c.wantError("não existe")

	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: deep, Parents: true})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("mkdir -p = %+v", m)
	}
	if st, err := os.Stat(deep); err != nil || !st.IsDir() {
		t.Fatal("the whole path must exist")
	}
	// Like mkdir -p, an existing folder is fine, which makes re-sending a tree safe.
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: deep, Parents: true})
	if m := c.reply(); m.Op != protocol.OpDone {
		t.Fatalf("existing folder = %+v", m)
	}
	// An existing file is not.
	file := filepath.Join(dir, "plain")
	_ = os.WriteFile(file, nil, 0o644)
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: file, Parents: true})
	c.wantError("não é uma pasta")
	c.send(protocol.FileMessage{Op: protocol.OpMkdir, Path: filepath.Join(file, "sub"), Parents: true})
	c.wantError("não é uma pasta")
}
