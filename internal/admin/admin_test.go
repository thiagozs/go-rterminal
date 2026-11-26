package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/protocol"
	"github.com/thiagozs/go-rterminal/internal/relay"
	"github.com/thiagozs/go-rterminal/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func TestMain(m *testing.M) {
	store.BcryptCost = bcrypt.MinCost // set once, before any parallel test runs
	os.Exit(m.Run())
}

type env struct {
	t      *testing.T
	store  *store.Store
	relay  *relay.Server
	srv    *httptest.Server
	client *http.Client
}

func newEnv(t *testing.T, tweaks ...func(*Config)) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	quiet := log.New(io.Discard, "", 0)
	rl := relay.New(relay.Config{
		AgentAuth: func(token string) (string, bool) {
			m, err := st.MachineByToken(token)
			if err != nil {
				return "", false
			}
			return m.ID, true
		},
		OnAgentConnected: func(id string, info protocol.Message) {
			_ = st.TouchMachine(id, info.Hostname, info.OS, info.Arch, info.Version)
		},
		SessionTimeout: 3 * time.Second,
		Logger:         quiet,
	})
	cfg := Config{Store: st, Relay: rl, Logger: quiet}
	for _, tweak := range tweaks {
		tweak(&cfg)
	}
	adm := New(cfg)

	mux := http.NewServeMux()
	for _, p := range []string{"/agent", "/client", "/session", "/healthz"} {
		mux.Handle(p, rl.Handler())
	}
	mux.Handle("/", adm.Handler())
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	return &env{t: t, store: st, relay: rl, srv: srv, client: &http.Client{Jar: jar}}
}

func (e *env) do(method, path string, body any) (int, []byte) {
	e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, e.srv.URL+path, reader)
	req.Header.Set("X-Requested-With", "rterminal")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := e.client.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (e *env) login() {
	e.t.Helper()
	if _, err := e.store.CreateAdmin("root", "correct-horse", false); err != nil {
		e.t.Fatal(err)
	}
	if code, body := e.do("POST", "/api/login", credentials{"root", "correct-horse"}); code != 200 {
		e.t.Fatalf("login = %d %s", code, body)
	}
}

func (e *env) createMachine(name string) machineCreated {
	e.t.Helper()
	code, body := e.do("POST", "/api/machines", machineInput{Name: name})
	if code != http.StatusCreated {
		e.t.Fatalf("create machine = %d %s", code, body)
	}
	var out machineCreated
	if err := json.Unmarshal(body, &out); err != nil {
		e.t.Fatal(err)
	}
	return out
}

func (e *env) wsURL(path string) string {
	return "ws" + strings.TrimPrefix(e.srv.URL, "http") + path
}

func (e *env) connectAgent(credential string, info protocol.Message) *websocket.Conn {
	e.t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(e.wsURL("/agent"), http.Header{"Authorization": {"Bearer " + credential}})
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { conn.Close() })
	info.Type = protocol.TypeRegisterAgent
	if err := conn.WriteJSON(info); err != nil {
		e.t.Fatal(err)
	}
	return conn
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestAPIRequiresLoginAndCSRFHeader(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if code, _ := e.do("GET", "/api/machines", nil); code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list = %d, want 401", code)
	}
	e.login()

	req, _ := http.NewRequest("POST", e.srv.URL+"/api/groups", strings.NewReader(`{"name":"x"}`))
	resp, err := e.client.Do(req) // valid cookie, no CSRF header
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("POST without CSRF header = %d, want 403", resp.StatusCode)
	}
}

func TestLoginCookieAndRateLimit(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.store.CreateAdmin("root", "correct-horse", false); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if code, _ := e.do("POST", "/api/login", credentials{"root", "wrong"}); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	// Even the right password is refused while locked out.
	if code, _ := e.do("POST", "/api/login", credentials{"root", "correct-horse"}); code != http.StatusTooManyRequests {
		t.Fatalf("locked-out login = %d, want 429", code)
	}
}

