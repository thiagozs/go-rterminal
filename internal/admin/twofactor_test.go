package admin

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thiagozs/go-rterminal/internal/store"
	"github.com/thiagozs/go-rterminal/internal/twofactor"
)

type sentMail struct{ to, subject, body string }

type fakeMailer struct {
	mu   sync.Mutex
	sent []sentMail
	err  error
}

func (f *fakeMailer) Send(to, subject, body string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	f.sent = append(f.sent, sentMail{to, subject, body})
	return nil
}

func (f *fakeMailer) last() sentMail {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.sent) == 0 {
		return sentMail{}
	}
	return f.sent[len(f.sent)-1]
}

func (f *fakeMailer) count() int { f.mu.Lock(); defer f.mu.Unlock(); return len(f.sent) }

var pinPattern = regexp.MustCompile(`Seu código: (\d{6})`)

func (f *fakeMailer) pin(t *testing.T) string {
	t.Helper()
	m := pinPattern.FindStringSubmatch(f.last().body)
	if m == nil {
		t.Fatalf("no PIN in the last e-mail: %q", f.last().body)
	}
	return m[1]
}

type challengeReply struct {
	TwoFactor bool     `json:"two_factor"`
	Challenge string   `json:"challenge"`
	Methods   []string `json:"methods"`
	Hint      string   `json:"email_hint"`
}

// tfEnv is an environment with a logged-in "root" admin and a fake mailer.
func tfEnv(t *testing.T, tweaks ...func(*Config)) (*env, *fakeMailer) {
	t.Helper()
	mailer := &fakeMailer{}
	e := newEnv(t, append([]func(*Config){func(c *Config) { c.Mailer = mailer }}, tweaks...)...)
	e.login()
	return e, mailer
}

func (e *env) adminPerson() *person { return &person{e: e, client: e.client} }

func (e *env) userID(name string) int64 {
	e.t.Helper()
	u, err := e.store.UserByName(name)
	if err != nil {
		e.t.Fatal(err)
	}
	return u.ID
}

func mustJSON[T any](t *testing.T, body []byte) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatalf("%v in %s", err, body)
	}
	return v
}

// enableTOTP pairs an "app" for the logged-in person and returns its secret and recovery codes.
func enableTOTP(t *testing.T, p *person) (string, []string) {
	t.Helper()
	code, body := p.do("POST", "/api/2fa/totp/setup", nil)
	if code != 200 {
		t.Fatalf("setup = %d %s", code, body)
	}
	setup := mustJSON[struct{ Secret, URI, QR string }](t, body)
	if !strings.HasPrefix(setup.QR, "data:image/png;base64,") || !strings.HasPrefix(setup.URI, "otpauth://totp/") {
		t.Fatalf("setup = %+v", setup)
	}
	now, _ := twofactor.Code(setup.Secret, time.Now())
	if code, _ := p.do("POST", "/api/2fa/totp/enable", codeInput{Code: "000000"}); code != http.StatusUnauthorized && now != "000000" {
		t.Fatalf("wrong code accepted: %d", code)
	}
	code, body = p.do("POST", "/api/2fa/totp/enable", codeInput{Code: now})
	if code != 200 {
		t.Fatalf("enable = %d %s", code, body)
	}
	return setup.Secret, mustJSON[struct {
		Codes []string `json:"recovery_codes"`
	}](t, body).Codes
}

// passwordLogin posts the password and returns the raw reply.
func passwordLogin(p *person, user, pass string) (int, []byte) {
	return p.do("POST", "/api/login", credentials{user, pass})
}

func verify(p *person, challenge, method, code string) (int, []byte) {
	return p.do("POST", "/api/login/2fa/verify", verifyInput{Challenge: challenge, Method: method, Code: code})
}

