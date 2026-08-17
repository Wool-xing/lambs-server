package auth

import (
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func mustHash(t *testing.T, s string) string {
	t.Helper()
	h, err := bcrypt.GenerateFromPassword([]byte(s), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt: %v", err)
	}
	return string(h)
}

// TestVerifyPassword covers the R7 salted contract and both legacy shapes.
func TestVerifyPassword(t *testing.T) {
	const pwd = "admin123"
	const salt = "a1b2c3d4e5f60718293a4b5c6d7e8f90"

	cases := []struct {
		name       string
		stored     string // bcrypt input at storage time
		payload    string // what the client sends now
		salt       string // account salt
		wantOK     bool
		wantLegacy bool
	}{
		{
			name:       "new contract salted payload",
			stored:     mustHash(t, sha256Hex(pwd+salt)),
			payload:    sha256Hex(pwd + salt),
			salt:       salt,
			wantOK:     true,
			wantLegacy: false,
		},
		{
			name:       "legacy row, new client empty-salt payload",
			stored:     mustHash(t, sha256Hex(pwd)),
			payload:    sha256Hex(pwd),
			salt:       "",
			wantOK:     true,
			wantLegacy: false,
		},
		{
			name:       "legacy row, legacy plaintext client",
			stored:     mustHash(t, sha256Hex(pwd)),
			payload:    pwd,
			salt:       "",
			wantOK:     true,
			wantLegacy: true,
		},
		{
			name:       "upgraded row, legacy plaintext client still works",
			stored:     mustHash(t, sha256Hex(pwd+salt)),
			payload:    pwd,
			salt:       salt,
			wantOK:     true,
			wantLegacy: true,
		},
		{
			name:       "wrong password rejected",
			stored:     mustHash(t, sha256Hex(pwd+salt)),
			payload:    sha256Hex("wrong" + salt),
			salt:       salt,
			wantOK:     false,
			wantLegacy: false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ok, legacy := VerifyPassword(c.stored, c.payload, c.salt)
			if ok != c.wantOK || legacy != c.wantLegacy {
				t.Errorf("got ok=%v legacy=%v want ok=%v legacy=%v", ok, legacy, c.wantOK, c.wantLegacy)
			}
		})
	}
}

func TestIsSHA256Hex(t *testing.T) {
	if !IsSHA256Hex(sha256Hex("x")) {
		t.Error("64-hex should pass")
	}
	for _, s := range []string{"", "abc", sha256Hex("x") + "g", "G" + sha256Hex("x")[1:]} {
		if IsSHA256Hex(s) {
			t.Errorf("%q should fail", s)
		}
	}
}

func TestIsSaltHex(t *testing.T) {
	if !IsSaltHex(NewSaltHex()) {
		t.Error("32-hex salt should pass")
	}
	if IsSaltHex(sha256Hex("x")) {
		t.Error("64-hex is not a valid salt (R7 regression: salt validation used the 64-hex matcher)")
	}
	if IsSaltHex("zz" + NewSaltHex()[2:]) {
		t.Error("non-hex salt should fail")
	}
}
