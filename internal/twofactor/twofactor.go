// Package twofactor implements the building blocks of the second login
// factor: TOTP (RFC 6238), one-time e-mail PINs, recovery codes and the
// sealing of secrets stored in the database.
package twofactor

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1" //nolint:gosec // RFC 6238 apps (Google Authenticator and others) use HMAC-SHA1
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// Period and Digits are what every authenticator app assumes.
	Period = 30
	Digits = 6
	// SkewSteps tolerates a phone clock off by one step either way.
	SkewSteps = 1
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewSecret returns a random 160-bit TOTP secret, base32 encoded.
func NewSecret() (string, error) {
	buf := make([]byte, 20)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return b32.EncodeToString(buf), nil
}

// URI is the otpauth:// link authenticator apps import (usually as a QR code).
func URI(issuer, account, secret string) string {
	label := url.PathEscape(issuer) + ":" + url.PathEscape(account)
	q := url.Values{"secret": {secret}, "issuer": {issuer}, "algorithm": {"SHA1"},
		"digits": {fmt.Sprint(Digits)}, "period": {fmt.Sprint(Period)}}
	return "otpauth://totp/" + label + "?" + q.Encode()
}

func step(t time.Time) int64 { return t.Unix() / Period }

func codeAt(secret string, counter int64) (string, error) {
	key, err := b32.DecodeString(strings.ToUpper(strings.ReplaceAll(secret, " ", "")))
	if err != nil {
		return "", err
	}
	var msg [8]byte
	binary.BigEndian.PutUint64(msg[:], uint64(counter))
	mac := hmac.New(sha1.New, key)
	mac.Write(msg[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	value := (uint32(sum[offset]&0x7f) << 24) | uint32(sum[offset+1])<<16 | uint32(sum[offset+2])<<8 | uint32(sum[offset+3])
	mod := uint32(1)
	for i := 0; i < Digits; i++ {
		mod *= 10
	}
	return fmt.Sprintf("%0*d", Digits, value%mod), nil
}

// Code is the TOTP code for secret at time t (used by tests and the setup check).
func Code(secret string, t time.Time) (string, error) { return codeAt(secret, step(t)) }

// Verify checks a code against the secret at time t, tolerating SkewSteps of
// clock drift. lastStep is the newest step already accepted for this account:
// only a strictly newer step passes, so a code cannot be replayed (for
// instance by someone who watched it being typed). It returns the step used.
func Verify(secret, code string, t time.Time, lastStep int64) (int64, bool) {
	code = strings.ReplaceAll(strings.TrimSpace(code), " ", "")
	if len(code) != Digits {
		return 0, false
	}
	matched := int64(-1)
	for d := int64(-SkewSteps); d <= SkewSteps; d++ {
		s := step(t) + d
		want, err := codeAt(secret, s)
		if err != nil {
			return 0, false
		}
		// Keep the comparison constant time and look at every candidate.
		if subtle.ConstantTimeCompare([]byte(want), []byte(code)) == 1 && s > lastStep && s > matched {
			matched = s
		}
	}
	if matched < 0 {
		return 0, false
	}
	return matched, true
}

// NewPIN returns a random 6-digit PIN for the e-mail factor.
func NewPIN() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

// HashSecretCode hashes a short-lived code (PIN) or a recovery code for
// storage. Salt ties the hash to one challenge or account.
func HashSecretCode(salt, code string) string {
	sum := sha256.Sum256([]byte(salt + "\x00" + NormalizeCode(code)))
	return hex.EncodeToString(sum[:])
}

// EqualHash compares two hex hashes in constant time.
func EqualHash(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }

// NormalizeCode ignores case, spaces and dashes users add when typing codes.
func NormalizeCode(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	return strings.NewReplacer(" ", "", "-", "").Replace(code)
}

// RecoveryCodeCount is how many one-time codes are issued at once.
const RecoveryCodeCount = 10

// NewRecoveryCodes returns fresh codes like "k3f9x-m2q7a" (50 bits each).
func NewRecoveryCodes() ([]string, error) {
	codes := make([]string, RecoveryCodeCount)
	for i := range codes {
		buf := make([]byte, 7)
		if _, err := rand.Read(buf); err != nil {
			return nil, err
		}
		s := strings.ToLower(b32.EncodeToString(buf))[:10]
		codes[i] = s[:5] + "-" + s[5:]
	}
	return codes, nil
}

// Sealer encrypts secrets (TOTP seeds) before they reach the database, so a
// leaked database or backup alone does not reveal them. The key lives in a
// separate 0600 file.
type Sealer struct{ aead cipher.AEAD }

// LoadSealer reads the key at path, creating a new random one if missing.
func LoadSealer(path string) (*Sealer, error) {
	key, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		key = make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
			return nil, fmt.Errorf("gravar chave de 2FA: %w", err)
		}
	} else if err != nil {
		return nil, err
	} else if key, err = hex.DecodeString(strings.TrimSpace(string(key))); err != nil || len(key) != 32 {
		return nil, fmt.Errorf("chave de 2FA inválida em %s", path)
	}
	return NewSealer(key)
}

// NewSealer builds a Sealer from a 32-byte key.
func NewSealer(key []byte) (*Sealer, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Sealer{aead: aead}, nil
}

// Seal encrypts plaintext; the result is safe to store as text.
func (s *Sealer) Seal(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(s.aead.Seal(nonce, nonce, []byte(plaintext), nil)), nil
}

// Open reverses Seal.
func (s *Sealer) Open(sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < s.aead.NonceSize() {
		return "", errors.New("segredo de 2FA corrompido")
	}
	plain, err := s.aead.Open(nil, raw[:s.aead.NonceSize()], raw[s.aead.NonceSize():], nil)
	if err != nil {
		return "", errors.New("não consegui abrir o segredo de 2FA (chave diferente da usada para gravá-lo?)")
	}
	return string(plain), nil
}
