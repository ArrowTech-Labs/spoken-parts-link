package codebox

import (
	"bytes"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

const (
	testSerial = "01P00A000000001"
	testCode   = "Ab3dEf7h" // made up
)

func mustKey(t *testing.T) *Key {
	t.Helper()
	k, err := Generate()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestKeyEncoding(t *testing.T) {
	k := mustKey(t)
	pub, err := base64.RawURLEncoding.DecodeString(k.PublicKey())
	if err != nil || len(pub) != 65 || pub[0] != 0x04 || strings.ContainsAny(k.PublicKey(), "=+/") {
		t.Fatalf("public key %q", k.PublicKey())
	}
	if len(k.ID()) != 16 || k.ID() != KeyID(pub) || strings.ToLower(k.ID()) != k.ID() {
		t.Fatalf("key id %q", k.ID())
	}
	b := k.Bytes()
	if len(b) != PrivateKeyLen {
		t.Fatalf("scalar %d bytes", len(b))
	}
	k2, err := FromBytes(b)
	if err != nil || k2.PublicKey() != k.PublicKey() || k2.ID() != k.ID() {
		t.Fatal("key did not round trip through its scalar")
	}
	if _, err := FromBytes(make([]byte, 32)); err == nil {
		t.Fatal("zero scalar accepted")
	}
}

func TestRoundTrip(t *testing.T) {
	k := mustKey(t)
	env, err := Seal(k.PublicBytes(), testSerial, []byte(testCode))
	if err != nil {
		t.Fatal(err)
	}
	if env.Scheme != Scheme || env.KeyID != k.ID() {
		t.Fatalf("envelope %+v", env)
	}
	plain, err := k.Open(env, testSerial)
	if err != nil || string(plain) != testCode {
		t.Fatalf("open: %q %v", plain, err)
	}
}

func TestRefused(t *testing.T) {
	k := mustKey(t)
	other := mustKey(t)
	good, err := Seal(k.PublicBytes(), testSerial, []byte(testCode))
	if err != nil {
		t.Fatal(err)
	}
	flip := func(s string, i int) string {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		b[i] ^= 1
		return base64.RawURLEncoding.EncodeToString(b)
	}
	resize := func(s string, n int) string {
		b, _ := base64.RawURLEncoding.DecodeString(s)
		if n < len(b) {
			b = b[:n]
		} else {
			b = append(b, make([]byte, n-len(b))...)
		}
		return base64.RawURLEncoding.EncodeToString(b)
	}
	sealedToOther, _ := Seal(other.PublicBytes(), testSerial, []byte(testCode))
	long, _ := Seal(k.PublicBytes(), testSerial, []byte(testCode+"X"))
	cases := map[string]struct {
		mod    func(*Envelope)
		serial string
	}{
		"wrong serial":        {func(*Envelope) {}, "01P00A000000002"},
		"empty serial":        {func(*Envelope) {}, ""},
		"wrong scheme":        {func(e *Envelope) { e.Scheme = "sp-code-v2" }, testSerial},
		"missing scheme":      {func(e *Envelope) { e.Scheme = "" }, testSerial},
		"wrong key id":        {func(e *Envelope) { e.KeyID = other.ID() }, testSerial},
		"sealed to other key": {func(e *Envelope) { *e = sealedToOther; e.KeyID = k.ID() }, testSerial},
		"tampered ciphertext": {func(e *Envelope) { e.Ciphertext = flip(e.Ciphertext, 0) }, testSerial},
		"tampered tag":        {func(e *Envelope) { e.Ciphertext = flip(e.Ciphertext, 23) }, testSerial},
		"tampered nonce":      {func(e *Envelope) { e.Nonce = flip(e.Nonce, 3) }, testSerial},
		"tampered epk":        {func(e *Envelope) { e.EPK = flip(e.EPK, 40) }, testSerial},
		"short epk":           {func(e *Envelope) { e.EPK = resize(e.EPK, 64) }, testSerial},
		"compressed epk":      {func(e *Envelope) { e.EPK = resize(e.EPK, 33) }, testSerial},
		"long nonce":          {func(e *Envelope) { e.Nonce = resize(e.Nonce, 16) }, testSerial},
		"short nonce":         {func(e *Envelope) { e.Nonce = resize(e.Nonce, 11) }, testSerial},
		"short ciphertext":    {func(e *Envelope) { e.Ciphertext = resize(e.Ciphertext, 23) }, testSerial},
		"long plaintext":      {func(e *Envelope) { *e = long }, testSerial},
		"padded base64":       {func(e *Envelope) { e.Nonce += "=" }, testSerial},
		"standard base64":     {func(e *Envelope) { e.EPK = base64.RawStdEncoding.EncodeToString(mustDecode(t, e.EPK)) + "+" }, testSerial},
		"bad base64":          {func(e *Envelope) { e.Ciphertext = strings.Repeat("!", 32) }, testSerial},
		"empty":               {func(e *Envelope) { *e = Envelope{Scheme: Scheme, KeyID: k.ID()} }, testSerial},
		"point not on curve":  {func(e *Envelope) { e.EPK = notOnCurve() }, testSerial},
		"identity point bytes": {func(e *Envelope) {
			e.EPK = base64.RawURLEncoding.EncodeToString(append([]byte{4}, make([]byte, 64)...))
		}, testSerial},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			env := good
			c.mod(&env)
			plain, err := k.Open(env, c.serial)
			if err != ErrBadEnvelope || plain != nil {
				t.Fatalf("opened: %q %v", plain, err)
			}
		})
	}
	if _, err := k.Open(good, testSerial); err != nil {
		t.Fatal("the good envelope stopped opening")
	}
}

