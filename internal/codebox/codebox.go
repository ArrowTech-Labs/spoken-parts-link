// Package codebox opens printer access codes that an owner typed on
// spokenparts.com, sealed in the browser to this link (scheme sp-code-v1).
//
// The link has a P-256 ECDH key pair, separate from its Ed25519 signing key.
// The sender makes an ephemeral P-256 key pair and computes
//
//	shared = ECDH(eph_priv, link_pub)                  (32 byte X coordinate)
//	key    = HKDF-SHA256(ikm = shared, salt = epk, info = "sp-code-v1", 32)
//	ct     = AES-256-GCM(key, nonce, code, AAD = "sp-code-v1|" + serial + "|" + key_id)
//
// where epk is the ephemeral public key as an uncompressed SEC1 point (65
// bytes) and key_id is the first 16 hex characters of sha256(link public
// key). The AAD binds the printer serial and the link key, so a code sealed
// for one printer or one link cannot be used for another.
//
// Spoken Parts only stores and forwards the envelope; it cannot open it.
package codebox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
)

// Scheme is the protocol id.
const Scheme = "sp-code-v1"

// Exact sizes of the decoded envelope fields.
const (
	PublicKeyLen  = 65 // uncompressed SEC1 P-256 point
	NonceLen      = 12
	CodeLen       = 8
	CiphertextLen = CodeLen + 16 // code and GCM tag
	PrivateKeyLen = 32
	keyLen        = 32
)

// ErrBadEnvelope means the envelope is not for this key, has the wrong
// shape, or does not open (wrong serial, wrong key, tampered).
var ErrBadEnvelope = errors.New("the sealed access code could not be opened")

var b64 = base64.RawURLEncoding.Strict()

// Key is the link's code box key pair.
type Key struct {
	priv *ecdh.PrivateKey
	pub  []byte
	id   string
}

// Generate makes a new key pair.
func Generate() (*Key, error) {
	priv, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return fromPrivate(priv), nil
}

// FromBytes loads a key from its 32 byte private scalar.
func FromBytes(scalar []byte) (*Key, error) {
	priv, err := ecdh.P256().NewPrivateKey(scalar)
	if err != nil {
		return nil, err
	}
	return fromPrivate(priv), nil
}

func fromPrivate(priv *ecdh.PrivateKey) *Key {
	pub := priv.PublicKey().Bytes()
	return &Key{priv: priv, pub: pub, id: KeyID(pub)}
}

// Bytes returns a copy of the 32 byte private scalar. The caller clears it.
func (k *Key) Bytes() []byte { return k.priv.Bytes() }

// PublicBytes is the uncompressed SEC1 public key (65 bytes).
func (k *Key) PublicBytes() []byte { return append([]byte(nil), k.pub...) }

// PublicKey is the public key in base64url without padding.
func (k *Key) PublicKey() string { return b64.EncodeToString(k.pub) }

// ID is the key id: first 16 lowercase hex characters of sha256(public key).
func (k *Key) ID() string { return k.id }

// KeyID computes a key id from raw public key bytes.
func KeyID(pub []byte) string {
	sum := sha256.Sum256(pub)
	return hex.EncodeToString(sum[:])[:16]
}

// Envelope is one sealed access code. Every field except Scheme and KeyID is
// base64url without padding.
type Envelope struct {
	Scheme     string
	KeyID      string
	EPK        string
	Nonce      string
	Ciphertext string
}

// AAD is the additional data for serial and key id.
func AAD(serial, keyID string) []byte {
	return []byte(Scheme + "|" + serial + "|" + keyID)
}

// Open checks env and opens it for serial. It returns the 8 byte plaintext,
// which the caller must register with the log redactor before doing
// anything else with it, then check it is a valid access code. Any problem
// with the envelope returns ErrBadEnvelope and no detail.
func (k *Key) Open(env Envelope, serial string) ([]byte, error) {
	if env.Scheme != Scheme || env.KeyID != k.id {
		return nil, ErrBadEnvelope
	}
	epk, err1 := decode(env.EPK, PublicKeyLen)
	nonce, err2 := decode(env.Nonce, NonceLen)
	ct, err3 := decode(env.Ciphertext, CiphertextLen)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, ErrBadEnvelope
	}
	eph, err := ecdh.P256().NewPublicKey(epk)
	if err != nil {
		return nil, ErrBadEnvelope
	}
	shared, err := k.priv.ECDH(eph)
	if err != nil {
		return nil, ErrBadEnvelope
	}
	defer clear(shared)
	aead, key, err := newAEAD(shared, epk)
	clear(key)
	if err != nil {
		return nil, ErrBadEnvelope
	}
	plain, err := aead.Open(nil, nonce, ct, AAD(serial, k.id))
	if err != nil || len(plain) != CodeLen {
		clear(plain)
		return nil, ErrBadEnvelope
	}
	return plain, nil
}

// Seal is the sender's side, as the website does it with WebCrypto. The link
// itself never seals; this is the reference for tests.
func Seal(linkPub []byte, serial string, code []byte) (Envelope, error) {
	pub, err := ecdh.P256().NewPublicKey(linkPub)
	if err != nil {
		return Envelope{}, err
	}
	eph, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return Envelope{}, err
	}
	nonce := make([]byte, NonceLen)
	if _, err := rand.Read(nonce); err != nil {
		return Envelope{}, err
	}
	return SealWith(eph, nonce, pub.Bytes(), serial, code)
}

// SealWith is Seal with a given ephemeral key and nonce (fixed test vectors).
func SealWith(eph *ecdh.PrivateKey, nonce, linkPub []byte, serial string, code []byte) (Envelope, error) {
	pub, err := ecdh.P256().NewPublicKey(linkPub)
	if err != nil {
		return Envelope{}, err
	}
	shared, err := eph.ECDH(pub)
	if err != nil {
		return Envelope{}, err
	}
	defer clear(shared)
	epk := eph.PublicKey().Bytes()
	aead, key, err := newAEAD(shared, epk)
	clear(key)
	if err != nil {
		return Envelope{}, err
	}
	id := KeyID(pub.Bytes())
	ct := aead.Seal(nil, nonce, code, AAD(serial, id))
	return Envelope{
		Scheme:     Scheme,
		KeyID:      id,
		EPK:        b64.EncodeToString(epk),
		Nonce:      b64.EncodeToString(nonce),
		Ciphertext: b64.EncodeToString(ct),
	}, nil
}

func newAEAD(shared, epk []byte) (cipher.AEAD, []byte, error) {
	key, err := hkdf.Key(sha256.New, shared, epk, Scheme, keyLen)
	if err != nil {
		return nil, nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, key, err
	}
	aead, err := cipher.NewGCM(block)
	return aead, key, err
}

func decode(s string, n int) ([]byte, error) {
	if len(s) != b64.EncodedLen(n) {
		return nil, ErrBadEnvelope
	}
	b, err := b64.DecodeString(s)
	if err != nil || len(b) != n {
		return nil, ErrBadEnvelope
	}
	return b, nil
}
