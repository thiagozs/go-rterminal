package twofactor

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// RFC 6238 appendix B: secret "12345678901234567890", SHA1. The RFC lists
// 8-digit codes; the 6-digit ones are their last six digits.
func TestCodeMatchesRFC6238Vectors(t *testing.T) {
	secret := b32.EncodeToString([]byte("12345678901234567890"))
	for unix, want := range map[int64]string{59: "287082", 1111111109: "081804", 1111111111: "050471", 1234567890: "005924", 2000000000: "279037"} {
		got, err := Code(secret, time.Unix(unix, 0))
		if err != nil || got != want {
			t.Errorf("t=%d: got %q, %v; want %q", unix, got, err, want)
		}
	}
}

func TestVerifyWindowAndReplay(t *testing.T) {
	secret, _ := NewSecret()
	now := time.Unix(1_700_000_000, 0)
	code, _ := Code(secret, now)

	s, ok := Verify(secret, code, now, 0)
	if !ok || s != step(now) {
		t.Fatalf("valid code rejected: %d %v", s, ok)
	}
	if _, ok := Verify(secret, code, now, s); ok {
		t.Error("the same code must not be accepted twice")
	}
	prev, _ := Code(secret, now.Add(-Period*time.Second))
	if _, ok := Verify(secret, prev, now, 0); !ok {
		t.Error("one step of clock drift should be tolerated")
	}
	old, _ := Code(secret, now.Add(-3*Period*time.Second))
	if _, ok := Verify(secret, old, now, 0); ok {
		t.Error("three steps old must be refused")
	}
	// A code from the past than the last accepted step is a replay too.
	if _, ok := Verify(secret, prev, now, step(now)); ok {
		t.Error("an older step than the last used must be refused")
	}
	for _, bad := range []string{"", "12345", "1234567", "abcdef"} {
		if _, ok := Verify(secret, bad, now, 0); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if _, ok := Verify(secret, " "+code[:3]+" "+code[3:]+" ", now, 0); !ok {
		t.Error("spaces typed by the user should be ignored")
	}
}

func TestPINsAndRecoveryCodes(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 50; i++ {
		p, err := NewPIN()
		if err != nil || len(p) != 6 || strings.Trim(p, "0123456789") != "" {
			t.Fatalf("pin = %q, %v", p, err)
		}
		seen[p] = true
	}
	if len(seen) < 40 {
		t.Errorf("PINs look not random: %d distinct of 50", len(seen))
	}
	codes, err := NewRecoveryCodes()
	if err != nil || len(codes) != RecoveryCodeCount {
		t.Fatal(codes, err)
	}
	uniq := map[string]bool{}
	for _, c := range codes {
		if len(c) != 11 || c[5] != '-' {
			t.Errorf("code format %q", c)
		}
		uniq[c] = true
	}
	if len(uniq) != len(codes) {
		t.Error("duplicate recovery codes")
	}
	// Typing variations hash the same.
	if HashSecretCode("u1", "K3F9X-M2Q7A") != HashSecretCode("u1", " k3f9x m2q7a ") {
		t.Error("normalisation failed")
	}
	if HashSecretCode("u1", "123456") == HashSecretCode("u2", "123456") {
		t.Error("the salt must change the hash")
	}
}

func TestSealerRoundTripAndKeyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "2fa.key")
	a, err := LoadSealer(path)
	if err != nil {
		t.Fatal(err)
	}
	sealed, _ := a.Seal("JBSWY3DPEHPK3PXP")
	if strings.Contains(sealed, "JBSWY") {
		t.Fatal("sealed value leaks the secret")
	}
	again, err := LoadSealer(path) // same file, same key
	if err != nil {
		t.Fatal(err)
	}
	if got, err := again.Open(sealed); err != nil || got != "JBSWY3DPEHPK3PXP" {
		t.Fatalf("open = %q, %v", got, err)
	}
	other, _ := LoadSealer(filepath.Join(t.TempDir(), "other.key"))
	if _, err := other.Open(sealed); err == nil {
		t.Fatal("a different key must not open it")
	}
	if _, err := a.Open("not base64!"); err == nil {
		t.Fatal("garbage must fail")
	}
}
