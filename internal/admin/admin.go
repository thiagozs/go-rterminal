// Package admin serves the admin REST API, the web terminal WebSocket and the
// embedded single-page UI used to manage machines and groups.
package admin

import (
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/relay"
	"github.com/thiagozs/go-rterminal/internal/store"
)

//go:embed web
var webFS embed.FS

// errPasswordChangeRequired is also matched by the UI.
const errPasswordChangeRequired = "password change required"

const (
	cookieName = "rt_session"
	// csrfHeader must accompany every state-changing API call. Browsers cannot
	// add it to cross-site requests without a CORS preflight, which we never allow.
	csrfHeader = "X-Requested-With"
	csrfValue  = "rterminal"

	maxBodyBytes = 1 << 20
)

// Relay is the part of the relay server the admin needs.
type Relay interface {
	IsOnline(id string) bool
	Disconnect(id, reason string)
	OpenTerminal(machineID string, conn *websocket.Conn) (string, error)
}

type Config struct {
	Store *store.Store
	Relay Relay
	// PublicURL is the externally reachable base URL (e.g. https://rt.example.com),
	// used in the generated agent commands. When empty it is derived from the request.
	PublicURL string
	// TrustedProxies are the networks whose X-Forwarded-For header is believed
	// (e.g. the Docker network of nginx-proxy). Without it the TCP peer address
	// is the client address, which behind a proxy would be the proxy itself.
	TrustedProxies []*net.IPNet
	SessionTTL     time.Duration
	Logger         *log.Logger
}

type Server struct {
	store      *store.Store
	relay      Relay
	publicURL  string
	trusted    []*net.IPNet
	sessionTTL time.Duration
	logger     *log.Logger
	limiter    *loginLimiter
	upgrader   websocket.Upgrader
}

func New(config Config) *Server {
	logger := config.Logger
	if logger == nil {
		logger = log.Default()
	}
	ttl := config.SessionTTL
	if ttl <= 0 {
		ttl = 12 * time.Hour
	}
	s := &Server{
		store:      config.Store,
		relay:      config.Relay,
		publicURL:  strings.TrimRight(config.PublicURL, "/"),
		trusted:    config.TrustedProxies,
		sessionTTL: ttl,
		logger:     logger,
		limiter:    newLoginLimiter(5, 5*time.Minute),
	}
	s.upgrader = websocket.Upgrader{CheckOrigin: sameOrigin}
	return s
}

// Handler returns the admin routes: /api/*, /ws/terminal and the static UI.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.login)
	mux.HandleFunc("POST /api/logout", s.authedAny(s.logout))
	mux.HandleFunc("GET /api/me", s.authedAny(s.me))
	mux.HandleFunc("POST /api/password", s.authedAny(s.changePassword))

	mux.HandleFunc("GET /api/groups", s.authed(s.listGroups))
	mux.HandleFunc("POST /api/groups", s.authed(s.createGroup))
	mux.HandleFunc("PUT /api/groups/{id}", s.authed(s.updateGroup))
	mux.HandleFunc("DELETE /api/groups/{id}", s.authed(s.deleteGroup))

	mux.HandleFunc("GET /api/machines", s.authed(s.listMachines))
	mux.HandleFunc("POST /api/machines", s.authed(s.createMachine))
	mux.HandleFunc("PUT /api/machines/{id}", s.authed(s.updateMachine))
	mux.HandleFunc("DELETE /api/machines/{id}", s.authed(s.deleteMachine))
	mux.HandleFunc("POST /api/machines/{id}/rotate", s.authed(s.rotateMachine))

	mux.HandleFunc("GET /api/audit", s.authed(s.listAudit))
	mux.HandleFunc("GET /ws/terminal", s.terminal)

	static, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(static)))
	return s.securityHeaders(mux)
}

