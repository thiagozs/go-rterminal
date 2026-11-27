package admin

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/skip2/go-qrcode"
	"github.com/thiagozs/go-rterminal/internal/store"
	"github.com/thiagozs/go-rterminal/internal/twofactor"
)

// Mailer sends the e-mail PIN.
type Mailer interface {
	Send(to, subject, body string) error
}

const (
	maxChallengeAttempts = 5                // wrong codes before the pending login is thrown away
	maxPINSends          = 5                // e-mails per pending login
	pinResendCooldown    = 60 * time.Second // between two e-mails
)

// ---------- pending logins and PINs ----------

// challenge is the state between the password and the second factor. It lives
// in memory: a restart simply makes people log in again.
type challenge struct {
	userID     int64
	expires    time.Time
	attempts   int
	pinHash    string
	pinExpires time.Time
	lastSent   time.Time
	sends      int
}

type challengeStore struct {
	mu  sync.Mutex
	ttl time.Duration
	m   map[string]*challenge
}

func newChallengeStore(ttl time.Duration) *challengeStore {
	return &challengeStore{ttl: ttl, m: map[string]*challenge{}}
}

func (c *challengeStore) sweep(now time.Time) {
	for id, ch := range c.m {
		if now.After(ch.expires) {
			delete(c.m, id)
		}
	}
}

// create opens a pending login for userID and returns its secret id.
func (c *challengeStore) create(userID int64) (string, error) {
	buf := make([]byte, 24)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := hex.EncodeToString(buf)
	c.put(id, userID)
	return id, nil
}

// put (re)starts the state under a chosen key, e.g. the PIN that confirms
// turning the e-mail factor on.
func (c *challengeStore) put(key string, userID int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	c.sweep(now)
	c.m[key] = &challenge{userID: userID, expires: now.Add(c.ttl)}
}

// with runs fn on the live challenge under the lock; false when it is missing or expired.
func (c *challengeStore) with(key string, fn func(*challenge)) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.m[key]
	if ch == nil || time.Now().After(ch.expires) {
		delete(c.m, key)
		return false
	}
	fn(ch)
	return true
}

func (c *challengeStore) drop(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.m, key)
}

func enableKey(userID int64) string { return "enable-email-" + strconv.FormatInt(userID, 10) }

var errPINCooldown = errors.New("wait before asking for another code")

// issuePIN prepares a new PIN on the challenge and returns it. Callers send it
// and call undoPIN if sending fails.
func (s *Server) issuePIN(key string) (pin string, err error) {
	pin, err = twofactor.NewPIN()
	if err != nil {
		return "", err
	}
	ok := s.challenges.with(key, func(ch *challenge) {
		switch {
		case ch.sends >= maxPINSends:
			err = errors.New("too many codes requested; log in again")
		case time.Since(ch.lastSent) < pinResendCooldown:
			err = errPINCooldown
		default:
			ch.pinHash = twofactor.HashSecretCode(key, pin)
			ch.pinExpires = time.Now().Add(s.challenges.ttl)
			ch.lastSent = time.Now()
			ch.sends++
		}
	})
	if !ok {
		return "", errChallengeGone
	}
	return pin, err
}

func (s *Server) undoPIN(key string) {
	s.challenges.with(key, func(ch *challenge) {
		ch.pinHash = ""
		ch.lastSent = time.Time{}
		ch.sends--
	})
}

// checkPIN consumes the PIN when code matches (single use).
func (s *Server) checkPIN(key, code string) bool {
	matched := false
	s.challenges.with(key, func(ch *challenge) {
		if ch.pinHash != "" && time.Now().Before(ch.pinExpires) &&
			twofactor.EqualHash(twofactor.HashSecretCode(key, code), ch.pinHash) {
			ch.pinHash = ""
			matched = true
		}
	})
	return matched
}

var errChallengeGone = errors.New("login expired; start again")

func (s *Server) sendPINEmail(to, purpose, pin, ip string) error {
	return s.mailer.Send(to, "Seu código de acesso ao "+s.issuer, fmt.Sprintf(
		"%s\n\nSeu código: %s\n\nO código é válido por %d minutos e só funciona uma vez. Não o compartilhe com ninguém.\n\nPedido feito do endereço %s em %s.\nSe não foi você, troque sua senha e avise um administrador.\n",
		purpose, pin, int(s.challenges.ttl.Minutes()), ip, time.Now().Format("02/01/2006 15:04:05 MST")))
}

