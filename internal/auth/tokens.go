// Package auth has the pieces of sign-in that aren't HTTP handlers: making
// and hashing tokens and codes, send limits, and the one read-access rule.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"math/big"
)

// NewToken returns a random 256-bit token (URL-safe text) and its hash.
// Only the hash is stored, so a copy of the database can't be used to sign
// in; the raw token exists only in the email or the cookie.
func NewToken() (raw, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err) // crypto/rand doesn't fail on supported platforms
	}
	raw = base64.RawURLEncoding.EncodeToString(b)
	return raw, Hash(raw)
}

// Hash is the stored form of a token.
func Hash(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// NewCode returns a random 6-digit code, for typing on the device where
// sign-in started when the email was opened somewhere else.
func NewCode() string {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		panic(err)
	}
	return fmt.Sprintf("%06d", n.Int64())
}

// CodeHash is the stored form of a code. It's salted with its sign-in's
// token hash so the same code on two sign-ins doesn't hash alike.
func CodeHash(tokenHash, code string) string {
	return Hash(tokenHash + ":" + code)
}

// CodeMatches compares in constant time.
func CodeMatches(tokenHash, code, stored string) bool {
	return subtle.ConstantTimeCompare([]byte(CodeHash(tokenHash, code)), []byte(stored)) == 1
}