func mustDecode(t *testing.T, s string) []byte {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func notOnCurve() string {
	b := make([]byte, 65)
	b[0] = 4
	b[32], b[64] = 1, 1
	return base64.RawURLEncoding.EncodeToString(b)
}

// vector is testdata/sealed_code_vector.json, made by sealed_code_vector.py
// with Python's cryptography library.
type vector struct {
	Scheme       string `json:"scheme"`
	PrivHex      string `json:"link_private_key_hex"`
	PrivPKCS8    string `json:"link_private_pkcs8_b64"`
	PrivJWK      string `json:"link_private_jwk"`
	PublicKey    string `json:"link_public_key"`
	KeyID        string `json:"key_id"`
	Serial       string `json:"serial"`
	Code         string `json:"code"`
	AAD          string `json:"aad"`
	EphHex       string `json:"eph_private_key_hex"`
	EPK          string `json:"epk"`
	Nonce        string `json:"nonce"`
	Ciphertext   string `json:"ciphertext"`
	SharedHex    string `json:"shared_secret_hex"`
	AESKeyHex    string `json:"aes_key_hex"`
	EnvelopeJSON struct {
		Scheme     string `json:"scheme"`
		KeyID      string `json:"key_id"`
		EPK        string `json:"epk"`
		Nonce      string `json:"nonce"`
		Ciphertext string `json:"ciphertext"`
	} `json:"envelope"`
}

func loadVector(t *testing.T) vector {
	t.Helper()
	b, err := os.ReadFile("testdata/sealed_code_vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var v vector
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestPythonVector: Go opens what Python's cryptography sealed, and sealing
// the same inputs in Go gives the same bytes.
func TestPythonVector(t *testing.T) {
	v := loadVector(t)
	scalar, err := hex.DecodeString(v.PrivHex)
	if err != nil {
		t.Fatal(err)
	}
	k, err := FromBytes(scalar)
	if err != nil {
		t.Fatal(err)
	}
	if k.PublicKey() != v.PublicKey || k.ID() != v.KeyID || v.Scheme != Scheme {
		t.Fatalf("key: %s %s", k.PublicKey(), k.ID())
	}
	if string(AAD(v.Serial, v.KeyID)) != v.AAD {
		t.Fatalf("aad %q", AAD(v.Serial, v.KeyID))
	}
	var jwk struct{ Kty, Crv, X, Y, D string }
	if err := json.Unmarshal([]byte(v.PrivJWK), &jwk); err != nil {
		t.Fatal(err)
	}
	point := append(append([]byte{4}, mustDecode(t, jwk.X)...), mustDecode(t, jwk.Y)...)
	if jwk.Kty != "EC" || jwk.Crv != "P-256" || jwk.D != base64.RawURLEncoding.EncodeToString(scalar) ||
		base64.RawURLEncoding.EncodeToString(point) != v.PublicKey {
		t.Fatalf("jwk %s", v.PrivJWK)
	}
	der, err := base64.StdEncoding.DecodeString(v.PrivPKCS8)
	if err != nil {
		t.Fatal(err)
	}
	pk, err := x509.ParsePKCS8PrivateKey(der)
	if err != nil {
		t.Fatal(err)
	}
	ec, ok := pk.(*ecdsa.PrivateKey)
	if !ok {
		t.Fatalf("pkcs8 holds %T", pk)
	}
	if eck, err := ec.ECDH(); err != nil || !bytes.Equal(eck.Bytes(), scalar) {
		t.Fatal("pkcs8 key differs from the hex scalar")
	}
	env := Envelope{Scheme: v.EnvelopeJSON.Scheme, KeyID: v.EnvelopeJSON.KeyID, EPK: v.EnvelopeJSON.EPK, Nonce: v.EnvelopeJSON.Nonce, Ciphertext: v.EnvelopeJSON.Ciphertext}
	if env.EPK != v.EPK || env.Nonce != v.Nonce || env.Ciphertext != v.Ciphertext || env.KeyID != v.KeyID {
		t.Fatal("envelope fields differ from the top level fields")
	}
	plain, err := k.Open(env, v.Serial)
	if err != nil || string(plain) != v.Code {
		t.Fatalf("open: %q %v", plain, err)
	}
	if _, err := k.Open(env, v.Serial+"X"); err != ErrBadEnvelope {
		t.Fatal("opened for another serial")
	}

	// Go sealing with the same ephemeral key and nonce gives the same bytes.
	ephScalar, _ := hex.DecodeString(v.EphHex)
	eph, err := ecdh.P256().NewPrivateKey(ephScalar)
	if err != nil {
		t.Fatal(err)
	}
	got, err := SealWith(eph, mustDecode(t, v.Nonce), k.PublicBytes(), v.Serial, []byte(v.Code))
	if err != nil {
		t.Fatal(err)
	}
	if got != env {
		t.Fatalf("Go sealed %+v, Python %+v", got, env)
	}
}