func maskEmail(email string) string {
	at := strings.LastIndex(email, "@")
	if at < 1 {
		return ""
	}
	return email[:1] + "***" + email[at:]
}

// ---------- policy ----------

// needsTwoFactorSetup tells whether the policy obliges this account to enable
// a second factor and it has none yet.
func (s *Server) needsTwoFactorSetup(admin *store.Admin) bool {
	if s.sealer == nil || admin.HasTwoFactor() {
		return false
	}
	switch s.require2FA {
	case "all":
		return true
	case "admins":
		return admin.IsAdmin()
	}
	return false
}

func (s *Server) twoFactorOff(w http.ResponseWriter) bool {
	if s.sealer == nil {
		writeError(w, http.StatusNotFound, "two-factor authentication is disabled on this server")
		return true
	}
	return false
}

// ---------- login ----------

// beginTwoFactor answers a correct password with the pending login instead of a session.
func (s *Server) beginTwoFactor(w http.ResponseWriter, admin *store.Admin) {
	id, err := s.challenges.create(admin.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	methods := []string{}
	if admin.TOTPEnabled {
		methods = append(methods, "totp")
	}
	if admin.EmailPIN && s.mailer != nil && admin.Email != "" {
		methods = append(methods, "email")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"two_factor": true, "challenge": id, "methods": methods,
		"email_hint": maskEmail(admin.Email), "expires_in": int(s.challenges.ttl.Seconds()),
	})
}

type challengeInput struct {
	Challenge string `json:"challenge"`
}

func (s *Server) sendLoginPIN(w http.ResponseWriter, r *http.Request) {
	if !checkCSRF(r) {
		writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	if s.twoFactorOff(w) {
		return
	}
	var in challengeInput
	if !decode(w, r, &in) {
		return
	}
	var userID int64
	if !s.challenges.with(in.Challenge, func(ch *challenge) { userID = ch.userID }) {
		writeError(w, http.StatusUnauthorized, errChallengeGone.Error())
		return
	}
	user, err := s.store.GetUser(userID)
	if err != nil || !user.EmailPIN || user.Email == "" || s.mailer == nil {
		writeError(w, http.StatusBadRequest, "e-mail code is not available for this account")
		return
	}
	pin, err := s.issuePIN(in.Challenge)
	if err != nil {
		s.pinError(w, err)
		return
	}
	if err := s.sendPINEmail(user.Email, "Use este código para concluir o login.", pin, s.clientIP(r)); err != nil {
		s.undoPIN(in.Challenge)
		s.logger.Printf("[admin] falha ao enviar PIN por e-mail | usuario=%s err=%v", user.Username, err)
		writeError(w, http.StatusBadGateway, "could not send the e-mail; try another method")
		return
	}
	s.audit(user.Username, "2fa_pin_sent", s.clientIP(r), "login")
	writeJSON(w, http.StatusOK, map[string]any{"email_hint": maskEmail(user.Email), "resend_in": int(pinResendCooldown.Seconds())})
}

func (s *Server) pinError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errPINCooldown):
		w.Header().Set("Retry-After", strconv.Itoa(int(pinResendCooldown.Seconds())))
		writeError(w, http.StatusTooManyRequests, err.Error())
	case errors.Is(err, errChallengeGone):
		writeError(w, http.StatusUnauthorized, err.Error())
	default:
		writeError(w, http.StatusTooManyRequests, err.Error())
	}
}

type verifyInput struct {
	Challenge string `json:"challenge"`
	Method    string `json:"method"` // totp | email | recovery
	Code      string `json:"code"`
}

