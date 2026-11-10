package rtsp

import (
	"errors"
	"fmt"
)

// H.264 NAL unit types used here.
const (
	nalIDR  = 5
	nalSPS  = 7
	nalPPS  = 8
	nalSTAP = 24
	nalFUA  = 28
)

var errRTP = errors.New("bad RTP packet")

type rtpPacket struct {
	marker  bool
	pt      int
	seq     uint16
	ts      uint32
	payload []byte
}

func parseRTP(p []byte) (rtpPacket, error) {
	var r rtpPacket
	if len(p) < 12 || p[0]>>6 != 2 {
		return r, errRTP
	}
	cc := int(p[0] & 0x0f)
	r.marker = p[1]&0x80 != 0
	r.pt = int(p[1] & 0x7f)
	r.seq = uint16(p[2])<<8 | uint16(p[3])
	r.ts = uint32(p[4])<<24 | uint32(p[5])<<16 | uint32(p[6])<<8 | uint32(p[7])
	off := 12 + 4*cc
	if off > len(p) {
		return r, errRTP
	}
	if p[0]&0x10 != 0 { // extension
		if off+4 > len(p) {
			return r, errRTP
		}
		off += 4 + 4*(int(p[off+2])<<8|int(p[off+3]))
		if off > len(p) {
			return r, errRTP
		}
	}
	end := len(p)
	if p[0]&0x20 != 0 { // padding
		pad := int(p[end-1])
		if pad == 0 || off+pad > end {
			return r, errRTP
		}
		end -= pad
	}
	r.payload = p[off:end]
	return r, nil
}

// assembler turns RTP H.264 packets (RFC 6184 single NAL, STAP-A, FU-A)
// into the first complete IDR access unit. Every buffer it keeps is bounded
// by maxFrame.
type assembler struct {
	pt       int // payload type, -1 for any
	maxFrame int
	sps, pps []byte

	haveSeq bool
	lastSeq uint16

	fu     []byte
	fuOn   bool // inside a fragmented NAL
	fuKeep bool // and it is one we keep

	inAU   bool
	ts     uint32
	idr    [][]byte
	size   int
	broken bool
}

func newAssembler(pt, maxFrame int) *assembler {
	if maxFrame <= 0 || maxFrame > MaxFrame {
		maxFrame = MaxFrame
	}
	return &assembler{pt: pt, maxFrame: maxFrame}
}

// param takes an SPS or PPS from the SDP.
func (a *assembler) param(n []byte) {
	if len(n) == 0 || len(n) > maxSprop {
		return
	}
	switch n[0] & 0x1f {
	case nalSPS:
		a.sps = append([]byte(nil), n...)
	case nalPPS:
		a.pps = append([]byte(nil), n...)
	}
}

// push takes one RTP packet. It returns the Annex-B access unit once the
// first complete IDR picture (with an SPS and PPS) is in.
func (a *assembler) push(p []byte) ([]byte, error) {
	r, err := parseRTP(p)
	if err != nil {
		return nil, nil // ignore what is not RTP
	}
	if a.pt >= 0 && r.pt != a.pt {
		return nil, nil
	}
	gap := false
	if a.haveSeq {
		switch r.seq - a.lastSeq {
		case 0:
			return nil, nil // duplicate
		case 1:
		default: // lost packets: the frame they were in is broken
			a.fuOn = false
			a.broken, gap = true, true
		}
	}
	a.haveSeq, a.lastSeq = true, r.seq
	if a.inAU && r.ts != a.ts {
		if au := a.finish(); au != nil {
			return au, nil
		}
	}
	if !a.inAU {
		// The lost packets may have been the start of this frame.
		a.inAU, a.ts, a.broken = true, r.ts, gap
	}
	if err := a.payload(r.payload); err != nil {
		return nil, err
	}
	if r.marker {
		return a.finish(), nil
	}
	return nil, nil
}

