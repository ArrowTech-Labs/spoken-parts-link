package rtsp

import (
	"crypto/md5"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"hash"
	"strconv"
	"strings"
)

// challenge is one WWW-Authenticate challenge.
type challenge struct {
	scheme    string // "digest" or "basic"
	realm     string
	nonce     string
	opaque    string
	algorithm string
	qop       []string
	stale     bool
}

// parseChallenges reads WWW-Authenticate values. One value may hold several
// challenges ("Digest realm=..., nonce=..., Basic realm=...").
func parseChallenges(values []string) []challenge {
	var out []challenge
	for _, v := range values {
		var cur *challenge
		for i := 0; i < len(v); {
			for i < len(v) && (v[i] == ' ' || v[i] == '\t' || v[i] == ',') {
				i++
			}
			start := i
			for i < len(v) && v[i] != '=' && v[i] != ' ' && v[i] != '\t' && v[i] != ',' {
				i++
			}
			tok := v[start:i]
			if tok == "" {
				break
			}
			j := i
			for j < len(v) && (v[j] == ' ' || v[j] == '\t') {
				j++
			}
			if j >= len(v) || v[j] != '=' {
				// A new scheme.
				out = append(out, challenge{scheme: strings.ToLower(tok)})
				cur = &out[len(out)-1]
				i = j
				continue
			}
			// key=value or key="quoted"
			i = j + 1
			for i < len(v) && (v[i] == ' ' || v[i] == '\t') {
				i++
			}
			var val string
			if i < len(v) && v[i] == '"' {
				i++
				var b strings.Builder
				for i < len(v) && v[i] != '"' {
					if v[i] == '\\' && i+1 < len(v) {
						i++
					}
					b.WriteByte(v[i])
					i++
				}
				i++ // closing quote
				val = b.String()
			} else {
				s := i
				for i < len(v) && v[i] != ',' {
					i++
				}
				val = strings.TrimSpace(v[s:i])
			}
			if cur == nil {
				continue
			}
			switch strings.ToLower(tok) {
			case "realm":
				cur.realm = val
			case "nonce":
				cur.nonce = val
			case "opaque":
				cur.opaque = val
			case "algorithm":
				cur.algorithm = val
			case "stale":
				cur.stale = strings.EqualFold(val, "true")
			case "qop":
				for _, q := range strings.Split(val, ",") {
					cur.qop = append(cur.qop, strings.ToLower(strings.TrimSpace(q)))
				}
			}
		}
	}
	return out
}

func digestHash(alg string) (func() hash.Hash, bool, bool) {
	switch strings.ToUpper(alg) {
	case "", "MD5":
		return md5.New, false, true
	case "MD5-SESS":
		return md5.New, true, true
	case "SHA-256":
		return sha256.New, false, true
	case "SHA-256-SESS":
		return sha256.New, true, true
	}
	return nil, false, false
}

// pickChallenge prefers Digest (SHA-256, then MD5) and takes Basic only
// over TLS.
func pickChallenge(values []string, overTLS bool) (challenge, bool) {
	var best challenge
	rank := 0
	for _, c := range parseChallenges(values) {
		r := 0
		switch c.scheme {
		case "digest":
			if _, _, ok := digestHash(c.algorithm); !ok || c.nonce == "" || !safeText(c.nonce) || !safeText(c.realm) || !safeText(c.opaque) {
				continue
			}
			if len(c.qop) > 0 && !hasQop(c.qop) {
				continue // only auth-int offered
			}
			r = 2
			if strings.HasPrefix(strings.ToUpper(c.algorithm), "SHA-256") {
				r = 3
			}
		case "basic":
			if !overTLS {
				continue
			}
			r = 1
		}
		if r > rank {
			best, rank = c, r
		}
	}
	return best, rank > 0
}

func hasQop(q []string) bool {
	for _, v := range q {
		if v == "auth" {
			return true
		}
	}
	return false
}