func (s *Server) verifyLogin2FA(w http.ResponseWriter, r *http.Request) {
	if !checkCSRF(r) {
		writeError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return
	}
	if s.twoFactorOff(w) {
		return
	}
	var in verifyInput
	if !decode(w, r, &in) {
		return
	}
	ip := s.clientIP(r)
	ipKey := "2fa:" + ip
	if blocked, wait := s.twoFAIPs.blocked(ipKey); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again in "+humanWait(wait))
		return
	}
	// Every try burns an attempt of the pending login; after a few it is gone
	// and the password has to be entered again.
	var userID int64
	exhausted := false
	alive := s.challenges.with(in.Challenge, func(ch *challenge) {
		userID = ch.userID
		ch.attempts++
		exhausted = ch.attempts > maxChallengeAttempts
	})
	if !alive {
		writeError(w, http.StatusUnauthorized, errChallengeGone.Error())
		return
	}
	if exhausted {
		s.challenges.drop(in.Challenge)
		writeError(w, http.StatusUnauthorized, "too many wrong codes; log in again")
		return
	}
	userKey := "u:" + strconv.FormatInt(userID, 10)
	if blocked, wait := s.twoFAFails.blocked(userKey); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		writeError(w, http.StatusTooManyRequests, "too many wrong codes for this account, try again in "+humanWait(wait))
		return
	}
	user, err := s.store.GetUser(userID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	ok, detail, err := s.checkSecondFactor(user, in.Challenge, in.Method, in.Code)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if !ok {
		s.twoFAIPs.fail(ipKey)
		s.twoFAFails.fail(userKey)
		s.logger.Printf("[admin] 2FA falhou | ip=%s usuario=%q metodo=%s", ip, user.Username, in.Method)
		s.audit(user.Username, "2fa_failed", ip, in.Method)
		writeError(w, http.StatusUnauthorized, "invalid code")
		return
	}
	s.challenges.drop(in.Challenge)
	s.twoFAIPs.reset(ipKey)
	s.twoFAFails.reset(userKey)
	s.startSession(w, r, user, detail)
}

// checkSecondFactor validates one code; detail describes what was used.
func (s *Server) checkSecondFactor(user *store.Admin, challengeKey, method, code string) (bool, string, error) {
	switch method {
	case "totp":
		if !user.TOTPEnabled {
			return false, "", nil
		}
		ok, err := s.checkTOTP(user.ID, code)
		return ok, "2fa totp", err
	case "email":
		if !user.EmailPIN {
			return false, "", nil
		}
		return s.checkPIN(challengeKey, code), "2fa email", nil
	case "recovery":
		ok, err := s.store.UseRecoveryCode(user.ID, twofactor.HashSecretCode(strconv.FormatInt(user.ID, 10), code))
		if ok {
			left, _ := s.store.RecoveryCodesLeft(user.ID)
			s.audit(user.Username, "2fa_recovery_used", "", fmt.Sprintf("%d codes left", left))
			return true, fmt.Sprintf("2fa recovery code (%d left)", left), nil
		}
		return false, "", err
	}
	return false, "", nil
}

// checkTOTP verifies an app code and records its step so it works only once.
func (s *Server) checkTOTP(userID int64, code string) (bool, error) {
	sealed, lastStep, enabled, err := s.store.TOTPSecret(userID)
	if err != nil || !enabled {
		return false, err
	}
	secret, err := s.sealer.Open(sealed)
	if err != nil {
		return false, err
	}
	step, ok := twofactor.Verify(secret, code, time.Now(), lastStep)
	if !ok {
		return false, nil
	}
	return s.store.AdvanceTOTPStep(userID, step)
}

// ---------- the account's own settings ----------

type twoFactorStatus struct {
	Available     bool   `json:"available"`
	TOTP          bool   `json:"totp"`
	EmailPIN      bool   `json:"email_pin"`
	Email         string `json:"email"`          // masked
	EmailPossible bool   `json:"email_possible"` // server can send mail and the account has an address
	RecoveryLeft  int    `json:"recovery_left"`
	Required      bool   `json:"required"`
}

func (s *Server) twoFactorStatus(w http.ResponseWriter, _ *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	left, err := s.store.RecoveryCodesLeft(admin.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, twoFactorStatus{
		Available: true, TOTP: admin.TOTPEnabled, EmailPIN: admin.EmailPIN, Email: maskEmail(admin.Email),
		EmailPossible: s.mailer != nil && admin.Email != "", RecoveryLeft: left,
		Required: s.require2FA == "all" || (s.require2FA == "admins" && admin.IsAdmin()),
	})
}

