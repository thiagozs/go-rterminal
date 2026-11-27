package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/auth"
	"github.com/thiagozs/go-rterminal/internal/endpoint"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/tlsutil"
)

const (
	fileChunk      = 64 << 10
	maxListEntries = 5000
)

func runFileSession(ctx context.Context, config agentConfig, sessionID string) {
	if err := fileSession(ctx, config, sessionID); err != nil {
		log.Printf("[agent] canal de arquivos encerrado | session=%s | err=%v", sessionID, err)
		return
	}
	log.Printf("[agent] canal de arquivos encerrado | session=%s", sessionID)
}

// fileSession joins a session opened for file transfer and serves it until
// either side leaves.
func fileSession(ctx context.Context, config agentConfig, sessionID string) error {
	pendingReleased := false
	releasePending := func() {
		if !pendingReleased {
			pendingReleased = true
			config.pendingSlots.release()
		}
	}
	defer releasePending()

	tlsConfig, err := tlsutil.ClientConfig(config.tlsCA, config.tlsInsecure)
	if err != nil {
		return err
	}
	sessionURL, err := endpoint.URL(config.serverURL, "/session", url.Values{"id": {sessionID}}, true)
	if err != nil {
		return err
	}
	headers := http.Header{}
	auth.SetAuthorization(headers, config.authToken)
	ws, _, err := (&websocket.Dialer{TLSClientConfig: tlsConfig}).DialContext(ctx, sessionURL, headers)
	if err != nil {
		return fmt.Errorf("conectar canal de arquivos: %w", err)
	}
	defer ws.Close()
	ws.SetReadLimit(1 << 20)
	if err := waitForPairing(ctx, ws); err != nil {
		return err
	}
	releasePending()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close() })
	defer stop()
	home := ""
	if config.identity != nil {
		home = config.identity.home // not the agent's own (root's) home
	}
	err = runAsIdentity(config.identity, func() error { return serveFiles(ws, home) })
	closeNormally(ws, "files closed")
	if err != nil && !isExpectedClose(err) {
		return err
	}
	return nil
}

// fileReply sends a text frame of the file protocol.
func fileReply(ws *websocket.Conn, m protocol.FileMessage) error {
	_ = ws.SetWriteDeadline(time.Now().Add(30 * time.Second))
	return ws.WriteJSON(m)
}

func fileError(ws *websocket.Conn, err error) error {
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpError, Error: friendlyFileError(err)})
}

// friendlyFileError turns an *os.PathError into a short sentence in Portuguese.
func friendlyFileError(err error) string {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return "não existe"
	case errors.Is(err, os.ErrPermission):
		return "sem permissão"
	case errors.Is(err, os.ErrExist):
		return "já existe"
	}
	var pe *os.PathError
	if errors.As(err, &pe) {
		return pe.Err.Error()
	}
	return err.Error()
}

// cleanFilePath accepts only absolute paths.
func cleanFilePath(p string) (string, error) {
	if p == "" || strings.ContainsRune(p, 0) {
		return "", errors.New("caminho inválido")
	}
	if !filepath.IsAbs(p) {
		return "", errors.New("o caminho precisa ser absoluto")
	}
	return filepath.Clean(p), nil
}

// serveFiles answers file operations on a paired session until it closes.
// Operations run one at a time. home is where an empty path starts; empty
// means the home folder of the account the agent runs as.
func serveFiles(ws *websocket.Conn, home string) error {
	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if messageType != websocket.TextMessage {
			if err := fileError(ws, errors.New("dados binários fora de um envio")); err != nil {
				return err
			}
			continue
		}
		var m protocol.FileMessage
		if err := json.Unmarshal(data, &m); err != nil {
			if err := fileError(ws, errors.New("mensagem inválida")); err != nil {
				return err
			}
			continue
		}
		switch m.Op {
		case protocol.OpList:
			err = serveList(ws, m, home)
		case protocol.OpGet:
			err = serveGet(ws, m)
		case protocol.OpPut:
			err = servePut(ws, m)
		case protocol.OpMkdir:
			err = serveMkdir(ws, m)
		default:
			err = fileError(ws, fmt.Errorf("operação desconhecida: %q", m.Op))
		}
		if err != nil {
			return err
		}
	}
}