func TestSessionCookieFlags(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.store.CreateAdmin("root", "correct-horse", false); err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest("POST", e.srv.URL+"/api/login", strings.NewReader(`{"username":"root","password":"correct-horse"}`))
	req.Header.Set("X-Requested-With", "rterminal")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	var cookie *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == cookieName {
			cookie = c
		}
	}
	if cookie == nil || !cookie.HttpOnly || cookie.SameSite != http.SameSiteStrictMode {
		t.Fatalf("session cookie = %+v, want HttpOnly + SameSite=Strict", cookie)
	}
}

func TestGroupsAndMachinesLifecycle(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()

	code, body := e.do("POST", "/api/groups", groupInput{Name: "homelab", Description: "casa"})
	if code != http.StatusCreated {
		t.Fatalf("create group = %d %s", code, body)
	}
	var group store.Group
	_ = json.Unmarshal(body, &group)
	if code, _ := e.do("POST", "/api/groups", groupInput{Name: "homelab"}); code != http.StatusConflict {
		t.Fatalf("duplicate group = %d, want 409", code)
	}

	code, body = e.do("POST", "/api/machines", machineInput{Name: "Raspberry Pi", GroupID: &group.ID})
	if code != http.StatusCreated {
		t.Fatalf("create machine = %d %s", code, body)
	}
	var created machineCreated
	_ = json.Unmarshal(body, &created)
	if created.Machine.ID != "raspberry-pi" || created.Credential == "" || created.Machine.Online {
		t.Fatalf("created = %+v", created)
	}
	if !strings.Contains(created.Commands.Binary, "RTERMINAL_TOKEN="+created.Credential) ||
		!strings.Contains(created.Commands.Binary, "--agent-id raspberry-pi") ||
		!strings.Contains(created.Commands.Binary, "--server-url ws://") ||
		!strings.Contains(created.Commands.Systemd, "ExecStart=/usr/local/bin/agent") {
		t.Fatalf("commands = %+v", created.Commands)
	}
	if code, _ := e.do("POST", "/api/machines", machineInput{Name: "x", GroupID: func() *int64 { v := int64(404); return &v }()}); code != http.StatusNotFound {
		t.Fatalf("unknown group = %d, want 404", code)
	}

	if code, _ := e.do("PUT", "/api/machines/raspberry-pi", machineInput{Name: "Pi da sala"}); code != http.StatusOK {
		t.Fatalf("update machine = %d", code)
	}
	// The credential is never returned again.
	_, body = e.do("GET", "/api/machines", nil)
	if bytes.Contains(body, []byte(created.Credential)) || !bytes.Contains(body, []byte("Pi da sala")) {
		t.Fatalf("list = %s", body)
	}

	if code, _ := e.do("DELETE", "/api/groups/"+itoa(group.ID), nil); code != http.StatusNoContent {
		t.Fatalf("delete group = %d", code)
	}
	if code, _ := e.do("DELETE", "/api/machines/raspberry-pi", nil); code != http.StatusNoContent {
		t.Fatalf("delete machine = %d", code)
	}
	if code, _ := e.do("DELETE", "/api/machines/raspberry-pi", nil); code != http.StatusNotFound {
		t.Fatalf("second delete = %d, want 404", code)
	}
}

func itoa(v int64) string { return strings.TrimSpace(strings.Trim(jsonNumber(v), "\"")) }

func jsonNumber(v int64) string { b, _ := json.Marshal(v); return string(b) }

