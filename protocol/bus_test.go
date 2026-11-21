package protocol_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/skrashevich/quasarc/protocol"
)

// memConn feeds a fixed byte sequence to the bus and records written bytes.
type memConn struct {
	net.Conn
	r        *bytes.Reader
	w        bytes.Buffer
	deadline time.Time
}

func (c *memConn) Read(p []byte) (int, error) {
	if c.r == nil {
		return 0, io.EOF
	}
	return c.r.Read(p)
}

func (c *memConn) Write(p []byte) (int, error) { return c.w.Write(p) }

func (c *memConn) SetReadDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (c *memConn) Close() error { return nil }

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	return b
}

func testBus(raw []byte, def protocol.Framing) *protocol.Bus {
	b := protocol.NewBus(&memConn{r: bytes.NewReader(raw)}, protocol.Auto, def)
	return b
}

// lengthPrefixed adds the uint32 little-endian header both builds put in front
// of every message.
func lengthPrefixed(msg []byte) []byte {
	n := len(msg)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, msg...)
}

// The captures these tests are built from were taken with quasarc -dump on the
// devices. Only the framing bytes are quoted verbatim; the body of the Station 2
// subscription is replaced by 85 zero bytes because its content is irrelevant to
// the framing being checked.
//
//	Station 2 (quasar 0.369.1.46), subscribed service, first frame:
//	  5b 00 00 00 | e2 0f 55 <85 bytes> e8 0f 03
//	Station Max, aliced (9875), first frame:
//	  0f 00 00 00 | 1a 0d 08 00 21 00 00 00 c0 4d 83 69 40 28 01

// maxAliceStateHex is the alice_state body captured with quasarc -dump on
// Station Max (frame 0f 00 00 00 | 1a 0d <body>).
const maxAliceStateHex = "080021000000c04d8369402801"

func station2Capture() []byte {
	return lengthPrefixed(protocol.Frame{ID: 3, WT: 2, Payload: make([]byte, 85)}.EncodeEnvelope())
}

func maxCapture() []byte {
	return lengthPrefixed(protocol.Frame{ID: 3, WT: 2, Payload: unhexStatic(maxAliceStateHex)}.EncodeDirect())
}

