package signer

import (
	"bytes"
	"crypto/ed25519"
	"testing"
)

// Fixed vectors, also written to CONTRACT-VECTORS.md for the backend.
// Key: Ed25519 seed of 32 bytes 0x01. Timestamp 1793782800.
var vectors = []struct {
	method, target, nonce, body, canonical, sig string
}{
	{
		"POST", "/api/link/v1/sync", "nonce-0123456789abcdef",
		`{"version":"0.1.0","uptime_s":60,"printers":[]}`,
		"v1\nPOST\n/api/link/v1/sync\n1793782800\nnonce-0123456789abcdef\ne76f17b5dd2ac292bc36a5c62b2561e7dba87eedb665dc02ae28874971969ce4",
		"v1=EQJkB0vtGPYJlex7HuYkBd29ftvvCpgc42O07iEVpWTMQ8gN1G5kHD/Ae9utdW7eCa6H7GSCF5S9ZhlFS3khAw==",
	},
	{
		"GET", "/api/link/v1/pair/status", "nonce-fedcba9876543210", "",
		"v1\nGET\n/api/link/v1/pair/status\n1793782800\nnonce-fedcba9876543210\ne3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855",
		"v1=BfHAWhjuI2wkYICYmocHdZQXOtNe4N4b/14s0puBoQjqgT0aKsWUMycR42vul8gwh6uiRUvhDjCaKP6atLveDA==",
	},
	{
		"POST", "/api/link/v1/pair", "nonce-pair-000000000001",
		`{"code":"K7P4QX9M","public_key":"iojj3XQJ8ZX9UtstPLpdcspnCb8dlBIb83SIAbQPb1w=","name":"Spoken Parts Link","platform":"ha_addon","version":"0.1.0"}`,
		"v1\nPOST\n/api/link/v1/pair\n1793782800\nnonce-pair-000000000001\ne667031ffe1e1fbd7b339f0e3a06cf9617108b1bc4f8307da39725e77108645d",
		"v1=vUbkGW8GSod5LAWeHN5iF4gGHiCIsmsmdmTmCQsEe7wI9c00Olb0cEfN3y2zyLOWLRc+7pfRWMdlW4j/SpZjDg==",
	},
}

const vectorTS = 1793782800

func vectorKey() ed25519.PrivateKey {
	return ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x01}, 32))
}

func TestVectors(t *testing.T) {
	priv := vectorKey()
	pub := priv.Public().(ed25519.PublicKey)
	for _, v := range vectors {
		if got := string(Canonical(v.method, v.target, vectorTS, v.nonce, []byte(v.body))); got != v.canonical {
			t.Errorf("%s %s canonical\n got %q\nwant %q", v.method, v.target, got, v.canonical)
		}
		if got := Sign(priv, v.method, v.target, vectorTS, v.nonce, []byte(v.body)); got != v.sig {
			t.Errorf("%s %s signature\n got %s\nwant %s", v.method, v.target, got, v.sig)
		}
		if err := Verify(pub, v.method, v.target, "1793782800", v.nonce, []byte(v.body), v.sig); err != nil {
			t.Errorf("verify: %v", err)
		}
	}
}

func TestMethodIsUpperCased(t *testing.T) {
	if string(Canonical("post", "/x", 1, "n", nil)) != string(Canonical("POST", "/x", 1, "n", nil)) {
		t.Fatal("method not upper cased")
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	priv := vectorKey()
	pub := priv.Public().(ed25519.PublicKey)
	v := vectors[0]
	cases := map[string]func() error{
		"body": func() error { return Verify(pub, v.method, v.target, "1793782800", v.nonce, []byte(v.body+" "), v.sig) },
		"path": func() error {
			return Verify(pub, v.method, "/api/link/v1/sync?x=1", "1793782800", v.nonce, []byte(v.body), v.sig)
		},
		"method": func() error { return Verify(pub, "PUT", v.target, "1793782800", v.nonce, []byte(v.body), v.sig) },
		"time":   func() error { return Verify(pub, v.method, v.target, "1793782801", v.nonce, []byte(v.body), v.sig) },
		"nonce":  func() error { return Verify(pub, v.method, v.target, "1793782800", v.nonce+"x", []byte(v.body), v.sig) },
		"prefix": func() error { return Verify(pub, v.method, v.target, "1793782800", v.nonce, []byte(v.body), v.sig[3:]) },
	}
	for name, f := range cases {
		if f() == nil {
			t.Errorf("%s tamper accepted", name)
		}
	}
	other, _, _ := ed25519.GenerateKey(nil)
	if Verify(other, v.method, v.target, "1793782800", v.nonce, []byte(v.body), v.sig) == nil {
		t.Error("other key accepted")
	}
}

func TestNonce(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		n := NewNonce()
		if !ValidNonce(n) || seen[n] {
			t.Fatalf("bad or repeated nonce %q", n)
		}
		seen[n] = true
	}
	for _, bad := range []string{"short", "has space in it here", "x!xxxxxxxxxxxxxxxxxx", string(bytes.Repeat([]byte("a"), 65))} {
		if ValidNonce(bad) {
			t.Errorf("accepted %q", bad)
		}
	}
}
