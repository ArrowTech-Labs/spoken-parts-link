// Package identity holds the link's Ed25519 key and its local secret.
//
// On first start the link creates two files, each with mode 0600:
//
//	<dir>/key           the 32 byte Ed25519 seed (the private key)
//	<dir>/local_secret  32 random bytes, mixed with the server data key to
//	                    derive the key that encrypts printer access codes
//
// The private key never leaves the device. Only the public key is sent, at
// pairing.
package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
)

// File names inside the identity directory.
const (
	KeyFile    = "key"
	SecretFile = "local_secret"
)

// SecretLen is the size of the local secret.
const SecretLen = 32

// Identity is the link's key pair and local secret.
type Identity struct {
	Private     ed25519.PrivateKey
	Public      ed25519.PublicKey
	LocalSecret []byte
}

// LoadOrCreate reads the identity from dir, creating any missing part.
// created is true when a new key was generated.
func LoadOrCreate(dir string) (id *Identity, created bool, err error) {
	if err := fsutil.EnsureDir(dir); err != nil {
		return nil, false, err
	}
	seed, err := readExact(filepath.Join(dir, KeyFile), ed25519.SeedSize)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		seed = make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, false, err
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, KeyFile), seed); err != nil {
			return nil, false, err
		}
		created = true
	case err != nil:
		return nil, false, fmt.Errorf("identity key: %w", err)
	}
	secret, err := readExact(filepath.Join(dir, SecretFile), SecretLen)
	switch {
	case errors.Is(err, fs.ErrNotExist) || created:
		// A new key always gets a new local secret.
		secret = make([]byte, SecretLen)
		if _, err := rand.Read(secret); err != nil {
			return nil, false, err
		}
		if err := fsutil.WriteFileAtomic(filepath.Join(dir, SecretFile), secret); err != nil {
			return nil, false, err
		}
	case err != nil:
		return nil, false, fmt.Errorf("local secret: %w", err)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	clear(seed)
	return &Identity{
		Private:     priv,
		Public:      priv.Public().(ed25519.PublicKey),
		LocalSecret: secret,
	}, created, nil
}

func readExact(path string, n int) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != n {
		clear(b)
		return nil, fmt.Errorf("%s has the wrong size", filepath.Base(path))
	}
	return b, nil
}

// Wipe deletes the key and the local secret.
func Wipe(dir string) error {
	err1 := fsutil.Wipe(filepath.Join(dir, KeyFile))
	err2 := fsutil.Wipe(filepath.Join(dir, SecretFile))
	return errors.Join(err1, err2)
}

// Zero clears the in-memory key material.
func (id *Identity) Zero() {
	if id == nil {
		return
	}
	clear(id.Private)
	clear(id.LocalSecret)
}

// PublicKeyB64 is the raw 32 byte public key in standard padded base64.
func (id *Identity) PublicKeyB64() string {
	return base64.StdEncoding.EncodeToString(id.Public)
}

// Fingerprint is "sha256:" plus the first 16 hex characters of
// sha256(raw public key) (CONTRACT 1.1).
func Fingerprint(pub ed25519.PublicKey) string {
	sum := sha256.Sum256(pub)
	return "sha256:" + hex.EncodeToString(sum[:])[:16]
}

// CheckPrefix is prepended to the public key when computing the check number.
const CheckPrefix = "sp-link-check-v1"

// CheckNumber is sha256("sp-link-check-v1" + raw public key), first 4 bytes
// as a big endian uint32, mod 10000, zero padded to 4 digits (CONTRACT 1.1).
func CheckNumber(pub ed25519.PublicKey) string {
	h := sha256.New()
	h.Write([]byte(CheckPrefix))
	h.Write(pub)
	sum := h.Sum(nil)
	return fmt.Sprintf("%04d", binary.BigEndian.Uint32(sum[:4])%10000)
}