func unhexStatic(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

func TestStation2EnvelopeCapture(t *testing.T) {
	raw := station2Capture()
	if len(raw) != 95 || raw[0] != 0x5b {
		t.Fatalf("capture is %d bytes with header %#x, want 95 and 0x5b as observed", len(raw), raw[0])
	}
	b := testBus(raw, protocol.Envelope)
	f, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if b.Framing() != protocol.Envelope {
		t.Errorf("detected %s, want %s", b.Framing(), protocol.Envelope)
	}
	if f.ID != 3 {
		t.Errorf("id = %d, want 3 (alice_state)", f.ID)
	}
	if len(f.Payload) != 85 {
		t.Errorf("payload = %d bytes, want 85", len(f.Payload))
	}
}

func TestMaxDirectCapture(t *testing.T) {
	raw := maxCapture()
	if len(raw) != 19 || raw[0] != 0x0f || raw[4] != 0x1a {
		t.Fatalf("capture is %d bytes %# x, want the 19 byte frame observed on aliced", len(raw), raw)
	}
	b := testBus(raw, protocol.Envelope)
	f, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if b.Framing() != protocol.Direct {
		t.Errorf("detected %s, want %s", b.Framing(), protocol.Direct)
	}
	if f.ID != 3 {
		t.Errorf("id = %d, want 3 (alice_state)", f.ID)
	}
	if f.WT != 2 {
		t.Errorf("wire type = %d, want 2", f.WT)
	}
	if got := hex.EncodeToString(f.Payload); got != "080021000000c04d8369402801" {
		t.Errorf("payload = %s", got)
	}
}

// TestWriteRebuildsCaptures proves the writer reproduces both captured frames
// byte for byte, which is what sending to those devices requires.
func TestWriteRebuildsCaptures(t *testing.T) {
	for _, tc := range []struct {
		name string
		fr   protocol.Framing
		want []byte
		f    protocol.Frame
	}{
		{"max", protocol.Direct, maxCapture(),
			protocol.Frame{ID: 3, WT: 2, Payload: unhexStatic("080021000000c04d8369402801")}},
		{"station2", protocol.Envelope, station2Capture(),
			protocol.Frame{ID: 3, WT: 2, Payload: make([]byte, 85)}},
	} {
		nc := &memConn{}
		b := protocol.NewBus(nc, tc.fr, tc.fr)
		if err := b.WriteFrame(tc.f); err != nil {
			t.Fatalf("%s: write: %v", tc.name, err)
		}
		if !bytes.Equal(nc.w.Bytes(), tc.want) {
			t.Errorf("%s: wrote %# x, want %# x", tc.name, nc.w.Bytes(), tc.want)
		}
		if n := binary.LittleEndian.Uint32(nc.w.Bytes()[:4]); int(n) != len(nc.w.Bytes())-4 {
			t.Errorf("%s: length header %d does not match %d body bytes", tc.name, n, len(nc.w.Bytes())-4)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	body := []byte("quasar bus body")
	got, err := protocol.DecodeEnvelope(protocol.Frame{ID: 207, WT: 2, Payload: body}.EncodeEnvelope())
	if err != nil {
		t.Fatalf("decodeEnvelope: %v", err)
	}
	if got.ID != 207 || !bytes.Equal(got.Payload, body) {
		t.Errorf("round trip gave id %d payload %q", got.ID, got.Payload)
	}
}

func TestScalarDirectFrame(t *testing.T) {
	// A varint-valued oneof field: id 240 carrying the value 100.
	msg := protocol.Frame{ID: 240, WT: 0, Payload: protocol.AppendVarint(nil, 100)}.EncodeDirect()
	got, err := protocol.DecodeDirect(msg)
	if err != nil {
		t.Fatalf("decodeDirect: %v", err)
	}
	if got.ID != 240 || got.WT != 0 {
		t.Errorf("got id %d wire type %d, want 240 and 0", got.ID, got.WT)
	}
	v, n := protocol.DecodeVarint(got.Payload)
	if n == 0 || v != 100 {
		t.Errorf("payload varint = %d (%d bytes), want 100", v, n)
	}
}

func TestBareFramingStillWorks(t *testing.T) {
	msg := protocol.Frame{ID: 9, WT: 2}.EncodeEnvelope()
	b := testBus(msg, protocol.Envelope)
	f, err := b.ReadFrame()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if b.Framing() != protocol.Bare {
		t.Errorf("detected %s, want %s", b.Framing(), protocol.Bare)
	}
	if f.ID != 9 {
		t.Errorf("id = %d, want 9", f.ID)
	}
}

// TestSharedNumbering pins the claim that lets one id table serve both builds.
func TestSharedNumbering(t *testing.T) {
	table, ok := protocol.MessageTable("linux")
	if !ok {
		t.Fatal("missing linux schema")
	}
	e, ok := table.ByID(3)
	if !ok {
		t.Fatalf("linux table has no id 3")
	}
	if e.Name != "alice_state" {
		t.Errorf("linux id 3 = %q, want alice_state", e.Name)
	}
	if _, ok := table.ByID(158); ok {
		t.Errorf("linux table inherited max id %d", 158)
	}
}

func TestSilentServiceKeepsBuildLayout(t *testing.T) {
	b := testBus(nil, protocol.Direct)
	if _, err := b.ReadFrame(); !errors.Is(err, io.EOF) {
		t.Fatalf("next on empty stream: %v", err)
	}
	if b.Framing() != protocol.Direct {
		t.Errorf("silent service settled on %s, want %s", b.Framing(), protocol.Direct)
	}
}

func TestWtFor(t *testing.T) {
	for kind, want := range map[string]uint64{
		"message":  2,
		"string":   2,
		"int32":    0,
		"bool":     0,
		"double":   1,
		"float":    5,
		"AliceSet": 2,
	} {
		if got := (protocol.Message{Kind: kind}).WireType(); got != want {
			t.Errorf("wtFor(%q) = %d, want %d", kind, got, want)
		}
	}
}