func TestTOTPLoginNeedsTheCodeAndRefusesReplays(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t)
	secret, recovery := enableTOTP(t, e.adminPerson())
	if len(recovery) != twofactor.RecoveryCodeCount {
		t.Fatalf("recovery codes = %v", recovery)
	}
	// The secret must not sit in the database in the clear.
	sealed, _, enabled, err := e.store.TOTPSecret(e.userID("root"))
	if err != nil || !enabled || sealed == "" || strings.Contains(sealed, secret) {
		t.Fatalf("stored secret %q (enabled=%v, err=%v)", sealed, enabled, err)
	}

	browser := e.newPerson()
	code, body := passwordLogin(browser, "root", "correct-horse")
	reply := mustJSON[challengeReply](t, body)
	if code != 200 || !reply.TwoFactor || reply.Challenge == "" || len(reply.Methods) != 1 || reply.Methods[0] != "totp" {
		t.Fatalf("login = %d %s", code, body)
	}
	if len(browser.client.Jar.Cookies(mustURL(e.srv.URL))) != 0 {
		t.Fatal("the password alone must not create a session cookie")
	}
	if code, _ := browser.do("GET", "/api/machines", nil); code != http.StatusUnauthorized {
		t.Fatalf("API open before the second factor: %d", code)
	}

	if code, _ := verify(browser, reply.Challenge, "totp", "123456"); code != http.StatusUnauthorized {
		t.Fatalf("wrong code = %d", code)
	}
	next, _ := twofactor.Code(secret, time.Now().Add(30*time.Second)) // a step the enabling code did not use
	if code, body := verify(browser, reply.Challenge, "totp", next); code != 200 || !strings.Contains(string(body), `"username":"root"`) {
		t.Fatalf("right code = %d %s", code, body)
	}
	if code, _ := browser.do("GET", "/api/machines", nil); code != 200 {
		t.Fatalf("API after 2FA = %d", code)
	}

	// The same code cannot be used again, nor the one used when pairing.
	again := e.newPerson()
	_, body = passwordLogin(again, "root", "correct-horse")
	ch := mustJSON[challengeReply](t, body).Challenge
	for name, c := range map[string]string{"replayed": next} {
		if code, _ := verify(again, ch, "totp", c); code != http.StatusUnauthorized {
			t.Errorf("%s code accepted: %d", name, code)
		}
	}
	_, body = e.do("GET", "/api/audit", nil)
	for _, want := range []string{"2fa_enabled", "2fa_failed", "2fa totp"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("audit lacks %q", want)
		}
	}
}

func TestWrongCodesBurnThePendingLogin(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t)
	secret, _ := enableTOTP(t, e.adminPerson())
	browser := e.newPerson()
	_, body := passwordLogin(browser, "root", "correct-horse")
	ch := mustJSON[challengeReply](t, body).Challenge
	for i := 0; i < maxChallengeAttempts; i++ {
		verify(browser, ch, "totp", "111111")
	}
	good, _ := twofactor.Code(secret, time.Now().Add(30*time.Second))
	code, body := verify(browser, ch, "totp", good)
	if code != http.StatusUnauthorized || !strings.Contains(string(body), "log in again") {
		t.Fatalf("after too many wrong codes, even the right one must fail: %d %s", code, body)
	}
	// And the challenge is gone for good.
	if code, _ := verify(browser, ch, "totp", good); code != http.StatusUnauthorized {
		t.Fatalf("challenge still alive: %d", code)
	}
}

func TestRecoveryCodesWorkOnceEach(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t)
	_, recovery := enableTOTP(t, e.adminPerson())
	login := func() (*person, string) {
		p := e.newPerson()
		_, body := passwordLogin(p, "root", "correct-horse")
		return p, mustJSON[challengeReply](t, body).Challenge
	}
	p, ch := login()
	if code, body := verify(p, ch, "recovery", strings.ToUpper(recovery[0])); code != 200 { // typed in capitals
		t.Fatalf("recovery = %d %s", code, body)
	}
	p, ch = login()
	if code, _ := verify(p, ch, "recovery", recovery[0]); code != http.StatusUnauthorized {
		t.Fatalf("a recovery code was accepted twice: %d", code)
	}
	_, body := e.do("GET", "/api/2fa", nil)
	if st := mustJSON[twoFactorStatus](t, body); st.RecoveryLeft != twofactor.RecoveryCodeCount-1 {
		t.Fatalf("recovery left = %d", st.RecoveryLeft)
	}
	// New codes replace the old ones and need the password.
	if code, _ := e.do("POST", "/api/2fa/recovery", passwordOnly{Password: "wrong"}); code != http.StatusUnauthorized {
		t.Fatalf("regenerate with a wrong password = %d", code)
	}
	if code, _ := e.do("POST", "/api/2fa/recovery", passwordOnly{Password: "correct-horse"}); code != 200 {
		t.Fatalf("regenerate = %d", code)
	}
	p, ch = login()
	if code, _ := verify(p, ch, "recovery", recovery[1]); code != http.StatusUnauthorized {
		t.Fatalf("an old recovery code survived the regeneration: %d", code)
	}
}

