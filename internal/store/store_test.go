package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	BcryptCost = bcrypt.MinCost
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{
		"Meu Notebook":     "meu-notebook",
		"  Raspberry Pi 4": "raspberry-pi-4",
		"srv__01!!":        "srv-01",
		"***":              "",
	} {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAdminAuthAndSessions(t *testing.T) {
	s := newTestStore(t)
	if _, err := s.CreateAdmin("root", "short", false); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short password err = %v, want ErrInvalid", err)
	}
	admin, err := s.CreateAdmin("root", "correct-horse", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateAdmin("root", "another-pass", false); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate admin err = %v", err)
	}
	if _, err := s.VerifyAdmin("root", "wrong"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong password err = %v", err)
	}
	if _, err := s.VerifyAdmin("ghost", "whatever"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("unknown user err = %v", err)
	}
	if got, err := s.VerifyAdmin("root", "correct-horse"); err != nil || got.ID != admin.ID {
		t.Fatalf("verify = %+v, %v", got, err)
	}

	token, err := s.CreateWebSession(admin.ID, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := s.LookupWebSession(token); err != nil || got.Username != "root" {
		t.Fatalf("lookup = %+v, %v", got, err)
	}
	if err := s.DeleteWebSession(token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LookupWebSession(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted session err = %v", err)
	}
	expired, _ := s.CreateWebSession(admin.ID, -time.Minute)
	if _, err := s.LookupWebSession(expired); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired session err = %v", err)
	}
}

func TestMachinesAndGroups(t *testing.T) {
	s := newTestStore(t)
	group, err := s.CreateGroup("homelab", "casa")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.CreateGroup("homelab", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate group err = %v", err)
	}

	m, token, err := s.CreateMachine("Meu Notebook", &group.ID)
	if err != nil {
		t.Fatal(err)
	}
	if m.ID != "meu-notebook" || token == "" || m.GroupID == nil || *m.GroupID != group.ID {
		t.Fatalf("machine = %+v token=%q", m, token)
	}
	// Same display name gets a distinct id.
	dup, _, err := s.CreateMachine("Meu Notebook", nil)
	if err != nil || dup.ID == m.ID {
		t.Fatalf("duplicate name: %+v, %v", dup, err)
	}
	if _, _, err := s.CreateMachine("x", func() *int64 { v := int64(999); return &v }()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown group err = %v", err)
	}

	if got, err := s.MachineByToken(token); err != nil || got.ID != m.ID {
		t.Fatalf("by token = %+v, %v", got, err)
	}
	if _, err := s.MachineByToken("nope"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bad token err = %v", err)
	}

	// Rotation invalidates the previous credential.
	fresh, err := s.RotateMachineToken(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.MachineByToken(token); !errors.Is(err, ErrNotFound) {
		t.Fatal("old credential still valid after rotation")
	}
	if _, err := s.MachineByToken(fresh); err != nil {
		t.Fatal(err)
	}

	if err := s.TouchMachine(m.ID, "host", "linux", "arm64", "v1"); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetMachine(m.ID)
	if got.LastSeen == nil || got.Hostname != "host" || got.Arch != "arm64" {
		t.Fatalf("touch not recorded: %+v", got)
	}

	// Deleting a group keeps its machines, ungrouped.
	if err := s.DeleteGroup(group.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetMachine(m.ID); got.GroupID != nil {
		t.Fatalf("machine still grouped: %+v", got)
	}
	if err := s.DeleteMachine(m.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteMachine(m.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete err = %v", err)
	}
}

func TestAuditNewestFirst(t *testing.T) {
	s := newTestStore(t)
	for _, action := range []string{"a", "b", "c"} {
		if err := s.AddAudit("admin", action, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.ListAudit(2)
	if err != nil || len(entries) != 2 || entries[0].Action != "c" {
		t.Fatalf("audit = %+v, %v", entries, err)
	}
}

func TestChangePasswordClearsTemporaryFlagAndOtherSessions(t *testing.T) {
	s := newTestStore(t)
	admin, err := s.CreateAdmin("root", "temporary-pass", true)
	if err != nil || !admin.MustChangePassword {
		t.Fatalf("create = %+v, %v", admin, err)
	}
	verified, _ := s.VerifyAdmin("root", "temporary-pass")
	if !verified.MustChangePassword {
		t.Fatal("temporary flag lost on verify")
	}
	mine, _ := s.CreateWebSession(admin.ID, time.Hour)
	other, _ := s.CreateWebSession(admin.ID, time.Hour)

	if err := s.ChangePassword(admin.ID, "wrong", "brand-new-pass", mine); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("wrong current err = %v", err)
	}
	if err := s.ChangePassword(admin.ID, "temporary-pass", "short", mine); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short new err = %v", err)
	}
	if err := s.ChangePassword(admin.ID, "temporary-pass", "temporary-pass", mine); !errors.Is(err, ErrInvalid) {
		t.Fatalf("same password err = %v", err)
	}
	if err := s.ChangePassword(admin.ID, "temporary-pass", "brand-new-pass", mine); err != nil {
		t.Fatal(err)
	}

	if _, err := s.VerifyAdmin("root", "temporary-pass"); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("old password still works")
	}
	if got, err := s.VerifyAdmin("root", "brand-new-pass"); err != nil || got.MustChangePassword {
		t.Fatalf("new password verify = %+v, %v", got, err)
	}
	if _, err := s.LookupWebSession(mine); err != nil {
		t.Fatalf("own session should survive: %v", err)
	}
	if _, err := s.LookupWebSession(other); !errors.Is(err, ErrNotFound) {
		t.Fatal("other session should have ended")
	}
}

func TestResetAdminPassword(t *testing.T) {
	s := newTestStore(t)
	admin, _ := s.CreateAdmin("root", "original-pass", false)
	session, _ := s.CreateWebSession(admin.ID, time.Hour)

	if err := s.ResetAdminPassword("ghost", "temporary-pass"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown user err = %v", err)
	}
	if err := s.ResetAdminPassword("root", "short"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("short temp err = %v", err)
	}
	if err := s.ResetAdminPassword("root", "temporary-pass"); err != nil {
		t.Fatal(err)
	}
	if got, err := s.VerifyAdmin("root", "temporary-pass"); err != nil || !got.MustChangePassword {
		t.Fatalf("after reset = %+v, %v", got, err)
	}
	if _, err := s.LookupWebSession(session); !errors.Is(err, ErrNotFound) {
		t.Fatal("sessions should end after reset")
	}
}

// A database created before must_change_password existed is upgraded in place.
func TestOpenMigratesOlderDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE admins (
		id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL, created_at INTEGER NOT NULL);
		INSERT INTO admins(username, password_hash, created_at) VALUES('legacy', 'x', 1);`); err != nil {
		t.Fatal(err)
	}
	raw.Close()

	BcryptCost = bcrypt.MinCost
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open old db: %v", err)
	}
	defer s.Close()
	if n, _ := s.CountAdmins(); n != 1 {
		t.Fatalf("admins = %d, want the legacy row kept", n)
	}
	if _, err := s.CreateAdmin("novo", "some-password", true); err != nil {
		t.Fatalf("create after migration: %v", err)
	}
	// Reopening an already migrated database must not fail.
	s.Close()
	s2, err := Open(path)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	s2.Close()
}

// A database from before roles existed must keep working, with its accounts
// promoted to administrators (they had full access).
func TestMigrationGivesExistingAccountsTheAdminRole(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := old.Exec(`CREATE TABLE admins (
		id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL, created_at INTEGER NOT NULL, must_change_password INTEGER NOT NULL DEFAULT 0);
		INSERT INTO admins(username, password_hash, created_at) VALUES('legacy', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	users, err := s.ListUsers()
	if err != nil || len(users) != 1 || users[0].Role != RoleAdmin {
		t.Fatalf("users = %+v, %v", users, err)
	}
}

func TestOperatorAccessFollowsGroups(t *testing.T) {
	s := newTestStore(t)
	g, _ := s.CreateGroup("homelab", "")
	other, _ := s.CreateGroup("cliente", "")
	inside, _, _ := s.CreateMachine("pi", &g.ID)
	outside, _, _ := s.CreateMachine("vps", &other.ID)
	loose, _, _ := s.CreateMachine("solta", nil)
	op, err := s.CreateUser("ana", "password-1", RoleOperator, []int64{g.ID}, false)
	if err != nil {
		t.Fatal(err)
	}
	admin, _ := s.CreateAdmin("root", "password-1", false)

	for _, c := range []struct {
		who  *Admin
		m    *Machine
		want bool
	}{{op, inside, true}, {op, outside, false}, {op, loose, false}, {admin, outside, true}, {admin, loose, true}} {
		got, err := s.CanAccessMachine(c.who, c.m)
		if err != nil || got != c.want {
			t.Errorf("%s on %s = %v, %v; want %v", c.who.Username, c.m.Name, got, err, c.want)
		}
	}
	// Deleting the group removes the access (cascade), without touching the account.
	if err := s.DeleteGroup(g.ID); err != nil {
		t.Fatal(err)
	}
	if ids, _ := s.UserGroupIDs(op.ID); len(ids) != 0 {
		t.Errorf("groups after delete = %v", ids)
	}
	if err := s.DeleteUser(admin.ID); !errors.Is(err, ErrLastAdmin) {
		t.Errorf("deleting last admin = %v, want ErrLastAdmin", err)
	}
}

func TestTwoFactorStateAndReplayGuard(t *testing.T) {
	s := newTestStore(t)
	u, _ := s.CreateAdmin("ana", "password-1", false)
	if u.HasTwoFactor() {
		t.Fatal("a new account has no second factor")
	}
	if err := s.SetTOTPPending(u.ID, "sealed-pending"); err != nil {
		t.Fatal(err)
	}
	if p, _ := s.TOTPPending(u.ID); p != "sealed-pending" {
		t.Fatalf("pending = %q", p)
	}
	if err := s.EnableTOTP(u.ID, "sealed-secret", 100); err != nil {
		t.Fatal(err)
	}
	sealed, last, enabled, _ := s.TOTPSecret(u.ID)
	if sealed != "sealed-secret" || last != 100 || !enabled {
		t.Fatalf("secret = %q %d %v", sealed, last, enabled)
	}
	if p, _ := s.TOTPPending(u.ID); p != "" {
		t.Fatal("pending secret must be cleared once enabled")
	}
	// Only a strictly newer step advances: the guard behind "a code works once".
	for _, c := range []struct {
		step int64
		want bool
	}{{100, false}, {99, false}, {101, true}, {101, false}, {102, true}} {
		if got, _ := s.AdvanceTOTPStep(u.ID, c.step); got != c.want {
			t.Errorf("advance(%d) = %v, want %v", c.step, got, c.want)
		}
	}
	got, _ := s.VerifyAdmin("ana", "password-1")
	if !got.TOTPEnabled || !got.HasTwoFactor() {
		t.Fatalf("login view = %+v", got)
	}
}

func TestRecoveryCodesAreSingleUseAndReplaceable(t *testing.T) {
	s := newTestStore(t)
	u, _ := s.CreateAdmin("ana", "password-1", false)
	if err := s.ReplaceRecoveryCodes(u.ID, []string{"h1", "h2", "h3"}); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.UseRecoveryCode(u.ID, "h1"); !ok {
		t.Fatal("first use refused")
	}
	if ok, _ := s.UseRecoveryCode(u.ID, "h1"); ok {
		t.Fatal("a recovery code worked twice")
	}
	if ok, _ := s.UseRecoveryCode(u.ID+1, "h2"); ok {
		t.Fatal("another account's code worked")
	}
	if n, _ := s.RecoveryCodesLeft(u.ID); n != 2 {
		t.Fatalf("left = %d", n)
	}
	_ = s.ReplaceRecoveryCodes(u.ID, []string{"n1"})
	if ok, _ := s.UseRecoveryCode(u.ID, "h2"); ok {
		t.Fatal("an old code survived the replacement")
	}
}

func TestClearTwoFactorEndsSessionsAndForgetsEverything(t *testing.T) {
	s := newTestStore(t)
	u, _ := s.CreateAdmin("ana", "password-1", false)
	_ = s.SetEmail(u.ID, "ana@x.co")
	_ = s.SetEmailPIN(u.ID, true)
	_ = s.EnableTOTP(u.ID, "sealed", 5)
	_ = s.ReplaceRecoveryCodes(u.ID, []string{"h1"})
	token, _ := s.CreateWebSession(u.ID, time.Hour)

	if err := s.ClearTwoFactor(u.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetUser(u.ID)
	if got.HasTwoFactor() || got.Email != "ana@x.co" {
		t.Fatalf("after clear = %+v (the e-mail address itself must stay)", got)
	}
	if n, _ := s.RecoveryCodesLeft(u.ID); n != 0 {
		t.Fatal("recovery codes left")
	}
	if _, err := s.LookupWebSession(token); !errors.Is(err, ErrNotFound) {
		t.Fatalf("session survived: %v", err)
	}
	if err := s.ClearTwoFactor(9999); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown account: %v", err)
	}
	// Clearing the address also switches the e-mail factor off.
	_ = s.SetEmailPIN(u.ID, true)
	_ = s.SetEmail(u.ID, "")
	if got, _ := s.GetUser(u.ID); got.EmailPIN {
		t.Fatal("e-mail factor stayed on without an address")
	}
}

func TestMigrationAddsTwoFactorColumnsToOlderDatabases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	old, _ := sql.Open("sqlite", "file:"+path)
	if _, err := old.Exec(`CREATE TABLE admins (
		id INTEGER PRIMARY KEY AUTOINCREMENT, username TEXT NOT NULL UNIQUE, password_hash TEXT NOT NULL,
		created_at INTEGER NOT NULL, must_change_password INTEGER NOT NULL DEFAULT 0, role TEXT NOT NULL DEFAULT 'admin');
		INSERT INTO admins(username, password_hash, created_at) VALUES('legacy', 'x', 1)`); err != nil {
		t.Fatal(err)
	}
	old.Close()
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	u, err := s.UserByName("legacy")
	if err != nil || u.HasTwoFactor() || u.Email != "" {
		t.Fatalf("legacy = %+v, %v", u, err)
	}
	if err := s.EnableTOTP(u.ID, "x", 1); err != nil {
		t.Fatal(err)
	}
}
