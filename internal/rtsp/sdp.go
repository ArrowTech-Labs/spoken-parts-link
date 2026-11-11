package rtsp

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

const (
	maxSDPLines = 512
	maxSprop    = 4 << 10
)

type media struct {
	kind    string
	pts     []int
	pt      int    // the H.264 payload type, -1 if not found
	control string // a=control
	sprop   [][]byte
}

type sdp struct {
	control string // session level a=control
	medias  []media
}

func parseSDP(body []byte) (*sdp, error) {
	lines := strings.Split(string(body), "\n")
	if len(lines) > maxSDPLines {
		return nil, fmt.Errorf("%w: SDP lines", ErrTooLarge)
	}
	d := &sdp{}
	var cur *media
	rtpmap := map[int]string{}
	fmtp := map[int]string{}
	finish := func() {
		if cur == nil {
			return
		}
		cur.pt = -1
		for _, pt := range cur.pts {
			enc, _, _ := strings.Cut(rtpmap[pt], "/")
			if strings.EqualFold(strings.TrimSpace(enc), "H264") {
				cur.pt = pt
				cur.sprop = spropSets(fmtp[pt])
				break
			}
		}
		d.medias = append(d.medias, *cur)
		rtpmap, fmtp = map[int]string{}, map[int]string{}
	}
	for _, l := range lines {
		l = strings.TrimRight(l, "\r")
		if len(l) < 2 || l[1] != '=' {
			continue
		}
		v := l[2:]
		switch l[0] {
		case 'm':
			finish()
			f := strings.Fields(v)
			if len(f) < 4 {
				return nil, fmt.Errorf("%w: SDP media line", ErrProtocol)
			}
			cur = &media{kind: strings.ToLower(f[0])}
			for _, p := range f[3:] {
				if n, err := strconv.Atoi(p); err == nil && n >= 0 && n < 128 {
					cur.pts = append(cur.pts, n)
				}
			}
		case 'a':
			k, val, _ := strings.Cut(v, ":")
			switch strings.ToLower(k) {
			case "control":
				if cur == nil {
					d.control = strings.TrimSpace(val)
				} else {
					cur.control = strings.TrimSpace(val)
				}
			case "rtpmap", "fmtp":
				if cur == nil {
					continue
				}
				ptStr, rest, _ := strings.Cut(val, " ")
				pt, err := strconv.Atoi(ptStr)
				if err != nil {
					continue
				}
				if strings.EqualFold(k, "rtpmap") {
					rtpmap[pt] = strings.TrimSpace(rest)
				} else {
					fmtp[pt] = rest
				}
			}
		}
	}
	finish()
	return d, nil
}

func spropSets(fmtp string) [][]byte {
	var out [][]byte
	for _, p := range strings.Split(fmtp, ";") {
		k, v, ok := strings.Cut(strings.TrimSpace(p), "=")
		if !ok || !strings.EqualFold(k, "sprop-parameter-sets") {
			continue
		}
		for _, s := range strings.Split(v, ",") {
			s = strings.TrimSpace(s)
			if s == "" || len(s) > maxSprop {
				continue
			}
			b, err := base64.StdEncoding.DecodeString(s)
			if err != nil {
				b, err = base64.RawStdEncoding.DecodeString(strings.TrimRight(s, "="))
			}
			if err == nil && len(b) > 0 {
				out = append(out, b)
			}
		}
	}
	return out
}

// h264 returns the first H.264 video track.
func (d *sdp) h264() (media, error) {
	for _, m := range d.medias {
		if m.kind == "video" && m.pt >= 0 {
			return m, nil
		}
	}
	return media{}, fmt.Errorf("%w: the stream has no H.264 video track", ErrProtocol)
}

// resolve turns a control attribute or Content-Base into a request URL.
// Relative references are taken under base (as a directory, which is what
// cameras mean: "trackID=1" under ".../live/1" is ".../live/1/trackID=1").
// Userinfo is always dropped.
func resolve(base, ref string, dir bool) (string, error) {
	bad := fmt.Errorf("%w: unusable control URL", ErrProtocol)
	b, err := url.Parse(base)
	if err != nil {
		return "", bad
	}
	if ref == "" || ref == "*" {
		return base, nil
	}
	r, err := url.Parse(ref)
	if err != nil || !safeText(ref) || strings.Contains(ref, " ") {
		return "", bad
	}
	var u *url.URL
	if r.IsAbs() {
		u = r
	} else {
		if dir && !strings.HasSuffix(b.Path, "/") {
			c := *b
			c.Path += "/"
			c.RawPath = ""
			b = &c
		}
		u = b.ResolveReference(r)
	}
	if (u.Scheme != "rtsp" && u.Scheme != "rtsps") || u.Host == "" {
		return "", bad
	}
	u.User = nil
	u.Fragment = ""
	return u.String(), nil
}
