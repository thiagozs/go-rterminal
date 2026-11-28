package main

import (
	"archive/zip"
	"compress/flate"
	"errors"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
)

const maxZipPaths = 1000

// protectedPath reports whether the panel must refuse to delete or move a
// path: the root, any top-level folder (/etc, /usr, /home...) and the home
// folder of the account in use together with everything above it.
func protectedPath(path, home string) bool {
	path = filepath.Clean(path)
	if path == "/" || filepath.Dir(path) == "/" {
		return true
	}
	homes := []string{home}
	if u, err := user.Current(); err == nil {
		homes = append(homes, u.HomeDir)
	}
	for _, h := range homes {
		if h == "" {
			continue
		}
		h = filepath.Clean(h)
		if path == h || strings.HasPrefix(h, path+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

var errProtected = errors.New("esta pasta é protegida e não pode ser apagada ou movida pelo painel")

// serveRm removes a file, an empty folder or, with recursive, a folder and
// everything in it. Symbolic links are removed, never followed.
func serveRm(ws *websocket.Conn, m protocol.FileMessage, home string) error {
	path, err := cleanFilePath(m.Path)
	if err != nil {
		return fileError(ws, err)
	}
	if protectedPath(path, home) {
		return fileError(ws, errProtected)
	}
	info, err := os.Lstat(path)
	if err != nil {
		return fileError(ws, err)
	}
	if info.IsDir() && m.Recursive {
		err = os.RemoveAll(path)
	} else {
		err = os.Remove(path)
	}
	if err != nil {
		return fileError(ws, err)
	}
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Path: path})
}

// serveMv renames or moves a path inside one file system. It never replaces
// something that already exists.
func serveMv(ws *websocket.Conn, m protocol.FileMessage, home string) error {
	from, err := cleanFilePath(m.Path)
	if err != nil {
		return fileError(ws, err)
	}
	to, err := cleanFilePath(m.To)
	if err != nil {
		return fileError(ws, err)
	}
	switch {
	case protectedPath(from, home):
		return fileError(ws, errProtected)
	case from == to:
		return fileError(ws, errors.New("o nome é o mesmo"))
	case strings.HasPrefix(to, from+string(filepath.Separator)):
		return fileError(ws, errors.New("não dá para mover uma pasta para dentro dela mesma"))
	}
	if _, err := os.Lstat(from); err != nil {
		return fileError(ws, err)
	}
	if _, err := os.Lstat(to); err == nil {
		return fileError(ws, os.ErrExist)
	}
	if st, err := os.Stat(filepath.Dir(to)); err != nil || !st.IsDir() {
		return fileError(ws, errors.New("a pasta de destino não existe"))
	}
	if err := os.Rename(from, to); err != nil {
		return fileError(ws, err)
	}
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Path: to})
}

// chunkWriter sends what is written to it as binary frames of at most
// fileChunk bytes and counts them.
type chunkWriter struct {
	ws    *websocket.Conn
	buf   []byte
	total int64
}

func (c *chunkWriter) Write(p []byte) (int, error) {
	written := len(p)
	for len(p) > 0 {
		room := fileChunk - len(c.buf)
		if room > len(p) {
			room = len(p)
		}
		c.buf = append(c.buf, p[:room]...)
		p = p[room:]
		if len(c.buf) >= fileChunk {
			if err := c.flush(); err != nil {
				return 0, err
			}
		}
	}
	return written, nil
}

func (c *chunkWriter) flush() error {
	if len(c.buf) == 0 {
		return nil
	}
	_ = c.ws.SetWriteDeadline(time.Now().Add(60 * time.Second))
	if err := c.ws.WriteMessage(websocket.BinaryMessage, c.buf); err != nil {
		return err
	}
	c.total += int64(len(c.buf))
	c.buf = c.buf[:0]
	return nil
}

