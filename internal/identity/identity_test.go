package identity

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// Contract test vector: seed = 32 bytes of 0x01 (also in the backend tests).
func TestContractVector(t *testing.T) {
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, 32))
	pub := priv.Public().(ed25519.PublicKey)
	if got := base64.StdEncoding.EncodeToString(pub); got != "iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w=" {
		t.Fatalf("public key %s", got)
	}
	if got := Fingerprint(pub); got != "sha256:34750f98bd59fcfc" {
		t.Fatalf("fingerprint %s", got)
	}
	if got := CheckNumber(pub); got != "3878" {
		t.Fatalf("check number %s", got)
	}
}

func TestCheckNumberIsFourDigits(t *testing.T) {
	for i := 0; i < 200; i++ {
		pub, _, _ := ed25519.GenerateKey(nil)
		c := CheckNumber(pub)
		if len(c) != 4 || c[0] < '0' || c[0] > '9' {
			t.Fatalf("bad check number %q", c)
		}
	}
}

func TestLoadOrCreateFilesAndModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "identity")
	id, created, err := LoadOrCreate(dir)
	if err != nil || !created {
		t.Fatalf("create: %v %v", err, created)
	}
	for _, f := range []string{KeyFile, SecretFile} {
		st, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, st.Mode().Perm())
		}
	}
	if st, _ := os.Stat(dir); st.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode %v", st.Mode().Perm())
	}
	again, created, err := LoadOrCreate(dir)
	if err != nil || created {
		t.Fatalf("reload: %v %v", err, created)
	}
	if !bytes.Equal(id.Public, again.Public) || !bytes.Equal(id.LocalSecret, again.LocalSecret) {
		t.Fatal("reload gave a different identity")
	}
	if len(id.LocalSecret) != 32 {
		t.Fatal("local secret length")
	}
}

func TestWipeMakesNewIdentity(t *testing.T) {
	dir := t.TempDir()
	a, _, _ := LoadOrCreate(dir)
	if err := Wipe(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{KeyFile, SecretFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); !os.IsNotExist(err) {
			t.Fatalf("%s still there", f)
		}
	}
	b, created, _ := LoadOrCreate(dir)
	if !created || bytes.Equal(a.Public, b.Public) || bytes.Equal(a.LocalSecret, b.LocalSecret) {
		t.Fatal("wipe did not give a fresh identity")
	}
}

func TestCorruptKeyIsAnError(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, KeyFile), []byte("short"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("want error for a corrupt key")
	}
}