func serveList(ws *websocket.Conn, m protocol.FileMessage, home string) error {
	path := m.Path
	if path == "" {
		path = home
	}
	if path == "" {
		if h, err := os.UserHomeDir(); err == nil {
			path = h
		} else {
			path = "/"
		}
	}
	path, err := cleanFilePath(path)
	if err != nil {
		return fileError(ws, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return fileError(ws, err)
	}
	if !info.IsDir() {
		return fileError(ws, errors.New("não é uma pasta"))
	}
	dirEntries, err := os.ReadDir(path)
	if err != nil {
		return fileError(ws, err)
	}
	entries := make([]protocol.FileEntry, 0, len(dirEntries))
	for _, de := range dirEntries {
		if len(entries) >= maxListEntries {
			break
		}
		li, err := de.Info()
		if err != nil {
			continue // vanished while listing
		}
		entry := protocol.FileEntry{Name: de.Name(), Size: li.Size(), Mode: li.Mode().Perm().String(), ModTime: li.ModTime().Unix()}
		if li.Mode()&os.ModeSymlink != 0 {
			entry.Link = true
			if target, err := os.Stat(filepath.Join(path, de.Name())); err == nil {
				entry.Dir = target.IsDir()
				entry.Size = target.Size()
			}
		} else {
			entry.Dir = li.IsDir()
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Dir != entries[j].Dir {
			return entries[i].Dir
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpList, Path: path, Entries: entries})
}

func serveMkdir(ws *websocket.Conn, m protocol.FileMessage) error {
	path, err := cleanFilePath(m.Path)
	if err != nil {
		return fileError(ws, err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		return fileError(ws, err)
	}
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Path: path})
}

func serveGet(ws *websocket.Conn, m protocol.FileMessage) error {
	path, err := cleanFilePath(m.Path)
	if err != nil {
		return fileError(ws, err)
	}
	f, err := os.Open(path)
	if err != nil {
		return fileError(ws, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fileError(ws, err)
	}
	if !info.Mode().IsRegular() {
		return fileError(ws, errors.New("só arquivos comuns podem ser baixados"))
	}
	if err := fileReply(ws, protocol.FileMessage{Op: protocol.OpGet, Path: path, Size: info.Size()}); err != nil {
		return err
	}
	buf := make([]byte, fileChunk)
	var sent int64
	for {
		n, readErr := f.Read(buf)
		if n > 0 {
			_ = ws.SetWriteDeadline(time.Now().Add(60 * time.Second))
			if err := ws.WriteMessage(websocket.BinaryMessage, buf[:n]); err != nil {
				return err
			}
			sent += int64(n)
		}
		if readErr == io.EOF {
			return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Bytes: sent})
		}
		if readErr != nil {
			return fileError(ws, readErr)
		}
	}
}

// servePut receives a file into a temporary file next to the destination and
// renames it into place only when every byte arrived, so a dropped connection
// never leaves a half-written file under the real name.
func servePut(ws *websocket.Conn, m protocol.FileMessage) error {
	path, err := cleanFilePath(m.Path)
	if err != nil {
		return fileError(ws, err)
	}
	if m.Size < 0 {
		return fileError(ws, errors.New("tamanho inválido"))
	}
	dir, name := filepath.Split(path)
	if name == "" || name == "." || name == ".." {
		return fileError(ws, errors.New("nome de arquivo inválido"))
	}
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		return fileError(ws, errors.New("a pasta de destino não existe"))
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(path); err == nil {
		switch {
		case st.IsDir():
			return fileError(ws, errors.New("o destino é uma pasta"))
		case !st.Mode().IsRegular():
			return fileError(ws, errors.New("o destino não é um arquivo comum"))
		case !m.Overwrite:
			return fileError(ws, os.ErrExist)
		}
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(dir, "."+name+".*.part")
	if err != nil {
		return fileError(ws, err)
	}
	done := false
	defer func() {
		tmp.Close()
		if !done {
			os.Remove(tmp.Name())
		}
	}()
	if err := fileReply(ws, protocol.FileMessage{Op: protocol.OpReady}); err != nil {
		return err
	}
	var got int64
	for {
		messageType, data, err := ws.ReadMessage()
		if err != nil {
			return err
		}
		if messageType == websocket.BinaryMessage {
			got += int64(len(data))
			if got > m.Size {
				_ = fileError(ws, errors.New("chegou mais dados do que o tamanho anunciado"))
				return errors.New("envio maior que o anunciado")
			}
			if _, err := tmp.Write(data); err != nil {
				_ = fileError(ws, err)
				return err
			}
			continue
		}
		var msg protocol.FileMessage
		if json.Unmarshal(data, &msg) != nil || msg.Op != protocol.OpEnd {
			_ = fileError(ws, errors.New("o envio foi interrompido por outra operação"))
			return errors.New("operação inesperada durante o envio")
		}
		if got != m.Size {
			_ = fileError(ws, fmt.Errorf("faltaram dados: recebi %d de %d bytes", got, m.Size))
			return errors.New("envio incompleto")
		}
		break
	}
	if err := tmp.Chmod(mode); err != nil {
		return fileError(ws, err)
	}
	if err := tmp.Sync(); err != nil {
		_ = fileError(ws, err)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = fileError(ws, err)
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		_ = fileError(ws, err)
		return err
	}
	done = true
	return fileReply(ws, protocol.FileMessage{Op: protocol.OpDone, Path: path, Bytes: got})
}
