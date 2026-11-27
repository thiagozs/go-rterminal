package admin

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/thiagozs/go-rterminal/internal/store"
)

// person is a second browser (own cookie jar) talking to the same server.
type person struct {
	e      *env
	client *http.Client
}

func (e *env) newPerson() *person {
	jar, _ := cookiejar.New(nil)
	return &person{e: e, client: &http.Client{Jar: jar}}
}

func (p *person) do(method, path string, body any) (int, []byte) {
	p.e.t.Helper()
	var reader io.Reader
	if body != nil {
		raw, _ := json.Marshal(body)
		reader = bytes.NewReader(raw)
	}
	req, _ := http.NewRequest(method, p.e.srv.URL+path, reader)
	req.Header.Set("X-Requested-With", "rterminal")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := p.client.Do(req)
	if err != nil {
		p.e.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, data
}

func (p *person) cookieHeader() http.Header {
	return http.Header{"Cookie": {p.client.Jar.Cookies(mustURL(p.e.srv.URL))[0].String()}}
}

func ids(t *testing.T, body []byte) []string {
	t.Helper()
	var items []struct {
		ID   any    `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &items); err != nil {
		t.Fatal(err)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.Name)
	}
	return out
}

func sameSet(a []string, b ...string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := map[string]bool{}
	for _, x := range a {
		seen[x] = true
	}
	for _, x := range b {
		if !seen[x] {
			return false
		}
	}
	return true
}

func TestOperatorOnlySeesAndOpensMachinesOfItsGroups(t *testing.T) {
	t.Parallel()
	e := newEnv(t, func(c *Config) { c.AgentsDir = t.TempDir() })
	e.login()

	var g1, g2 store.Group
	for _, g := range []struct {
		name string
		out  *store.Group
	}{{"homelab", &g1}, {"cliente", &g2}} {
		code, body := e.do("POST", "/api/groups", groupInput{Name: g.name})
		if code != http.StatusCreated {
			t.Fatalf("group = %d %s", code, body)
		}
		_ = json.Unmarshal(body, g.out)
	}
	for _, m := range []struct {
		name  string
		group *int64
	}{{"pi", &g1.ID}, {"vps-cliente", &g2.ID}, {"solta", nil}} {
		if code, body := e.do("POST", "/api/machines", machineInput{Name: m.name, GroupID: m.group}); code != http.StatusCreated {
			t.Fatalf("machine = %d %s", code, body)
		}
	}

	// Admin creates an operator for the homelab group; the temporary password is returned once.
	code, body := e.do("POST", "/api/users", userInput{Username: "ana", Role: store.RoleOperator, GroupIDs: []int64{g1.ID}})
	if code != http.StatusCreated {
		t.Fatalf("create user = %d %s", code, body)
	}
	var created userCreated
	_ = json.Unmarshal(body, &created)
	if len(created.TemporaryPassword) < store.MinPasswordLength || created.User.Role != store.RoleOperator || len(created.User.GroupIDs) != 1 {
		t.Fatalf("created = %+v", created)
	}

	ana := e.newPerson()
	if code, body := ana.do("POST", "/api/login", credentials{"ana", created.TemporaryPassword}); code != 200 || !bytes.Contains(body, []byte(`"role":"operator"`)) {
		t.Fatalf("operator login = %d %s", code, body)
	}
	if code, _ := ana.do("GET", "/api/machines", nil); code != http.StatusForbidden {
		t.Fatalf("temporary password must block the API, got %d", code)
	}
	if code, body := ana.do("POST", "/api/password", passwordInput{Current: created.TemporaryPassword, New: "a-new-password"}); code != http.StatusNoContent {
		t.Fatalf("change password = %d %s", code, body)
	}

	// Lists are filtered.
	_, body = ana.do("GET", "/api/machines", nil)
	if got := ids(t, body); !sameSet(got, "pi") {
		t.Fatalf("operator machines = %v, want only pi", got)
	}
	_, body = ana.do("GET", "/api/groups", nil)
	if got := ids(t, body); !sameSet(got, "homelab") {
		t.Fatalf("operator groups = %v, want only homelab", got)
	}
	_, body = e.do("GET", "/api/machines", nil)
	if got := ids(t, body); !sameSet(got, "pi", "vps-cliente", "solta") {
		t.Fatalf("admin machines = %v", got)
	}

	// Management endpoints are administrator only.
	for _, c := range []struct{ method, path string }{
		{"POST", "/api/groups"}, {"PUT", "/api/groups/1"}, {"DELETE", "/api/groups/1"},
		{"POST", "/api/machines"}, {"PUT", "/api/machines/pi"}, {"DELETE", "/api/machines/pi"}, {"POST", "/api/machines/pi/rotate"},
		{"GET", "/api/audit"}, {"GET", "/api/users"}, {"POST", "/api/users"}, {"PUT", "/api/users/1"},
		{"DELETE", "/api/users/1"}, {"POST", "/api/users/1/reset"},
	} {
		var body any
		if c.method == "POST" || c.method == "PUT" {
			body = map[string]string{}
		}
		if code, _ := ana.do(c.method, c.path, body); code != http.StatusForbidden {
			t.Errorf("operator %s %s = %d, want 403", c.method, c.path, code)
		}
	}
	// Agent binaries are not for operators either.
	if code, _ := ana.do("GET", "/download/agent/linux-amd64", nil); code != http.StatusUnauthorized {
		t.Errorf("operator download = %d, want 401", code)
	}

	// Terminal: other group and ungrouped machines look like they do not exist.
	for _, id := range []string{"vps-cliente", "solta", "inexistente"} {
		if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine="+id), ana.cookieHeader()); err == nil || resp.StatusCode != http.StatusNotFound {
			t.Errorf("operator terminal %s: err=%v resp=%v, want 404", id, err, resp)
		}
	}
	// Own group: reaches the machine (offline here, so 409 rather than 404).
	if _, resp, err := websocket.DefaultDialer.Dial(e.wsURL("/ws/terminal?machine=pi"), ana.cookieHeader()); err == nil || resp.StatusCode != http.StatusConflict {
		t.Errorf("operator terminal pi: err=%v resp=%v, want 409", err, resp)
	}
	_, body = e.do("GET", "/api/audit", nil)
	if !bytes.Contains(body, []byte("terminal_denied")) || !bytes.Contains(body, []byte("user_create")) {
		t.Errorf("audit lacks denial/creation: %s", body)
	}

	// Changing the groups takes effect on the next request.
	if code, body := e.do("PUT", "/api/users/"+itoa(created.User.ID), userUpdate{Role: store.RoleOperator, GroupIDs: []int64{g2.ID}}); code != http.StatusNoContent {
		t.Fatalf("update user = %d %s", code, body)
	}
	_, body = ana.do("GET", "/api/machines", nil)
	if got := ids(t, body); !sameSet(got, "vps-cliente") {
		t.Fatalf("after regroup machines = %v, want only vps-cliente", got)
	}
	// An operator with no group sees nothing.
	_ = e.store.UpdateUser(created.User.ID, store.RoleOperator, nil)
	_, body = ana.do("GET", "/api/machines", nil)
	if got := ids(t, body); len(got) != 0 {
		t.Fatalf("operator without groups sees %v", got)
	}

	// Reset ends the operator's sessions and issues a new temporary password.
	code, body = e.do("POST", "/api/users/"+itoa(created.User.ID)+"/reset", nil)
	if code != 200 || !bytes.Contains(body, []byte("temporary_password")) {
		t.Fatalf("reset = %d %s", code, body)
	}
	if code, _ := ana.do("GET", "/api/machines", nil); code != http.StatusUnauthorized {
		t.Fatalf("session should be gone after reset, got %d", code)
	}

	// Deleting the account removes the access for good.
	if code, _ := e.do("DELETE", "/api/users/"+itoa(created.User.ID), nil); code != http.StatusNoContent {
		t.Fatalf("delete user = %d", code)
	}
	if code, _ := ana.do("POST", "/api/login", credentials{"ana", "a-new-password"}); code != http.StatusUnauthorized {
		t.Fatalf("deleted user logged in: %d", code)
	}
}

func TestUserManagementGuards(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	_, body := e.do("GET", "/api/users", nil)
	var users []store.User
	_ = json.Unmarshal(body, &users)
	if len(users) != 1 || users[0].Role != store.RoleAdmin {
		t.Fatalf("users = %s", body)
	}
	self := itoa(users[0].ID)

	if code, _ := e.do("DELETE", "/api/users/"+self, nil); code != http.StatusBadRequest {
		t.Errorf("deleting yourself = %d, want 400", code)
	}
	// The only administrator cannot be demoted.
	if code, _ := e.do("PUT", "/api/users/"+self, userUpdate{Role: store.RoleOperator}); code != http.StatusConflict {
		t.Errorf("demoting last admin = %d, want 409", code)
	}
	for name, in := range map[string]userInput{
		"bad role":      {Username: "x", Role: "root"},
		"no username":   {Username: " ", Role: store.RoleOperator},
		"unknown group": {Username: "y", Role: store.RoleOperator, GroupIDs: []int64{999}},
	} {
		if code, _ := e.do("POST", "/api/users", in); code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, code)
		}
	}
	if code, _ := e.do("POST", "/api/users", userInput{Username: "root", Role: store.RoleAdmin}); code != http.StatusConflict {
		t.Errorf("duplicate username should be 409")
	}
	// A second administrator makes demoting the first possible.
	if code, _ := e.do("POST", "/api/users", userInput{Username: "dois", Role: store.RoleAdmin}); code != http.StatusCreated {
		t.Fatal("create second admin")
	}
	if code, _ := e.do("PUT", "/api/users/"+self, userUpdate{Role: store.RoleOperator}); code != http.StatusNoContent {
		t.Errorf("demoting with another admin present = %d, want 204", code)
	}
}

func TestExistingAccountsBecomeAdministrators(t *testing.T) {
	t.Parallel()
	e := newEnv(t)
	e.login()
	_, body := e.do("GET", "/api/me", nil)
	if !bytes.Contains(body, []byte(`"role":"admin"`)) {
		t.Fatalf("me = %s", body)
	}
}
