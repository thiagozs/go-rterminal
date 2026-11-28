package admin

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/thiagozs/go-rterminal/internal/store"
)

func getPrefs(t *testing.T, p interface {
	do(method, path string, body any) (int, []byte)
}) map[string]any {
	t.Helper()
	code, body := p.do("GET", "/api/prefs", nil)
	if code != http.StatusOK {
		t.Fatalf("GET prefs = %d %s", code, body)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

type adminPerson struct{ e *env }

func (a adminPerson) do(method, path string, body any) (int, []byte) {
	return a.e.do(method, path, body)
}

func TestPrefsAreSavedPerAccount(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	me := adminPerson{e}

	if got := getPrefs(t, me); len(got) != 0 {
		t.Fatalf("a new account has preferences: %v", got)
	}
	code, body := e.do("PUT", "/api/prefs", map[string]any{"autoCloseTransfers": false, "autoCloseSeconds": 10, "uploadParallel": 4})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	got := getPrefs(t, me)
	if got["autoCloseTransfers"] != false || got["autoCloseSeconds"] != float64(10) || got["uploadParallel"] != float64(4) {
		t.Fatalf("saved = %v", got)
	}

	// A later partial update keeps what was saved before.
	e.do("PUT", "/api/prefs", map[string]any{"skipGit": true})
	got = getPrefs(t, me)
	if got["skipGit"] != true || got["uploadParallel"] != float64(4) || len(got) != 4 {
		t.Fatalf("after a partial update = %v", got)
	}

	// Another account has its own.
	code, body = e.do("POST", "/api/users", userInput{Username: "ana", Role: store.RoleOperator})
	var created userCreated
	_ = json.Unmarshal(body, &created)
	ana := e.newPerson()
	ana.do("POST", "/api/login", credentials{"ana", created.TemporaryPassword})
	ana.do("POST", "/api/password", passwordInput{Current: created.TemporaryPassword, New: "operador-123"})
	if got := getPrefs(t, ana); len(got) != 0 {
		t.Fatalf("ana sees someone else's preferences: %v", got)
	}
	ana.do("PUT", "/api/prefs", map[string]any{"uploadParallel": 2})
	if got := getPrefs(t, me); got["uploadParallel"] != float64(4) {
		t.Fatalf("ana's change leaked into the first account: %v", got)
	}
}

func TestPrefsKeepOnlyKnownValidValues(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	code, body := e.do("PUT", "/api/prefs", map[string]any{
		"autoCloseSeconds":   5000,                   // out of range
		"uploadParallel":     "many",                 // wrong type
		"skipGit":            "yes",                  // wrong type
		"skipJunk":           true,                   // fine
		"role":               "admin",                // unknown
		"payload":            map[string]any{"x": 1}, // unknown
		"autoCloseTransfers": nil,
	})
	if code != http.StatusOK {
		t.Fatalf("PUT = %d %s", code, body)
	}
	got := getPrefs(t, adminPerson{e})
	if len(got) != 1 || got["skipJunk"] != true {
		t.Fatalf("only skipJunk should have been kept, got %v", got)
	}
	if code, _ := e.do("PUT", "/api/prefs", nil); code != http.StatusBadRequest && code != http.StatusOK {
		t.Fatalf("an empty body = %d", code)
	}
}

func TestPrefsNeedALogin(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	anon := e.newPerson()
	for _, m := range []string{"GET", "PUT"} {
		if code, _ := anon.do(m, "/api/prefs", map[string]any{}); code != http.StatusUnauthorized {
			t.Errorf("anonymous %s = %d, want 401", m, code)
		}
	}
}