func TestAgentOnlineStatusAndRotation(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("notebook")

	agent := e.connectAgent(m.Credential, protocol.Message{Hostname: "thiago-nb", OS: "linux", Arch: "amd64"})
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("notebook") })
	_, body := e.do("GET", "/api/machines", nil)
	var list []machineView
	_ = json.Unmarshal(body, &list)
	if len(list) != 1 || !list[0].Online || list[0].Hostname != "thiago-nb" || list[0].LastSeen == nil {
		t.Fatalf("list = %s", body)
	}

	// Rotation kicks the agent and the old credential stops working.
	code, body := e.do("POST", "/api/machines/notebook/rotate", nil)
	if code != 200 {
		t.Fatalf("rotate = %d %s", code, body)
	}
	var rotated machineCreated
	_ = json.Unmarshal(body, &rotated)
	_ = agent.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, _, err := agent.ReadMessage(); !websocket.IsCloseError(err, websocket.ClosePolicyViolation) {
		t.Fatalf("agent not kicked: %v", err)
	}
	if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/agent"), http.Header{"Authorization": {"Bearer " + m.Credential}}); err == nil || resp.StatusCode != 401 {
		t.Fatalf("old credential still accepted: err=%v", err)
	}
	e.connectAgent(rotated.Credential, protocol.Message{})
	waitUntil(t, "online again", func() bool { return e.relay.IsOnline("notebook") })
}