// authState holds what is needed to sign requests. basic and ha1 are
// derived from the password and are wiped after the grab.
type authState struct {
	digest bool
	basic  []byte // "Basic <base64>"
	ha1    []byte // hex
	h      func() hash.Hash
	user   string
	c      challenge
	qop    bool
	cnonce string
	nc     uint32
}

func newAuth(c challenge, user string, password []byte) *authState {
	if c.scheme == "basic" {
		raw := make([]byte, 0, len(user)+1+len(password))
		raw = append(raw, user...)
		raw = append(raw, ':')
		raw = append(raw, password...)
		b := make([]byte, 6+base64.StdEncoding.EncodedLen(len(raw)))
		copy(b, "Basic ")
		base64.StdEncoding.Encode(b[6:], raw)
		clear(raw)
		return &authState{basic: b}
	}
	h, sess, _ := digestHash(c.algorithm)
	a := &authState{digest: true, h: h, user: user, c: c, qop: hasQop(c.qop)}
	var cn [12]byte
	_, _ = rand.Read(cn[:])
	a.cnonce = hex.EncodeToString(cn[:])
	raw := make([]byte, 0, len(user)+len(c.realm)+len(password)+2)
	raw = append(raw, user...)
	raw = append(raw, ':')
	raw = append(raw, c.realm...)
	raw = append(raw, ':')
	raw = append(raw, password...)
	a.ha1 = a.hexHash(raw)
	clear(raw)
	if sess {
		old := a.ha1
		a.ha1 = a.hexHash(old, []byte(":"+c.nonce+":"+a.cnonce))
		clear(old)
	}
	return a
}

func (a *authState) hexHash(parts ...[]byte) []byte {
	h := a.h()
	for _, p := range parts {
		h.Write(p)
	}
	sum := h.Sum(nil)
	out := make([]byte, hex.EncodedLen(len(sum)))
	hex.Encode(out, sum)
	clear(sum)
	return out
}

func (a *authState) wipe() {
	clear(a.basic)
	clear(a.ha1)
}

func quote(b []byte, s string) []byte {
	b = append(b, '"')
	for i := 0; i < len(s); i++ {
		if s[i] == '"' || s[i] == '\\' {
			b = append(b, '\\')
		}
		b = append(b, s[i])
	}
	return append(b, '"')
}

// appendHeader appends the Authorization header value for one request.
func (a *authState) appendHeader(b []byte, method, uri string) []byte {
	if !a.digest {
		return append(b, a.basic...)
	}
	ha2 := a.hexHash([]byte(method + ":" + uri))
	var resp []byte
	nc := ""
	if a.qop {
		a.nc++
		nc = strconv.FormatUint(uint64(a.nc)|1<<32, 16)[1:] // 8 hex digits
		resp = a.hexHash(a.ha1, []byte(":"+a.c.nonce+":"+nc+":"+a.cnonce+":auth:"), ha2)
	} else {
		resp = a.hexHash(a.ha1, []byte(":"+a.c.nonce+":"), ha2)
	}
	b = append(b, "Digest username="...)
	b = quote(b, a.user)
	b = append(b, ", realm="...)
	b = quote(b, a.c.realm)
	b = append(b, ", nonce="...)
	b = quote(b, a.c.nonce)
	b = append(b, ", uri="...)
	b = quote(b, uri)
	b = append(b, ", response=\""...)
	b = append(b, resp...)
	b = append(b, '"')
	if a.c.algorithm != "" {
		b = append(b, ", algorithm="...)
		b = append(b, a.c.algorithm...)
	}
	if a.c.opaque != "" {
		b = append(b, ", opaque="...)
		b = quote(b, a.c.opaque)
	}
	if a.qop {
		b = append(b, ", qop=auth, nc="...)
		b = append(b, nc...)
		b = append(b, ", cnonce="...)
		b = quote(b, a.cnonce)
	}
	clear(resp)
	return b
}