func (s *Server) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		if strings.HasPrefix(s.publicURL, "https://") {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- plumbing ----------

type authedHandler func(w http.ResponseWriter, r *http.Request, admin *store.Admin)

func (s *Server) adminFromCookie(r *http.Request) (*store.Admin, bool) {
	cookie, err := r.Cookie(cookieName)
	if err != nil || cookie.Value == "" {
		return nil, false
	}
	admin, err := s.store.LookupWebSession(cookie.Value)
	if err != nil {
		return nil, false
	}
	return admin, true
}

// authed requires a logged-in admin who has no pending password change.
func (s *Server) authed(next authedHandler) http.HandlerFunc {
	return s.authedWith(next, false)
}

// authedAny also admits admins who must still change a temporary password.
func (s *Server) authedAny(next authedHandler) http.HandlerFunc {
	return s.authedWith(next, true)
}

func (s *Server) authedWith(next authedHandler, allowPending bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !checkCSRF(r) {
			writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
			return
		}
		admin, ok := s.adminFromCookie(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "login required")
			return
		}
		if admin.MustChangePassword && !allowPending {
			writeError(w, http.StatusForbidden, errPasswordChangeRequired)
			return
		}
		next(w, r, admin)
	}
}

func checkCSRF(r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(csrfHeader)), []byte(csrfValue)) == 1
}

func sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true // not a browser
	}
	u, err := url.Parse(origin)
	return err == nil && strings.EqualFold(u.Host, r.Host)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(io.LimitReader(r.Body, maxBodyBytes))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}

func (s *Server) storeError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeError(w, http.StatusNotFound, "not found")
	case errors.Is(err, store.ErrConflict):
		writeError(w, http.StatusConflict, "already exists")
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest, "invalid input")
	default:
		s.logger.Printf("[admin] erro interno: %v", err)
		writeError(w, http.StatusInternalServerError, "internal error")
	}
}

func (s *Server) audit(actor, action, target, detail string) {
	if err := s.store.AddAudit(actor, action, target, detail); err != nil {
		s.logger.Printf("[admin] falha ao gravar auditoria: %v", err)
	}
}

func peerIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func (s *Server) isTrusted(ip net.IP) bool {
	for _, network := range s.trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP returns the address of the real client. X-Forwarded-For is only
// honoured when the TCP peer is a trusted proxy; it is then read right to
// left, skipping further trusted hops, so a client cannot forge its address.
func (s *Server) clientIP(r *http.Request) string {
	peer := peerIP(r)
	ip := net.ParseIP(peer)
	if ip == nil || !s.isTrusted(ip) {
		return peer
	}
	forwarded := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(forwarded) - 1; i >= 0; i-- {
		hop := net.ParseIP(strings.TrimSpace(forwarded[i]))
		if hop == nil {
			return peer // malformed header: do not guess
		}
		if !s.isTrusted(hop) {
			return hop.String()
		}
	}
	return peer
}

func (s *Server) secure(r *http.Request) bool {
	return r.TLS != nil || strings.HasPrefix(s.publicURL, "https://")
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil
}

// ---------- auth ----------

type credentials struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	if !checkCSRF(r) {
		writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	var in credentials
	if !decode(w, r, &in) {
		return
	}
	ip := s.clientIP(r)
	if !s.limiter.allow(ip) || !s.limiter.allow("user:"+strings.ToLower(in.Username)) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	admin, err := s.store.VerifyAdmin(in.Username, in.Password)
	if err != nil {
		if errors.Is(err, store.ErrInvalidCredentials) {
			s.limiter.fail(ip)
			s.limiter.fail("user:" + strings.ToLower(in.Username))
			s.audit(in.Username, "login_failed", ip, "")
			writeError(w, http.StatusUnauthorized, "invalid username or password")
			return
		}
		s.storeError(w, err)
		return
	}
	s.limiter.reset(ip)
	s.limiter.reset("user:" + strings.ToLower(in.Username))
	token, err := s.store.CreateWebSession(admin.ID, s.sessionTTL)
	if err != nil {
		s.storeError(w, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: cookieName, Value: token, Path: "/", HttpOnly: true,
		SameSite: http.SameSiteStrictMode, Secure: s.secure(r),
		Expires: time.Now().Add(s.sessionTTL),
	})
	s.audit(admin.Username, "login", ip, "")
	_ = s.store.PurgeExpiredSessions()
	writeJSON(w, http.StatusOK, sessionView(admin))
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if cookie, err := r.Cookie(cookieName); err == nil {
		_ = s.store.DeleteWebSession(cookie.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: cookieName, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: s.secure(r)})
	s.audit(admin.Username, "logout", "", "")
	w.WriteHeader(http.StatusNoContent)
}

func sessionView(admin *store.Admin) map[string]any {
	return map[string]any{"username": admin.Username, "must_change_password": admin.MustChangePassword}
}

func (s *Server) me(w http.ResponseWriter, _ *http.Request, admin *store.Admin) {
	writeJSON(w, http.StatusOK, sessionView(admin))
}

type passwordInput struct {
	Current string `json:"current_password"`
	New     string `json:"new_password"`
}

func (s *Server) changePassword(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	var in passwordInput
	if !decode(w, r, &in) {
		return
	}
	// Guessing the current password through this endpoint is rate limited too.
	key := "pw:" + strconv.FormatInt(admin.ID, 10)
	if !s.limiter.allow(key) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	cookie, _ := r.Cookie(cookieName)
	err := s.store.ChangePassword(admin.ID, in.Current, in.New, cookie.Value)
	switch {
	case errors.Is(err, store.ErrInvalidCredentials):
		s.limiter.fail(key)
		s.audit(admin.Username, "password_change_failed", s.clientIP(r), "")
		writeError(w, http.StatusUnauthorized, "current password is incorrect")
		return
	case errors.Is(err, store.ErrInvalid):
		writeError(w, http.StatusBadRequest,
			"new password must have at least "+strconv.Itoa(store.MinPasswordLength)+" characters and differ from the current one")
		return
	case err != nil:
		s.storeError(w, err)
		return
	}
	s.limiter.reset(key)
	s.audit(admin.Username, "password_change", "", "other sessions ended")
	w.WriteHeader(http.StatusNoContent)
}

// ---------- groups ----------

type groupInput struct {
	Name        string `json:"name"`
	Description string `json:"description"`
}

func (s *Server) listGroups(w http.ResponseWriter, _ *http.Request, _ *store.Admin) {
	groups, err := s.store.ListGroups()
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, groups)
}

