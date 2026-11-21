// Package protocol reads and writes Quasar service frames over a stream connection.
package protocol

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"net"
	"strings"
	"time"
)

const (
	fieldPayload = 252
	fieldID      = 253

	maxPayload = 1 << 22
)

// Framing is how messages are separated and wrapped on the socket. Both builds
// prefix every message with its uint32 little-endian byte count (verified with
// raw dumps on both devices: "5b 00 00 00 e2 0f 55 ..." on Station 2,
// "0f 00 00 00 1a 0d ..." on Station Max), but they wrap the body differently:
//
//	Buildroot columns (Station 2 / Midi, quasar 0.369+):
//	  QuasarMessage { field 252 = body, field 253 = message id }
//	Station Max (yandexstation_2, quasar 20201111):
//	  QuasarMessage { field <id> = body }  -- the body is the oneof field value
//
// The id numbering is shared: ids seen on Station 2 that are <= 157 decode with
// the Max oneof field names, so the newer build only moved the body into a
// generic envelope and kept the ids.
type Framing int

const (
	Auto Framing = iota
	Envelope
	Direct
	Bare
)

// ParseFraming accepts a layout name or CLI alias.
func ParseFraming(s string) (Framing, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return Auto, nil
	case "env", "envelope", "len", "length", "prefixed", "le32", "linux":
		return Envelope, nil
	case "direct", "oneof", "max":
		return Direct, nil
	case "bare", "concat", "none":
		return Bare, nil
	}
	return Auto, fmt.Errorf("unknown -framing %q (auto, env, direct, bare)", s)
}

func (f Framing) String() string {
	switch f {
	case Envelope:
		return "len+252/253"
	case Direct:
		return "len+oneof"
	case Bare:
		return "bare+252/253"
	}
	return "auto"
}

// prefixed reports whether messages carry the uint32 length header.
func (f Framing) prefixed() bool { return f != Bare }

// envForm reports whether the body travels inside fields 252/253.
func (f Framing) envForm() bool { return f != Direct }

var (
	tagPayload = AppendVarint(nil, fieldPayload<<3|2) // e2 0f
	tagID      = AppendVarint(nil, fieldID<<3|0)      // a8 0f
)

// Bus carries frames over one service connection. Use it from one goroutine at a time.
type Bus struct {
	c        net.Conn
	r        *bufio.Reader
	fr       Framing
	def      Framing
	deadline time.Time
}

// NewBus wraps a service connection. For Auto, fallback selects the layout of a silent service.
func NewBus(c net.Conn, fr, fallback Framing) *Bus {
	if fallback == Auto {
		fallback = Envelope
	}
	return &Bus{c: c, r: bufio.NewReaderSize(c, 1<<16), fr: fr, def: fallback}
}

// SetReadDeadline sets the absolute deadline for subsequent reads and framing detection.
func (b *Bus) SetReadDeadline(t time.Time) error {
	if err := b.c.SetReadDeadline(t); err != nil {
		return err
	}
	b.deadline = t
	return nil
}

// detect settles the framing from the first bytes; a subscribed service pushes
// a frame right after connect, so a short bounded peek tells the layouts apart.
// A silent service keeps the layout implied by the selected build.
func (b *Bus) detect() {
	if b.fr != Auto {
		return
	}
	peek := time.Now().Add(300 * time.Millisecond)
	if !b.deadline.IsZero() && b.deadline.Before(peek) {
		peek = b.deadline
	}
	_ = b.c.SetReadDeadline(peek)
	defer func() { _ = b.c.SetReadDeadline(b.deadline) }()
	p, _ := b.r.Peek(6)
	switch {
	case len(p) >= 6 && le32(p[0:4]) >= 4 && le32(p[0:4]) <= maxPayload &&
		p[4] == tagPayload[0] && p[5] == tagPayload[1]:
		b.fr = Envelope
	case len(p) >= 5 && le32(p[0:4]) >= 2 && le32(p[0:4]) <= maxPayload && plausibleTag(p[4]):
		b.fr = Direct
	case len(p) >= 2 && p[0] == tagPayload[0] && p[1] == tagPayload[1]:
		b.fr = Bare
	default:
		b.fr = b.def
	}
}