func (a *assembler) payload(pl []byte) error {
	if len(pl) == 0 {
		return nil
	}
	typ := pl[0] & 0x1f
	switch {
	case typ >= 1 && typ <= 23:
		a.fuOn = false
		return a.nal(pl)
	case typ == nalSTAP:
		a.fuOn = false
		rest := pl[1:]
		for len(rest) > 0 {
			if len(rest) < 2 {
				a.broken = true
				return nil
			}
			n := int(rest[0])<<8 | int(rest[1])
			rest = rest[2:]
			if n == 0 || n > len(rest) {
				a.broken = true
				return nil
			}
			if err := a.nal(rest[:n]); err != nil {
				return err
			}
			rest = rest[n:]
		}
		return nil
	case typ == nalFUA:
		if len(pl) < 2 {
			a.broken = true
			return nil
		}
		start, end := pl[1]&0x80 != 0, pl[1]&0x40 != 0
		inner := pl[1] & 0x1f
		if start {
			if a.fuOn {
				a.broken = true // the previous one never ended
			}
			a.fuOn = true
			a.fuKeep = keep(inner)
			a.fu = a.fu[:0]
			if a.fuKeep {
				a.fu = append(a.fu, pl[0]&0xe0|inner)
			}
		} else if !a.fuOn {
			return nil // the start was lost
		}
		if a.fuKeep {
			if len(a.fu)+len(pl)-2 > a.maxFrame {
				return fmt.Errorf("%w: NAL unit over %d bytes", ErrTooLarge, a.maxFrame)
			}
			a.fu = append(a.fu, pl[2:]...)
		}
		if end {
			a.fuOn = false
			if a.fuKeep {
				return a.nal(a.fu)
			}
		}
		return nil
	default:
		// STAP-B, MTAP and FU-B are for interleaved packetization mode,
		// which needs out of order handling: not supported.
		return nil
	}
}

func keep(t byte) bool { return t == nalIDR || t == nalSPS || t == nalPPS }

func (a *assembler) nal(n []byte) error {
	if len(n) == 0 || n[0]&0x80 != 0 { // forbidden_zero_bit
		return nil
	}
	if len(n) > a.maxFrame {
		return fmt.Errorf("%w: NAL unit over %d bytes", ErrTooLarge, a.maxFrame)
	}
	switch n[0] & 0x1f {
	case nalSPS:
		a.sps = append(a.sps[:0], n...)
	case nalPPS:
		a.pps = append(a.pps[:0], n...)
	case nalIDR:
		if len(a.idr) >= maxNALsPerAU {
			return fmt.Errorf("%w: slices in one frame", ErrTooLarge)
		}
		a.size += 4 + len(n)
		if a.size+8+len(a.sps)+len(a.pps) > a.maxFrame {
			return fmt.Errorf("%w: frame over %d bytes", ErrTooLarge, a.maxFrame)
		}
		a.idr = append(a.idr, append([]byte(nil), n...))
	}
	return nil
}

// finish ends the current access unit and returns it if it is a whole IDR.
func (a *assembler) finish() []byte {
	var out []byte
	// The first slice must start at macroblock 0 (first_mb_in_slice is
	// ue(v), so a leading 1 bit): otherwise we joined in the middle of the
	// picture.
	whole := len(a.idr) > 0 && len(a.idr[0]) > 1 && a.idr[0][1]&0x80 != 0
	if whole && !a.broken && !a.fuOn && len(a.sps) > 0 && len(a.pps) > 0 {
		out = make([]byte, 0, a.size+8+len(a.sps)+len(a.pps))
		for _, n := range append([][]byte{a.sps, a.pps}, a.idr...) {
			out = append(out, 0, 0, 0, 1)
			out = append(out, n...)
		}
	}
	a.inAU, a.idr, a.size, a.broken = false, nil, 0, false
	if a.fuOn {
		// A fragment spanning two timestamps is broken.
		a.fuOn = false
	}
	return out
}