func (s *Server) createGroup(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	var in groupInput
	if !decode(w, r, &in) {
		return
	}
	group, err := s.store.CreateGroup(in.Name, in.Description)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "group_create", group.Name, "")
	writeJSON(w, http.StatusCreated, group)
}

func (s *Server) updateGroup(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	var in groupInput
	if !decode(w, r, &in) {
		return
	}
	if err := s.store.UpdateGroup(id, in.Name, in.Description); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "group_update", in.Name, "")
	group, err := s.store.GetGroup(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, group)
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	group, err := s.store.GetGroup(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.store.DeleteGroup(id); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "group_delete", group.Name, "machines kept, ungrouped")
	w.WriteHeader(http.StatusNoContent)
}

// ---------- audit ----------

func (s *Server) listAudit(w http.ResponseWriter, r *http.Request, _ *store.Admin) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.store.ListAudit(limit)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, entries)
}

// ---------- web terminal ----------

func (s *Server) terminal(w http.ResponseWriter, r *http.Request) {
	admin, ok := s.adminFromCookie(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "login required")
		return
	}
	if admin.MustChangePassword {
		writeError(w, http.StatusForbidden, errPasswordChangeRequired)
		return
	}
	machine, err := s.store.GetMachine(r.URL.Query().Get("machine"))
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !s.relay.IsOnline(machine.ID) {
		writeError(w, http.StatusConflict, "machine offline")
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return // Upgrade already replied (e.g. origin rejected).
	}
	sessionID, err := s.relay.OpenTerminal(machine.ID, conn)
	if err != nil {
		reason := "could not start session"
		if errors.Is(err, relay.ErrAgentOffline) {
			reason = "machine offline"
		}
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, reason), time.Now().Add(time.Second))
		_ = conn.Close()
		return
	}
	s.audit(admin.Username, "terminal_open", machine.ID, "session "+sessionID)
}