func (s *Server) totpSetup(w http.ResponseWriter, _ *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	if admin.TOTPEnabled {
		writeError(w, http.StatusConflict, "authenticator app already enabled; disable it first to pair another device")
		return
	}
	secret, err := twofactor.NewSecret()
	if err != nil {
		s.storeError(w, err)
		return
	}
	sealed, err := s.sealer.Seal(secret)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.store.SetTOTPPending(admin.ID, sealed); err != nil {
		s.storeError(w, err)
		return
	}
	uri := twofactor.URI(s.issuer, admin.Username, secret)
	png, err := qrcode.Encode(uri, qrcode.Medium, 240)
	if err != nil {
		s.storeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"secret": secret, "uri": uri, "qr": "data:image/png;base64," + base64.StdEncoding.EncodeToString(png),
	})
}

type codeInput struct {
	Code string `json:"code"`
}

type passwordOnly struct {
	Password string `json:"password"`
}

// finishEnable returns the recovery codes when this is the account's first factor.
func (s *Server) finishEnable(admin *store.Admin) ([]string, error) {
	left, err := s.store.RecoveryCodesLeft(admin.ID)
	if err != nil || left > 0 {
		return nil, err
	}
	return s.newRecoveryCodes(admin.ID)
}

func (s *Server) newRecoveryCodes(userID int64) ([]string, error) {
	codes, err := twofactor.NewRecoveryCodes()
	if err != nil {
		return nil, err
	}
	salt := strconv.FormatInt(userID, 10)
	hashes := make([]string, len(codes))
	for i, c := range codes {
		hashes[i] = twofactor.HashSecretCode(salt, c)
	}
	return codes, s.store.ReplaceRecoveryCodes(userID, hashes)
}

func (s *Server) totpEnable(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	var in codeInput
	if !decode(w, r, &in) {
		return
	}
	if blocked, _ := s.twoFAFails.blocked("u:" + strconv.FormatInt(admin.ID, 10)); blocked {
		writeError(w, http.StatusTooManyRequests, "too many wrong codes, try again later")
		return
	}
	sealed, err := s.store.TOTPPending(admin.ID)
	if err != nil || sealed == "" {
		writeError(w, http.StatusBadRequest, "start the setup first")
		return
	}
	secret, err := s.sealer.Open(sealed)
	if err != nil {
		s.storeError(w, err)
		return
	}
	step, ok := twofactor.Verify(secret, in.Code, time.Now(), 0)
	if !ok {
		s.twoFAFails.fail("u:" + strconv.FormatInt(admin.ID, 10))
		writeError(w, http.StatusUnauthorized, "invalid code: check the app and your phone clock")
		return
	}
	if err := s.store.EnableTOTP(admin.ID, sealed, step); err != nil {
		s.storeError(w, err)
		return
	}
	codes, err := s.finishEnable(admin)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "2fa_enabled", "", "authenticator app")
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// verifyOwnPassword confirms the password for sensitive changes, rate limited
// like the password change itself.
func (s *Server) verifyOwnPassword(w http.ResponseWriter, r *http.Request, admin *store.Admin) bool {
	var in passwordOnly
	if !decode(w, r, &in) {
		return false
	}
	key := "pw:" + strconv.FormatInt(admin.ID, 10)
	if !s.limiter.allow(key) {
		writeError(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return false
	}
	if _, err := s.store.VerifyAdmin(admin.Username, in.Password); err != nil {
		if errors.Is(err, store.ErrInvalidCredentials) {
			s.limiter.fail(key)
			s.audit(admin.Username, "password_check_failed", s.clientIP(r), "2fa change")
			writeError(w, http.StatusUnauthorized, "password is incorrect")
		} else {
			s.storeError(w, err)
		}
		return false
	}
	s.limiter.reset(key)
	return true
}

// mayDisable refuses to remove the last factor of an account the policy obliges to have one.
func (s *Server) mayDisable(w http.ResponseWriter, admin *store.Admin, remaining bool) bool {
	if remaining {
		return true
	}
	if s.require2FA == "all" || (s.require2FA == "admins" && admin.IsAdmin()) {
		writeError(w, http.StatusConflict, "two-factor authentication is required for your account; enable another method before removing this one")
		return false
	}
	return true
}

func (s *Server) totpDisable(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	if !s.verifyOwnPassword(w, r, admin) {
		return
	}
	if !s.mayDisable(w, admin, admin.EmailPIN) {
		return
	}
	if err := s.store.DisableTOTP(admin.ID); err != nil {
		s.storeError(w, err)
		return
	}
	s.afterDisable(admin, !admin.EmailPIN)
	s.audit(admin.Username, "2fa_disabled", "", "authenticator app")
	w.WriteHeader(http.StatusNoContent)
}

// afterDisable drops the recovery codes once no factor is left.
func (s *Server) afterDisable(admin *store.Admin, none bool) {
	if none {
		_ = s.store.ReplaceRecoveryCodes(admin.ID, nil)
	}
}

func (s *Server) emailEnableSend(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	if s.mailer == nil || admin.Email == "" {
		writeError(w, http.StatusBadRequest, "no e-mail sending configured or no e-mail registered for your account; ask an administrator")
		return
	}
	key := enableKey(admin.ID)
	if !s.challenges.with(key, func(*challenge) {}) {
		s.challenges.put(key, admin.ID)
	}
	pin, err := s.issuePIN(key)
	if err != nil {
		s.pinError(w, err)
		return
	}
	if err := s.sendPINEmail(admin.Email, "Use este código para ativar a verificação por e-mail.", pin, s.clientIP(r)); err != nil {
		s.undoPIN(key)
		s.logger.Printf("[admin] falha ao enviar PIN por e-mail | usuario=%s err=%v", admin.Username, err)
		writeError(w, http.StatusBadGateway, "could not send the e-mail")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"email_hint": maskEmail(admin.Email), "resend_in": int(pinResendCooldown.Seconds())})
}