func TestEmailPINLogin(t *testing.T) {
	t.Parallel()
	e, mail := tfEnv(t)
	adm := e.adminPerson()
	id := e.userID("root")

	// Without an address the factor cannot be enabled.
	if code, _ := adm.do("POST", "/api/2fa/email/send", nil); code != http.StatusBadRequest {
		t.Fatalf("send without an address = %d", code)
	}
	for _, bad := range []string{"not-an-email", "Ana <ana@x.co>"} {
		if code, _ := adm.do("PUT", "/api/users/"+itoa(id), map[string]any{"role": "admin", "email": bad}); code != http.StatusBadRequest {
			t.Errorf("email %q = %d, want 400", bad, code)
		}
	}
	if code, body := adm.do("PUT", "/api/users/"+itoa(id), map[string]any{"role": "admin", "email": "root@exemplo.com"}); code != http.StatusNoContent {
		t.Fatalf("set email = %d %s", code, body)
	}

	// Enabling proves the mailbox: a PIN arrives and must be typed back.
	if code, body := adm.do("POST", "/api/2fa/email/send", nil); code != 200 || mail.count() != 1 || mail.last().to != "root@exemplo.com" {
		t.Fatalf("send = %d %s, mails=%d", code, body, mail.count())
	}
	if code, _ := adm.do("POST", "/api/2fa/email/send", nil); code != http.StatusTooManyRequests {
		t.Fatalf("resending at once = %d, want 429", code)
	}
	if code, _ := adm.do("POST", "/api/2fa/email/enable", codeInput{Code: "000000"}); code != http.StatusUnauthorized {
		t.Fatalf("wrong PIN = %d", code)
	}
	code, body := adm.do("POST", "/api/2fa/email/enable", codeInput{Code: mail.pin(t)})
	if code != 200 || !strings.Contains(string(body), "recovery_codes") {
		t.Fatalf("enable = %d %s", code, body)
	}

	// Login: password, ask for the PIN, type it.
	browser := e.newPerson()
	_, body = passwordLogin(browser, "root", "correct-horse")
	reply := mustJSON[challengeReply](t, body)
	if len(reply.Methods) != 1 || reply.Methods[0] != "email" || reply.Hint != "r***@exemplo.com" {
		t.Fatalf("login reply = %s", body)
	}
	before := mail.count()
	if code, _ := browser.do("POST", "/api/login/2fa/send", challengeInput{Challenge: reply.Challenge}); code != 200 || mail.count() != before+1 {
		t.Fatalf("send login PIN = %d, mails %d→%d", code, before, mail.count())
	}
	pin := mail.pin(t)
	if !strings.Contains(mail.last().body, "Pedido feito do endereço") || strings.Contains(mail.last().subject, pin) {
		t.Fatalf("e-mail content: %+v", mail.last())
	}
	if code, _ := verify(browser, reply.Challenge, "email", "000000"); code != http.StatusUnauthorized {
		t.Fatalf("wrong PIN = %d", code)
	}
	if code, body := verify(browser, reply.Challenge, "email", pin); code != 200 {
		t.Fatalf("right PIN = %d %s", code, body)
	}
	if code, _ := verify(browser, reply.Challenge, "email", pin); code != http.StatusUnauthorized {
		t.Fatalf("PIN accepted twice")
	}

	// A PIN from one login is useless in another one.
	other := e.newPerson()
	_, body = passwordLogin(other, "root", "correct-horse")
	ch2 := mustJSON[challengeReply](t, body).Challenge
	other.do("POST", "/api/login/2fa/send", challengeInput{Challenge: ch2})
	stale := mail.pin(t)
	third := e.newPerson()
	_, body = passwordLogin(third, "root", "correct-horse")
	ch3 := mustJSON[challengeReply](t, body).Challenge
	if code, _ := verify(third, ch3, "email", stale); code != http.StatusUnauthorized {
		t.Fatalf("a PIN issued for another login worked: %d", code)
	}
}

