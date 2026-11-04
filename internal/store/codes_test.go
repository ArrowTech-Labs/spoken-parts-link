package store

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
)

func key(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

const testCode = "AbCd1234"

func TestDeriveKeyVector(t *testing.T) {
	k, err := DeriveKey(bytes.Repeat([]byte{2}, 32), bytes.Repeat([]byte{3}, 32))
	if err != nil {
		t.Fatal(err)
	}
	if got := hex.EncodeToString(k); got != "5e00aaad32826a9b39dd7e6ab4ed24dc4d3a0782d84a68448ce90aa2b95d79db" {
		t.Fatalf("kdf %s", got)
	}
	if _, err := DeriveKey(make([]byte, 31), make([]byte, 32)); err == nil {
		t.Fatal("short secret accepted")
	}
}

func TestRoundTripAndModes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "secrets", "codes.enc")
	k := key(t)
	c := NewCodes(path)
	if err := c.Set("01P00A000000001", testCode); err != ErrLocked {
		t.Fatalf("locked set: %v", err)
	}
	if err := c.Unlock(k); err != nil {
		t.Fatal(err)
	}
	if err := c.Set("01P00A000000001", testCode); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("file mode %v", st.Mode().Perm())
	}
	if st, _ := os.Stat(filepath.Dir(path)); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	raw, _ := os.ReadFile(path)
	if bytes.Contains(raw, []byte(testCode)) || bytes.Contains(raw, []byte("01P00A000000001")) {
		t.Fatal("plaintext on disk")
	}
	c2 := NewCodes(path)
	if err := c2.Unlock(k); err != nil {
		t.Fatal(err)
	}
	if got, ok := c2.Get("01P00A000000001"); !ok || got != testCode {
		t.Fatalf("got %q %v", got, ok)
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Fatalf("temp files left: %v", entries)
	}
}

func TestFreshNoncePerWrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codes.enc")
	c := NewCodes(path)
	_ = c.Unlock(key(t))
	_ = c.Set("01P00A000000001", testCode)
	a, _ := os.ReadFile(path)
	_ = c.Set("01P00A000000001", testCode)
	b, _ := os.ReadFile(path)
	if bytes.Equal(a[:17], b[:17]) {
		t.Fatal("nonce reused across writes")
	}
}

func TestWrongKeyFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codes.enc")
	c := NewCodes(path)
	_ = c.Unlock(key(t))
	_ = c.Set("01P00A000000001", testCode)
	c2 := NewCodes(path)
	if err := c2.Unlock(key(t)); err != ErrCannotOpen {
		t.Fatalf("wrong key: %v", err)
	}
	if !c2.Damaged() || c2.Has("01P00A000000001") {
		t.Fatal("wrong key must give an empty, damaged store")
	}
	// Saving after a failed open replaces the file with one the new key opens.
	if err := c2.Set("01P00A000000002", "ZZZZ9999"); err != nil {
		t.Fatal(err)
	}
	if c2.Damaged() {
		t.Fatal("still damaged after save")
	}
}

func TestTamperFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codes.enc")
	k := key(t)
	c := NewCodes(path)
	_ = c.Unlock(k)
	_ = c.Set("01P00A000000001", testCode)
	raw, _ := os.ReadFile(path)
	for _, i := range []int{0, 4, 5, 16, 17, len(raw) - 1} {
		bad := bytes.Clone(raw)
		bad[i] ^= 0x01
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := NewCodes(path).Unlock(k); err != ErrCannotOpen {
			t.Errorf("byte %d flip accepted: %v", i, err)
		}
	}
	if err := os.WriteFile(path, raw[:10], 0o600); err != nil {
		t.Fatal(err)
	}
	if err := NewCodes(path).Unlock(k); err != ErrCannotOpen {
		t.Errorf("truncated file accepted")
	}
}

func TestWipe(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codes.enc")
	c := NewCodes(path)
	_ = c.Unlock(key(t))
	_ = c.Set("01P00A000000001", testCode)
	if err := c.Wipe(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("file still there")
	}
	if !c.Locked() || c.Has("01P00A000000001") || len(c.Values()) != 0 {
		t.Fatal("memory not cleared")
	}
	if err := c.Wipe(); err != nil {
		t.Fatalf("second wipe: %v", err)
	}
}

func TestDeleteAndLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codes.enc")
	k := key(t)
	c := NewCodes(path)
	_ = c.Unlock(k)
	_ = c.Set("01P00A000000001", testCode)
	_ = c.Set("01P00A000000002", "ZZZZ9999")
	if err := c.Delete("01P00A000000001"); err != nil {
		t.Fatal(err)
	}
	c.Lock()
	if _, ok := c.Get("01P00A000000002"); ok {
		t.Fatal("locked store returned a code")
	}
	_ = c.Unlock(k)
	if c.Has("01P00A000000001") || !c.Has("01P00A000000002") {
		t.Fatal("delete not persisted")
	}
}

func TestStateJSONMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "state.json")
	if err := SaveJSON(path, State{LinkID: "x", Status: StatusPending}); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
	var s State
	if err := LoadJSON(path, &s); err != nil || s.LinkID != "x" {
		t.Fatalf("%v %+v", err, s)
	}
	var missing State
	if err := LoadJSON(filepath.Join(t.TempDir(), "nope.json"), &missing); err != nil {
		t.Fatal(err)
	}
}