// serveZip streams a ZIP archive of the named files and folders. The archive
// is built while it is sent, so nothing is held in memory or on disk. Entries
// keep their modification time and permission bits, and empty folders are kept.
// Anything that cannot be read (permissions, symbolic links, special files)
// is left out and counted.
func serveZip(ws *websocket.Conn, m protocol.FileMessage) error {
	if len(m.Paths) == 0 || len(m.Paths) > maxZipPaths {
		return fileError(ws, errors.New("escolha de 1 a 1000 itens"))
	}
	roots := make([]string, 0, len(m.Paths))
	for _, p := range m.Paths {
		path, err := cleanFilePath(p)
		if err != nil {
			return fileError(ws, err)
		}
		if _, err := os.Lstat(path); err != nil {
			return fileError(ws, err)
		}
		roots = append(roots, path)
	}
	if err := fileReply(ws, protocol.FileMessage{Op: protocol.OpZip}); err != nil {
		return err
	}
	out := &chunkWriter{ws: ws, buf: make([]byte, 0, fileChunk)}
	zw := zip.NewWriter(out)
	// Speed over size: the agent may be a small board.
	zw.RegisterCompressor(zip.Deflate, func(w io.Writer) (io.WriteCloser, error) { return flate.NewWriter(w, flate.BestSpeed) })
	skipped := 0

	addFile := func(name, full string, info fs.FileInfo) error {
		f, err := os.Open(full)
		if err != nil {
			skipped++
			return nil
		}
		defer f.Close()
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			skipped++
			return nil
		}
		header.Name = name
		header.Method = zip.Deflate
		w, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if _, err := io.Copy(w, f); err != nil {
			var pathErr *fs.PathError
			if errors.As(err, &pathErr) { // the file could not be read: this entry is already started, so it ends here
				skipped++
				return nil
			}
			return err // the connection is the problem
		}
		return nil
	}

	for _, root := range roots {
		base := filepath.Base(root)
		walkErr := filepath.WalkDir(root, func(full string, d fs.DirEntry, err error) error {
			if err != nil {
				skipped++
				if d != nil && d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			rel, _ := filepath.Rel(root, full)
			name := base
			if rel != "." {
				name = base + "/" + filepath.ToSlash(rel)
			}
			info, err := d.Info()
			if err != nil {
				skipped++
				return nil
			}
			switch {
			case info.IsDir():
				header, err := zip.FileInfoHeader(info)
				if err != nil {
					skipped++
					return nil
				}
				header.Name = name + "/"
				header.Method = zip.Store
				_, err = zw.CreateHeader(header)
				return err
			case info.Mode().IsRegular():
				return addFile(name, full, info)
			default: // symbolic links, sockets, devices
				skipped++
				return nil
			}
		})
		if walkErr != nil {
			return walkErr
		}
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := out.flush(); err != nil {
		return err
	}
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Bytes: out.total, Skipped: skipped})
}

// serveCp copies a file or a folder tree to a new path. It never replaces
// something that already exists. Permission bits and modification times are
// kept; symbolic links and special files are left out and counted.
func serveCp(ws *websocket.Conn, m protocol.FileMessage) error {
	from, err := cleanFilePath(m.Path)
	if err != nil {
		return fileError(ws, err)
	}
	to, err := cleanFilePath(m.To)
	if err != nil {
		return fileError(ws, err)
	}
	switch {
	case from == to || strings.HasPrefix(to, from+string(filepath.Separator)):
		return fileError(ws, errors.New("não dá para copiar uma pasta para dentro dela mesma"))
	}
	info, err := os.Lstat(from)
	if err != nil {
		return fileError(ws, err)
	}
	if _, err := os.Lstat(to); err == nil {
		return fileError(ws, os.ErrExist)
	}
	if st, err := os.Stat(filepath.Dir(to)); err != nil || !st.IsDir() {
		return fileError(ws, errors.New("a pasta de destino não existe"))
	}
	skipped := 0
	var copyTree func(src, dst string, info fs.FileInfo) error
	copyTree = func(src, dst string, info fs.FileInfo) error {
		switch {
		case info.IsDir():
			if err := os.Mkdir(dst, 0o700); err != nil {
				return err
			}
			entries, err := os.ReadDir(src)
			if err != nil {
				return err
			}
			for _, e := range entries {
				ei, err := e.Info()
				if err != nil {
					skipped++
					continue
				}
				if err := copyTree(filepath.Join(src, e.Name()), filepath.Join(dst, e.Name()), ei); err != nil {
					return err
				}
			}
			if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(dst, info.ModTime(), info.ModTime())
		case info.Mode().IsRegular():
			in, err := os.Open(src)
			if err != nil {
				skipped++ // unreadable: leave it out, like zip does
				return nil
			}
			defer in.Close()
			out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, in); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
			if err := os.Chmod(dst, info.Mode().Perm()); err != nil {
				return err
			}
			return os.Chtimes(dst, info.ModTime(), info.ModTime())
		default:
			skipped++
			return nil
		}
	}
	if !info.IsDir() && !info.Mode().IsRegular() {
		return fileError(ws, errors.New("só dá para copiar arquivos e pastas"))
	}
	if err := copyTree(from, to, info); err != nil {
		_ = os.RemoveAll(to) // do not leave a half copy behind
		return fileError(ws, err)
	}
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Path: to, Skipped: skipped})
}