func TestWebTerminalEndToEnd(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	m := e.createMachine("server-remoto")

	cookies := e.client.Jar.Cookies(mustURL(e.srv.URL))
	header := http.Header{"Cookie": {cookies[0].String()}}

	// Offline machine.
	if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=server-remoto"), header); err == nil || resp.StatusCode != http.StatusConflict {
		t.Fatalf("offline terminal: err=%v resp=%v, want 409", err, resp)
	}

	agent := e.connectAgent(m.Credential, protocol.Message{})
	waitUntil(t, "online", func() bool { return e.relay.IsOnline("server-remoto") })

	// Cross-site WebSocket is refused even with a valid cookie.
	evil := http.Header{"Cookie": header["Cookie"], "Origin": {"https://evil.example"}}
	if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=server-remoto"), evil); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin terminal: err=%v resp=%v, want 403", err, resp)
	}
	// No cookie, no terminal.
	if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=server-remoto"), nil); err == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("anonymous terminal: err=%v resp=%v, want 401", err, resp)
	}

	browser, _, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=server-remoto"), header)
	if err != nil {
		t.Fatal(err)
	}
	defer browser.Close()

	var start protocol.Message
	_ = agent.SetReadDeadline(time.Now().Add(3 * time.Second))
	if err := agent.ReadJSON(&start); err != nil || start.Type != protocol.TypeStartSession {
		t.Fatalf("start = %+v, %v", start, err)
	}
	session, _, err := websocket.DefaultDialer.Dial(e.wsURL("/session?id="+url.QueryEscape(start.SessionID)),
		http.Header{"Authorization": {"Bearer " + m.Credential}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	for _, c := range []*websocket.Conn{browser, session} {
		var notice protocol.Message
		if err := c.ReadJSON(&notice); err != nil || notice.Type != protocol.TypePaired {
			t.Fatalf("notice = %+v, %v", notice, err)
		}
	}
	_ = browser.WriteMessage(websocket.BinaryMessage, []byte("whoami\n"))
	if _, got, err := session.ReadMessage(); err != nil || string(got) != "whoami\n" {
		t.Fatalf("to agent = %q, %v", got, err)
	}
	_ = session.WriteMessage(websocket.BinaryMessage, []byte("root\n"))
	if _, got, err := browser.ReadMessage(); err != nil || string(got) != "root\n" {
		t.Fatalf("to browser = %q, %v", got, err)
	}

	_, body := e.do("GET", "/api/audit", nil)
	if !bytes.Contains(body, []byte("terminal_open")) || !bytes.Contains(body, []byte("machine_create")) {
		t.Fatalf("audit = %s", body)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestStaticUIAndSecurityHeaders(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	resp, err := http.Get(e.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
		resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("status=%d headers=%v", resp.StatusCode, resp.Header)
	}
}

func TestCommandsUsePublicURL(t *testing.T) {
	t.Parallel()
	s := New(Config{PublicURL: "https://rt.example.com/", Logger: log.New(io.Discard, "", 0)})
	req := httptest.NewRequest("GET", "http://ignored.local/", nil)
	cmds := s.commands(req, "pi", "secret123")
	for _, want := range []string{"--server-url wss://rt.example.com ", "--agent-id pi", "RTERMINAL_TOKEN=secret123"} {
		if !strings.Contains(cmds.Binary+" ", want) {
			t.Errorf("binary command missing %q: %s", want, cmds.Binary)
		}
	}
	if !strings.Contains(cmds.Systemd, "echo 'RTERMINAL_TOKEN=secret123'") || !strings.Contains(cmds.Systemd, "wss://rt.example.com") {
		t.Errorf("systemd command = %s", cmds.Systemd)
	}
	if !s.secure(req) {
		t.Error("https public URL should mark cookies Secure")
	}
}

func TestTemporaryPasswordMustBeChangedBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	if _, err := e.store.CreateAdmin("root", "temporary-pass", true); err != nil {
		t.Fatal(err)
	}
	code, body := e.do("POST", "/api/login", credentials{"root", "temporary-pass"})
	if code != 200 || !strings.Contains(string(body), `"must_change_password":true`) {
		t.Fatalf("login = %d %s", code, body)
	}

	// Everything except me/logout/password is blocked, including the terminal.
	for _, path := range []string{"/api/machines", "/api/groups", "/api/audit"} {
		if code, body := e.do("GET", path, nil); code != http.StatusForbidden || !strings.Contains(string(body), errPasswordChangeRequired) {
			t.Fatalf("GET %s = %d %s, want 403 password change required", path, code, body)
		}
	}
	if code, _ := e.do("POST", "/api/machines", machineInput{Name: "x"}); code != http.StatusForbidden {
		t.Fatalf("create machine = %d, want 403", code)
	}
	cookies := e.client.Jar.Cookies(mustURL(e.srv.URL))
	if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=x"), http.Header{"Cookie": {cookies[0].String()}}); err == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("terminal with temp password: err=%v resp=%v, want 403", err, resp)
	}
	if code, body := e.do("GET", "/api/me", nil); code != 200 || !strings.Contains(string(body), `"must_change_password":true`) {
		t.Fatalf("me = %d %s", code, body)
	}

	// Validation of the change itself.
	for name, in := range map[string]passwordInput{
		"wrong current": {Current: "nope", New: "a-new-password"},
		"too short":     {Current: "temporary-pass", New: "short"},
		"unchanged":     {Current: "temporary-pass", New: "temporary-pass"},
	} {
		if code, _ := e.do("POST", "/api/password", in); code == http.StatusNoContent {
			t.Fatalf("%s: change was accepted", name)
		}
	}
	if code, _ := e.do("POST", "/api/password", passwordInput{Current: "temporary-pass", New: "a-new-password"}); code != http.StatusNoContent {
		t.Fatalf("change = %d, want 204", code)
	}

	// The same session is now fully usable, and the new password logs in cleanly.
	if code, _ := e.do("GET", "/api/machines", nil); code != 200 {
		t.Fatalf("machines after change = %d", code)
	}
	if code, body := e.do("GET", "/api/me", nil); code != 200 || !strings.Contains(string(body), `"must_change_password":false`) {
		t.Fatalf("me after change = %d %s", code, body)
	}
	if _, err := e.store.VerifyAdmin("root", "a-new-password"); err != nil {
		t.Fatalf("new password rejected: %v", err)
	}
	_, audit := e.do("GET", "/api/audit", nil)
	if !strings.Contains(string(audit), "password_change") {
		t.Fatalf("audit = %s", audit)
	}
}

func TestChangePasswordGuessingIsRateLimited(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	for i := 0; i < 5; i++ {
		if code, _ := e.do("POST", "/api/password", passwordInput{Current: "wrong", New: "another-password"}); code != http.StatusUnauthorized {
			t.Fatalf("attempt %d = %d, want 401", i, code)
		}
	}
	if code, _ := e.do("POST", "/api/password", passwordInput{Current: "correct-horse", New: "another-password"}); code != http.StatusTooManyRequests {
		t.Fatalf("locked-out change = %d, want 429", code)
	}
}

func TestClientIPBehindTrustedProxy(t *testing.T) {
	t.Parallel()
	_, proxyNet, _ := net.ParseCIDR("172.16.0.0/12")
	s := New(Config{TrustedProxies: []*net.IPNet{proxyNet}, Logger: log.New(io.Discard, "", 0)})
	for name, tc := range map[string]struct {
		peer, xff, want string
	}{
		"no proxy, header ignored":          {"203.0.113.9:5000", "1.2.3.4", "203.0.113.9"},
		"trusted proxy, real client":        {"172.18.0.2:4000", "198.51.100.7", "198.51.100.7"},
		"forged prefix is skipped":          {"172.18.0.2:4000", "6.6.6.6, 198.51.100.7", "198.51.100.7"},
		"extra trusted hop is skipped":      {"172.18.0.2:4000", "198.51.100.7, 172.20.0.9", "198.51.100.7"},
		"malformed header falls back":       {"172.18.0.2:4000", "not-an-ip", "172.18.0.2"},
		"trusted proxy without header":      {"172.18.0.2:4000", "", "172.18.0.2"},
		"untrusted peer cannot spoof proxy": {"8.8.8.8:1", "172.18.0.2, 1.1.1.1", "8.8.8.8"},
	} {
		req := httptest.NewRequest("GET", "/", nil)
		req.RemoteAddr = tc.peer
		if tc.xff != "" {
			req.Header.Set("X-Forwarded-For", tc.xff)
		}
		if got := s.clientIP(req); got != tc.want {
			t.Errorf("%s: clientIP = %q, want %q", name, got, tc.want)
		}
	}
}

// Behind the proxy, one attacker's failures must not lock out other users.
func TestLoginLimiterIsPerRealClientBehindProxy(t *testing.T) {
	t.Parallel()
	_, localNet, _ := net.ParseCIDR("127.0.0.0/8")
	e := newEnv(t, func(c *Config) { c.TrustedProxies = []*net.IPNet{localNet} })
	if _, err := e.store.CreateAdmin("root", "correct-horse", false); err != nil {
		t.Fatal(err)
	}
	attempt := func(forwardedFor, user, pass string) int {
		raw, _ := json.Marshal(credentials{user, pass})
		req, _ := http.NewRequest("POST", e.srv.URL+"/api/login", bytes.NewReader(raw))
		req.Header.Set("X-Requested-With", "rterminal")
		req.Header.Set("X-Forwarded-For", forwardedFor)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for i := 0; i < 5; i++ {
		attempt("203.0.113.66", "ghost"+itoa(int64(i)), "x") // attacker, different usernames
	}
	if got := attempt("203.0.113.66", "root", "correct-horse"); got != http.StatusTooManyRequests {
		t.Fatalf("attacker IP not limited: %d", got)
	}
	if got := attempt("198.51.100.10", "root", "correct-horse"); got != http.StatusOK {
		t.Fatalf("legitimate client behind the same proxy was locked out: %d", got)
	}
}

func TestHSTSOnlyWithHTTPSPublicURL(t *testing.T) {
	t.Parallel()
	for url, want := range map[string]bool{"https://rt.example.com": true, "": false, "http://rt.local": false} {
		e := newEnv(t, func(c *Config) { c.PublicURL = url })
		resp, err := http.Get(e.srv.URL + "/")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if got := resp.Header.Get("Strict-Transport-Security") != ""; got != want {
			t.Errorf("publicURL %q: HSTS present = %v, want %v", url, got, want)
		}
	}
}