func TestEmailSendFailureDoesNotConsumeTheCooldown(t *testing.T) {
	t.Parallel()
	e, mail := tfEnv(t)
	adm := e.adminPerson()
	adm.do("PUT", "/api/users/"+itoa(e.userID("root")), map[string]any{"role": "admin", "email": "root@exemplo.com"})
	mail.err = errors.New("smtp down")
	if code, _ := adm.do("POST", "/api/2fa/email/send", nil); code != http.StatusBadGateway {
		t.Fatalf("send with SMTP down = %d, want 502", code)
	}
	mail.mu.Lock()
	mail.err = nil
	mail.mu.Unlock()
	if code, _ := adm.do("POST", "/api/2fa/email/send", nil); code != 200 {
		t.Fatalf("retry after the failure = %d", code)
	}
}

func TestEmailFactorUnavailableWithoutMailer(t *testing.T) {
	t.Parallel()
	e := newEnv(t) // no mailer
	e.login()
	adm := e.adminPerson()
	adm.do("PUT", "/api/users/"+itoa(e.userID("root")), map[string]any{"role": "admin", "email": "root@exemplo.com"})
	if code, _ := adm.do("POST", "/api/2fa/email/send", nil); code != http.StatusBadRequest {
		t.Fatalf("code = %d", code)
	}
	_, body := adm.do("GET", "/api/2fa", nil)
	if st := mustJSON[twoFactorStatus](t, body); st.EmailPossible || !st.Available {
		t.Fatalf("status = %+v", st)
	}
}

func TestPolicyForcesSetupBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t, func(c *Config) { c.Require2FA = "admins" })
	// The "root" logged in by tfEnv has no factor yet.
	adm := e.adminPerson()
	if code, body := adm.do("GET", "/api/machines", nil); code != http.StatusForbidden || !strings.Contains(string(body), errTwoFactorSetupRequired) {
		t.Fatalf("machines = %d %s", code, body)
	}
	_, body := adm.do("GET", "/api/me", nil)
	if !strings.Contains(string(body), `"must_setup_2fa":true`) {
		t.Fatalf("me = %s", body)
	}
	if code, _ := adm.do("GET", "/api/audit", nil); code != http.StatusForbidden {
		t.Fatalf("audit before setup = %d", code)
	}
	enableTOTP(t, adm)
	if code, _ := adm.do("GET", "/api/machines", nil); code != 200 {
		t.Fatalf("machines after setup = %d", code)
	}
	// The last factor cannot be removed while the policy requires one.
	if code, body := adm.do("POST", "/api/2fa/totp/disable", passwordOnly{Password: "correct-horse"}); code != http.StatusConflict {
		t.Fatalf("disable under policy = %d %s", code, body)
	}

	// Operators are outside the "admins" policy.
	_, body = adm.do("POST", "/api/users", userInput{Username: "ana", Role: store.RoleOperator})
	temp := mustJSON[userCreated](t, body).TemporaryPassword
	ana := e.newPerson()
	passwordLogin(ana, "ana", temp)
	ana.do("POST", "/api/password", passwordInput{Current: temp, New: "operador-123"})
	if code, _ := ana.do("GET", "/api/machines", nil); code != 200 {
		t.Fatalf("operator under an admins-only policy = %d", code)
	}
}