// plausibleTag accepts only tags that can start a QuasarMessage oneof field.
func plausibleTag(c byte) bool {
	f := uint64(c) >> 3
	return f >= 1 && f <= 1000 && (c&7 == 0 || c&7 == 1 || c&7 == 2 || c&7 == 5)
}

func le32(b []byte) uint64 {
	return uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24
}

// ReadFrame returns the next frame.
func (b *Bus) ReadFrame() (*Frame, error) {
	b.detect()
	if b.fr == Bare {
		return b.nextBare()
	}
	hdr := make([]byte, 4)
	if _, err := io.ReadFull(b.r, hdr); err != nil {
		return nil, err
	}
	n := le32(hdr)
	if n == 0 || n > maxPayload {
		return nil, fmt.Errorf("message length %d out of range", n)
	}
	buf := make([]byte, int(n))
	if _, err := io.ReadFull(b.r, buf); err != nil {
		return nil, err
	}
	var f Frame
	var err error
	if b.fr == Envelope {
		f, err = DecodeEnvelope(buf)
	} else {
		f, err = DecodeDirect(buf)
	}
	if err != nil {
		return nil, fmt.Errorf("in %d byte message: %w", n, err)
	}
	f.raw = buf
	return &f, nil
}

// nextBare reads one envelope message without a length header.
func (b *Bus) nextBare() (*Frame, error) {
	tag, err := readVarint(b.r)
	if err != nil {
		return nil, err
	}
	if tag>>3 != fieldPayload || tag&7 != 2 {
		return nil, fmt.Errorf("expected field %d tag, got field %d wire type %d", fieldPayload, tag>>3, tag&7)
	}
	n, err := readVarint(b.r)
	if err != nil {
		return nil, err
	}
	if n > maxPayload {
		return nil, fmt.Errorf("payload length %d too large", n)
	}
	payload := make([]byte, int(n))
	if _, err := io.ReadFull(b.r, payload); err != nil {
		return nil, err
	}
	f := Frame{Payload: payload, WT: 2}
	if p, err := b.r.Peek(len(tagID)); err == nil && bytes.HasPrefix(p, tagID) {
		if _, err := b.r.Discard(len(tagID)); err != nil {
			return nil, err
		}
		id, err := readVarint(b.r)
		if err != nil {
			return nil, err
		}
		f.ID = id
	}
	return &f, nil
}

// WriteFrame sends one frame in the layout the connection settled on.
func (b *Bus) WriteFrame(f Frame) error {
	b.detect()
	if b.fr.prefixed() {
		msg := b.EncodeFrame(f)
		n := len(msg)
		msg = append([]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, msg...)
		_, err := b.c.Write(msg)
		return err
	}
	_, err := b.c.Write(b.EncodeFrame(f))
	return err
}

// EncodeFrame returns the protobuf bytes without a length prefix. For frames
// read from the bus it preserves the received bytes, including compound messages.
// Construct a new Frame when changing a received message or its layout.
func (b *Bus) EncodeFrame(f Frame) []byte {
	if f.raw != nil {
		return f.raw
	}
	if b.Framing().envForm() {
		return f.EncodeEnvelope()
	}
	return f.EncodeDirect()
}

// WriteRaw sends bytes exactly as given.
func (b *Bus) WriteRaw(raw []byte) error {
	_, err := b.c.Write(raw)
	return err
}

// VisitMessages yields every logical message from one transport frame. A Max
// packet may contain repeated IDs. Field 1 is request metadata unless alone.
func (b *Bus) VisitMessages(f *Frame, visit func(Frame) error) error {
	if f.raw == nil || b.fr != Direct || f.ID == 1 {
		return visit(*f)
	}
	return WalkFields(f.raw, func(part Frame) error {
		if part.ID == 1 {
			return nil
		}
		return visit(part)
	})
}

// Framing returns the selected layout. Auto detects it on the first call.
func (b *Bus) Framing() Framing {
	b.detect()
	return b.fr
}

// AwaitFraming waits for a service's initial broadcast before detecting its layout.
func (b *Bus) AwaitFraming() (Framing, error) {
	if b.fr == Auto {
		if _, err := b.r.Peek(6); err != nil {
			return b.fr, err
		}
	}
	return b.Framing(), nil
}

// Close closes the underlying service connection.
func (b *Bus) Close() error { return b.c.Close() }
