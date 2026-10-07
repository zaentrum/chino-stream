package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testKey = "c3RyZWFtLXNpZ25pbmcta2V5LWZvci10ZXN0cy0zMmI=" // 32 bytes, base64

// mint is a stream token as chino-api writes one: base64url(user|exp) "."
// base64url(HMAC-SHA256 of the first part), unpadded.
func mint(t *testing.T, user string, exp time.Time) string {
	t.Helper()
	key, err := base64.StdEncoding.DecodeString(testKey)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString([]byte(user + "|" + strconv.FormatInt(exp.Unix(), 10)))
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func signer(t *testing.T) *Signer {
	t.Helper()
	s, err := NewSigner(testKey)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestVerifyTakesAToken(t *testing.T) {
	user, err := signer(t).Verify(mint(t, "user-1", time.Now().Add(time.Hour)))
	if err != nil || user != "user-1" {
		t.Fatalf("Verify: %q, %v", user, err)
	}
	if _, err := signer(t).Verify(mint(t, "user-1", time.Now().Add(-time.Minute))); err == nil {
		t.Error("an expired token verified")
	}
}

// The last character of a 32-byte MAC's 43 carries four of its bits and two
// that are zero. Set one of those and a lenient decoder reads the same MAC:
// that spelling is refused, whichever of the three it is.
func TestVerifyRefusesAnotherSpellingOfItsMAC(t *testing.T) {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	tok := mint(t, "user-1", time.Now().Add(time.Hour))
	dot := strings.IndexByte(tok, '.')
	sig := tok[dot+1:]
	last := strings.IndexByte(alphabet, sig[len(sig)-1])
	if len(sig) != 43 || last%4 != 0 {
		t.Fatalf("not a canonical 32-byte MAC: %q", sig)
	}
	mac, _ := base64.RawURLEncoding.DecodeString(sig)
	for _, bits := range []int{1, 2, 3} {
		other := sig[:len(sig)-1] + string(alphabet[last|bits])
		same, err := base64.RawURLEncoding.DecodeString(other)
		if err != nil || !bytes.Equal(same, mac) {
			t.Fatalf("%q is no other spelling of the MAC: %v", other, err)
		}
		if user, err := signer(t).Verify(tok[:dot+1] + other); err == nil {
			t.Errorf("the MAC spelled %q verified (user %q)", other, user)
		}
	}
	if _, err := signer(t).Verify(tok); err != nil {
		t.Errorf("the token itself: %v", err)
	}
}