func (s *Server) emailEnable(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	var in codeInput
	if !decode(w, r, &in) {
		return
	}
	userKey := "u:" + strconv.FormatInt(admin.ID, 10)
	if blocked, _ := s.twoFAFails.blocked(userKey); blocked {
		writeError(w, http.StatusTooManyRequests, "too many wrong codes, try again later")
		return
	}
	key := enableKey(admin.ID)
	exhausted := false
	s.challenges.with(key, func(ch *challenge) { ch.attempts++; exhausted = ch.attempts > maxChallengeAttempts })
	if exhausted {
		s.challenges.drop(key)
	}
	if exhausted || !s.checkPIN(key, in.Code) {
		s.twoFAFails.fail(userKey)
		writeError(w, http.StatusUnauthorized, "invalid or expired code")
		return
	}
	s.challenges.drop(key)
	if err := s.store.SetEmailPIN(admin.ID, true); err != nil {
		s.storeError(w, err)
		return
	}
	codes, err := s.finishEnable(admin)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "2fa_enabled", "", "e-mail code")
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

func (s *Server) emailDisable(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	if !s.verifyOwnPassword(w, r, admin) {
		return
	}
	if !s.mayDisable(w, admin, admin.TOTPEnabled) {
		return
	}
	if err := s.store.SetEmailPIN(admin.ID, false); err != nil {
		s.storeError(w, err)
		return
	}
	s.afterDisable(admin, !admin.TOTPEnabled)
	s.audit(admin.Username, "2fa_disabled", "", "e-mail code")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) recoveryRegenerate(w http.ResponseWriter, r *http.Request, admin *store.Admin) {
	if s.twoFactorOff(w) {
		return
	}
	if !admin.HasTwoFactor() {
		writeError(w, http.StatusBadRequest, "enable a second factor first")
		return
	}
	if !s.verifyOwnPassword(w, r, admin) {
		return
	}
	codes, err := s.newRecoveryCodes(admin.ID)
	if err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(admin.Username, "2fa_recovery_regenerated", "", "")
	writeJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// resetUser2FA lets an administrator rescue someone who lost their device.
func (s *Server) resetUser2FA(w http.ResponseWriter, r *http.Request, actor *store.Admin) {
	id, ok := pathID(r)
	if !ok {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	target, err := s.store.GetUser(id)
	if err != nil {
		s.storeError(w, err)
		return
	}
	if err := s.store.ClearTwoFactor(id); err != nil {
		s.storeError(w, err)
		return
	}
	s.audit(actor.Username, "user_2fa_reset", target.Username, "sessions ended")
	w.WriteHeader(http.StatusNoContent)
}