func TestDisablingNeedsThePasswordAndRemovesRecoveryCodes(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t)
	adm := e.adminPerson()
	enableTOTP(t, adm)
	if code, _ := adm.do("POST", "/api/2fa/totp/disable", passwordOnly{Password: "nope"}); code != http.StatusUnauthorized {
		t.Fatalf("disable with a wrong password = %d", code)
	}
	if code, _ := adm.do("POST", "/api/2fa/totp/disable", passwordOnly{Password: "correct-horse"}); code != http.StatusNoContent {
		t.Fatalf("disable = %d", code)
	}
	if n, _ := e.store.RecoveryCodesLeft(e.userID("root")); n != 0 {
		t.Fatalf("recovery codes left after disabling everything: %d", n)
	}
	// Plain login again.
	p := e.newPerson()
	if code, body := passwordLogin(p, "root", "correct-horse"); code != 200 || strings.Contains(string(body), `"two_factor":true`) {
		t.Fatalf("login after disabling = %d %s", code, body)
	}
}

func TestAdministratorCanResetSomeoneElsesSecondFactor(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t)
	adm := e.adminPerson()
	_, body := adm.do("POST", "/api/users", userInput{Username: "ana", Role: store.RoleOperator})
	temp := mustJSON[userCreated](t, body).TemporaryPassword
	ana := e.newPerson()
	passwordLogin(ana, "ana", temp)
	ana.do("POST", "/api/password", passwordInput{Current: temp, New: "operador-123"})
	enableTOTP(t, ana)

	// She lost the phone and the codes; the admin clears it.
	if code, _ := ana.do("POST", "/api/users/1/reset-2fa", nil); code != http.StatusForbidden {
		t.Fatalf("operator resetting 2FA = %d", code)
	}
	if code, _ := adm.do("POST", "/api/users/"+itoa(e.userID("ana"))+"/reset-2fa", nil); code != http.StatusNoContent {
		t.Fatalf("reset = %d", code)
	}
	if code, _ := ana.do("GET", "/api/machines", nil); code != http.StatusUnauthorized {
		t.Fatalf("her old session survived the reset: %d", code)
	}
	p := e.newPerson()
	if code, body := passwordLogin(p, "ana", "operador-123"); code != 200 || strings.Contains(string(body), `"two_factor":true`) {
		t.Fatalf("login after reset = %d %s", code, body)
	}
	_, body = adm.do("GET", "/api/audit", nil)
	if !strings.Contains(string(body), "user_2fa_reset") {
		t.Fatalf("audit = %s", body)
	}
}

func TestPendingLoginExpires(t *testing.T) {
	t.Parallel()
	e, _ := tfEnv(t, func(c *Config) { c.ChallengeTTL = 100 * time.Millisecond })
	secret, _ := enableTOTP(t, e.adminPerson())
	p := e.newPerson()
	_, body := passwordLogin(p, "root", "correct-horse")
	ch := mustJSON[challengeReply](t, body).Challenge
	time.Sleep(200 * time.Millisecond)
	good, _ := twofactor.Code(secret, time.Now().Add(30*time.Second))
	if code, body := verify(p, ch, "totp", good); code != http.StatusUnauthorized || !strings.Contains(string(body), "expired") {
		t.Fatalf("expired challenge = %d %s", code, body)
	}
}

func TestTwoFactorIsOffWithoutASealer(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.Sealer = nil })
	e.login()
	for _, path := range []string{"/api/2fa"} {
		if code, _ := e.do("GET", path, nil); code != http.StatusNotFound {
			t.Errorf("%s = %d, want 404", path, code)
		}
	}
	if code, _ := e.do("POST", "/api/2fa/totp/setup", nil); code != http.StatusNotFound {
		t.Errorf("setup without a sealer = %d", code)
	}
}

func TestSecondFactorEndpointsNeedLoginAndCSRF(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	anon := e.newPerson()
	for _, path := range []string{"/api/2fa", "/api/2fa/totp/setup", "/api/2fa/recovery"} {
		method := "POST"
		if path == "/api/2fa" {
			method = "GET"
		}
		if code, _ := anon.do(method, path, nil); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d", path, code)
		}
	}
	resp, err := http.Post(e.srv.URL+"/api/login/2fa/verify", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("verify without the CSRF header = %d", resp.StatusCode)
	}
}
