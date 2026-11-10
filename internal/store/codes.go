// Package store keeps the link's files under /data: the encrypted access
// code file and the plain state files.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sort"
	"sync"

	"github.com/ArrowTech-Labs/spoken-parts-link/internal/fsutil"
)

// Key derivation parameters (owner decision; see THREAT-MODEL.md).
const (
	KDFSalt = "spoken-parts-link"
	KDFInfo = "access-codes-v1"
	KeyLen  = 32
)

// fileMagic starts every codes.enc file: "SPLC" and format version 1.
// It is also the AES-GCM additional data.
var fileMagic = []byte{'S', 'P', 'L', 'C', 1}

// Errors.
var (
	ErrLocked     = errors.New("access codes are locked until Spoken Parts sends the data key")
	ErrCannotOpen = errors.New("the access code file could not be opened with this key")
	ErrBadKey     = errors.New("bad key length")
)

// DeriveKey returns HKDF-SHA256(ikm = localSecret || dataKey,
// salt = "spoken-parts-link", info = "access-codes-v1"), 32 bytes.
// The result must only ever live in memory.
func DeriveKey(localSecret, dataKey []byte) ([]byte, error) {
	return DeriveKeyFor(localSecret, dataKey, KDFInfo)
}

// HAInfo is the HKDF info for the Home Assistant settings file (the webhook
// id), so it has its own key.
const HAInfo = "ha-webhook-v1"

// DeriveKeyFor is DeriveKey with another info string.
func DeriveKeyFor(localSecret, dataKey []byte, info string) ([]byte, error) {
	if len(localSecret) != 32 || len(dataKey) != 32 {
		return nil, ErrBadKey
	}
	ikm := make([]byte, 0, 64)
	ikm = append(ikm, localSecret...)
	ikm = append(ikm, dataKey...)
	defer clear(ikm)
	return hkdf.Key(sha256.New, ikm, []byte(KDFSalt), info, KeyLen)
}

// Codes is the encrypted map of printer serial to access code.
type Codes struct {
	path string

	mu      sync.Mutex
	key     []byte // nil while locked
	codes   map[string]string
	damaged bool
}

// NewCodes returns a locked store for the file at path.
func NewCodes(path string) *Codes {
	return &Codes{path: path, codes: map[string]string{}}
}

// Path is the file location.
func (c *Codes) Path() string { return c.path }

// Unlock sets the key and loads the file if it exists. If the file exists
// but cannot be opened with key, the store is still unlocked (empty) so new
// codes can be saved, Damaged reports true, and ErrCannotOpen is returned.
// Unlock takes its own copy of key.
func (c *Codes) Unlock(key []byte) error {
	if len(key) != KeyLen {
		return ErrBadKey
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.zeroKeyLocked()
	c.key = append([]byte(nil), key...)
	c.codes = map[string]string{}
	c.damaged = false
	blob, err := os.ReadFile(c.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	plain, err := open(c.key, blob)
	if err != nil {
		c.damaged = true
		return ErrCannotOpen
	}
	defer clear(plain)
	m := map[string]string{}
	if err := json.Unmarshal(plain, &m); err != nil {
		c.damaged = true
		return ErrCannotOpen
	}
	c.codes = m
	return nil
}

// Locked reports whether the key is missing.
func (c *Codes) Locked() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.key == nil
}

// Damaged reports whether the last Unlock found a file it could not open.
func (c *Codes) Damaged() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.damaged
}

// Get returns the code for serial.
func (c *Codes) Get(serial string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == nil {
		return "", false
	}
	code, ok := c.codes[serial]
	return code, ok
}

// Has reports whether a code is stored for serial.
func (c *Codes) Has(serial string) bool {
	_, ok := c.Get(serial)
	return ok
}

// Values returns every stored code (used to register them for redaction).
func (c *Codes) Values() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.codes))
	for _, v := range c.codes {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// Set stores code for serial and rewrites the file.
func (c *Codes) Set(serial, code string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == nil {
		return ErrLocked
	}
	next := make(map[string]string, len(c.codes)+1)
	for k, v := range c.codes {
		next[k] = v
	}
	next[serial] = code
	if err := c.writeLocked(next); err != nil {
		return err
	}
	c.codes = next
	c.damaged = false
	return nil
}

// Delete removes the code for serial and rewrites the file.
func (c *Codes) Delete(serial string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.key == nil {
		return ErrLocked
	}
	next := make(map[string]string, len(c.codes))
	for k, v := range c.codes {
		if k != serial {
			next[k] = v
		}
	}
	if err := c.writeLocked(next); err != nil {
		return err
	}
	c.codes = next
	return nil
}

func (c *Codes) writeLocked(m map[string]string) error {
	plain, err := json.Marshal(m)
	if err != nil {
		return err
	}
	defer clear(plain)
	blob, err := seal(c.key, plain)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(c.path, blob)
}

// Lock forgets the key and the decrypted codes. The file stays.
func (c *Codes) Lock() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.zeroKeyLocked()
	c.codes = map[string]string{}
}

// Wipe forgets the key and codes and deletes the file.
func (c *Codes) Wipe() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.zeroKeyLocked()
	c.codes = map[string]string{}
	c.damaged = false
	return fsutil.Wipe(c.path)
}

func (c *Codes) zeroKeyLocked() {
	if c.key != nil {
		clear(c.key)
		c.key = nil
	}
}

// seal encrypts plain with AES-256-GCM and a fresh random nonce.
// Layout: magic (5 bytes) | nonce (12 bytes) | ciphertext and tag.
func seal(key, plain []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(fileMagic)+len(nonce)+len(plain)+aead.Overhead())
	out = append(out, fileMagic...)
	out = append(out, nonce...)
	return aead.Seal(out, nonce, plain, fileMagic), nil
}

func open(key, blob []byte) ([]byte, error) {
	aead, err := newAEAD(key)
	if err != nil {
		return nil, err
	}
	head := len(fileMagic) + aead.NonceSize()
	if len(blob) < head+aead.Overhead() || string(blob[:len(fileMagic)]) != string(fileMagic) {
		return nil, ErrCannotOpen
	}
	nonce := blob[len(fileMagic):head]
	plain, err := aead.Open(nil, nonce, blob[head:], fileMagic)
	if err != nil {
		return nil, ErrCannotOpen
	}
	return plain, nil
}

func newAEAD(key []byte) (cipher.AEAD, error) {
	if len(key) != KeyLen {
		return nil, ErrBadKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
