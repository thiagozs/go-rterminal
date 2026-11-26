package main

import (
	"bytes"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/thiagozs/go-rterminal/internal/store"
	"golang.org/x/crypto/bcrypt"
)

func openTestStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	store.BcryptCost = bcrypt.MinCost
	path := filepath.Join(t.TempDir(), "t.db")
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, path
}

func TestBootstrapAdminGeneratesTemporaryPasswordAndLogsIt(t *testing.T) {
	st, _ := openTestStore(t)
	t.Setenv(envAdminPassword, "")
	var logs bytes.Buffer
	log.SetOutput(&logs)
	defer log.SetOutput(os.Stderr)

	if err := bootstrapAdmin(st, "dono"); err != nil {
		t.Fatal(err)
	}
	match := regexp.MustCompile(`senha:\s+(\S+)`).FindStringSubmatch(logs.String())
	if match == nil || !strings.Contains(logs.String(), "TEMPORARIA") {
		t.Fatalf("temporary password not logged:\n%s", logs.String())
	}
	admin, err := st.VerifyAdmin("dono", match[1])
	if err != nil {
		t.Fatalf("logged password does not work: %v", err)
	}
	if !admin.MustChangePassword {
		t.Fatal("generated password must force a change on first login")
	}
}

func TestBootstrapAdminFromEnvIsNotTemporary(t *testing.T) {
	st, _ := openTestStore(t)
	t.Setenv(envAdminPassword, "senha-do-ambiente")
	log.SetOutput(io.Discard)
	defer log.SetOutput(os.Stderr)

	if err := bootstrapAdmin(st, "dono"); err != nil {
		t.Fatal(err)
	}
	admin, err := st.VerifyAdmin("dono", "senha-do-ambiente")
	if err != nil || admin.MustChangePassword {
		t.Fatalf("env admin = %+v, %v; want usable without forced change", admin, err)
	}
	// With an admin present nothing else is created.
	if err := bootstrapAdmin(st, "outro"); err != nil {
		t.Fatal(err)
	}
	if n, _ := st.CountAdmins(); n != 1 {
		t.Fatalf("admins = %d, want 1", n)
	}
}

func TestBootstrapAdminRejectsShortEnvPassword(t *testing.T) {
	st, _ := openTestStore(t)
	t.Setenv(envAdminPassword, "curta")
	if err := bootstrapAdmin(st, "admin"); err == nil {
		t.Fatal("short password was accepted")
	}
}

func TestResetPasswordCommand(t *testing.T) {
	st, path := openTestStore(t)
	if _, err := st.CreateAdmin("admin", "original-pass", false); err != nil {
		t.Fatal(err)
	}
	st.Close() // the command opens the database itself

	cmd := &cobra.Command{}
	cmd.Flags().String("db", path, "")
	cmd.Flags().String("user", "admin", "")
	out := captureStdout(t, func() {
		if err := runResetPassword(cmd, nil); err != nil {
			t.Fatal(err)
		}
	})
	match := regexp.MustCompile(`: (\S+)\n`).FindStringSubmatch(out)
	if match == nil {
		t.Fatalf("no password printed: %q", out)
	}
	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	admin, err := st2.VerifyAdmin("admin", match[1])
	if err != nil || !admin.MustChangePassword {
		t.Fatalf("reset password = %+v, %v", admin, err)
	}
	if _, err := st2.VerifyAdmin("admin", "original-pass"); err == nil {
		t.Fatal("old password still works after reset")
	}

	cmd.Flags().Set("user", "fantasma")
	if err := runResetPassword(cmd, nil); err == nil {
		t.Fatal("reset for unknown admin should fail")
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	fn()
	w.Close()
	os.Stdout = old
	data, _ := io.ReadAll(r)
	return string(data)
}

func TestParseCIDRs(t *testing.T) {
	nets, err := parseCIDRs([]string{"172.16.0.0/12", " ", "10.0.0.0/8"})
	if err != nil || len(nets) != 2 {
		t.Fatalf("nets=%v err=%v", nets, err)
	}
	if _, err := parseCIDRs([]string{"nao-e-cidr"}); err == nil {
		t.Fatal("invalid CIDR accepted")
	}
}
