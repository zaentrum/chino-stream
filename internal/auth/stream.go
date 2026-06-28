package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"
)

// Signer verifies short-lived signed stream tokens that chino-api mints
// for <video src> URLs. Same format and shared HMAC key as chino-api's
// auth.Signer — when STREAM_SIGNING_KEY differs between the two
// services, URLs minted at chino-api won't validate here and the
// player falls back to the OIDC bearer.
//
// Token format: base64url(userID|expUnix) "." base64url(HMAC-SHA256).
type Signer struct {
	key []byte
}

func NewSigner(keyB64 string) (*Signer, error) {
	if keyB64 == "" {
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			return nil, fmt.Errorf("generate stream key: %w", err)
		}
		log.Printf("WARN: STREAM_SIGNING_KEY unset; generated ephemeral key (tokens from chino-api won't validate)")
		return &Signer{key: key}, nil
	}
	key, err := base64.StdEncoding.DecodeString(keyB64)
	if err != nil {
		return nil, fmt.Errorf("decode STREAM_SIGNING_KEY (must be base64): %w", err)
	}
	if len(key) < 16 {
		return nil, errors.New("STREAM_SIGNING_KEY must be at least 16 bytes (base64-decoded)")
	}
	return &Signer{key: key}, nil
}

// Verify mirrors chino-api/internal/auth.(*Signer).Verify exactly.
func (s *Signer) Verify(token string) (userID string, err error) {
	dot := strings.IndexByte(token, '.')
	if dot < 1 || dot == len(token)-1 {
		return "", errors.New("malformed stream token")
	}
	payload, sig := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	expected := mac.Sum(nil)
	got, decErr := base64.RawURLEncoding.DecodeString(sig)
	if decErr != nil || !hmac.Equal(expected, got) {
		return "", errors.New("invalid stream token signature")
	}
	body, decErr := base64.RawURLEncoding.DecodeString(payload)
	if decErr != nil {
		return "", errors.New("malformed stream token payload")
	}
	pipe := strings.IndexByte(string(body), '|')
	if pipe < 1 {
		return "", errors.New("malformed stream token payload (no separator)")
	}
	userID = string(body[:pipe])
	expUnix, parseErr := strconv.ParseInt(string(body[pipe+1:]), 10, 64)
	if parseErr != nil {
		return "", errors.New("malformed stream token expiry")
	}
	if time.Now().Unix() > expUnix {
		return "", errors.New("stream token expired")
	}
	return userID, nil
}
