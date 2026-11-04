// Package signer builds and checks the request signatures of the link API
// (CONTRACT 1.1).
//
// The signed message is these fields joined with a single "\n", with no
// trailing newline:
//
//	v1
//	<METHOD in upper case>
//	<request target: path, plus "?" and the raw query if there is one>
//	<unix timestamp in seconds>
//	<nonce>
//	<lower case hex sha256 of the raw body>
//
// The signature header is "v1=" plus the standard padded base64 of the 64
// byte Ed25519 signature.
package signer

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"regexp"
	"strconv"
	"strings"
)

// Header names.
const (
	HeaderLink      = "X-SP-Link"
	HeaderTimestamp = "X-SP-Timestamp"
	HeaderNonce     = "X-SP-Nonce"
	HeaderSignature = "X-SP-Signature"
)

// Version is the scheme version, first line of the message and the
// signature prefix.
const Version = "v1"

var nonceRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// ValidNonce reports whether n is 16 to 64 characters of [A-Za-z0-9_-].
func ValidNonce(n string) bool { return nonceRe.MatchString(n) }

// NewNonce returns a fresh random 32 character nonce.
func NewNonce() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("signer: no randomness: " + err.Error())
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// BodyHash is the lower case hex sha256 of body.
func BodyHash(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Canonical returns the bytes that are signed.
func Canonical(method, target string, timestamp int64, nonce string, body []byte) []byte {
	return []byte(strings.Join([]string{
		Version,
		strings.ToUpper(method),
		target,
		strconv.FormatInt(timestamp, 10),
		nonce,
		BodyHash(body),
	}, "\n"))
}

// Sign returns the X-SP-Signature header value.
func Sign(priv ed25519.PrivateKey, method, target string, timestamp int64, nonce string, body []byte) string {
	sig := ed25519.Sign(priv, Canonical(method, target, timestamp, nonce, body))
	return Version + "=" + base64.StdEncoding.EncodeToString(sig)
}

// Errors returned by Verify.
var (
	ErrBadSignature = errors.New("bad signature")
	ErrBadNonce     = errors.New("bad nonce")
	ErrBadTimestamp = errors.New("bad timestamp")
)

// Verify checks a signature header against the request fields. It does not
// check the timestamp window or nonce reuse; that is the server's job.
func Verify(pub ed25519.PublicKey, method, target, timestamp, nonce string, body []byte, header string) error {
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || ts <= 0 {
		return ErrBadTimestamp
	}
	if !ValidNonce(nonce) {
		return ErrBadNonce
	}
	enc, ok := strings.CutPrefix(header, Version+"=")
	if !ok {
		return ErrBadSignature
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(enc)
	if err != nil || len(sig) != ed25519.SignatureSize || len(pub) != ed25519.PublicKeySize {
		return ErrBadSignature
	}
	if !ed25519.Verify(pub, Canonical(method, target, ts, nonce, body), sig) {
		return ErrBadSignature
	}
	return nil
}
