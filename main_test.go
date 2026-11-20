package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"net"
	"strings"
	"testing"
	"time"
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

func testBus(raw []byte, def framing) *bus {
	b := newBus(&memConn{r: bytes.NewReader(raw)}, framingAuto, def)
	b.r = bufio.NewReader(bytes.NewReader(raw))
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
	return lengthPrefixed(frame{ID: 3, WT: 2, Payload: make([]byte, 85)}.encodeEnvelope())
}

func maxCapture() []byte {
	return lengthPrefixed(frame{ID: 3, WT: 2, Payload: unhexStatic(maxAliceStateHex)}.encodeDirect())
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
	b := testBus(raw, framingLenEnv)
	f, err := b.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if b.fr != framingLenEnv {
		t.Errorf("detected %s, want %s", b.fr, framingLenEnv)
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
	b := testBus(raw, framingLenEnv)
	f, err := b.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if b.fr != framingLenDirect {
		t.Errorf("detected %s, want %s", b.fr, framingLenDirect)
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
		fr   framing
		want []byte
		f    frame
	}{
		{"max", framingLenDirect, maxCapture(),
			frame{ID: 3, WT: 2, Payload: unhexStatic("080021000000c04d8369402801")}},
		{"station2", framingLenEnv, station2Capture(),
			frame{ID: 3, WT: 2, Payload: make([]byte, 85)}},
	} {
		nc := &memConn{}
		b := newBus(nc, tc.fr, tc.fr)
		if err := b.write(tc.f); err != nil {
			t.Fatalf("%s: write: %v", tc.name, err)
		}
		if !bytes.Equal(nc.w.Bytes(), tc.want) {
			t.Errorf("%s: wrote %# x, want %# x", tc.name, nc.w.Bytes(), tc.want)
		}
		if n := le32(nc.w.Bytes()[:4]); int(n) != len(nc.w.Bytes())-4 {
			t.Errorf("%s: length header %d does not match %d body bytes", tc.name, n, len(nc.w.Bytes())-4)
		}
	}
}

func TestEnvelopeRoundTrip(t *testing.T) {
	body := []byte("quasar bus body")
	got, err := decodeEnvelope(frame{ID: 207, WT: 2, Payload: body}.encodeEnvelope())
	if err != nil {
		t.Fatalf("decodeEnvelope: %v", err)
	}
	if got.ID != 207 || !bytes.Equal(got.Payload, body) {
		t.Errorf("round trip gave id %d payload %q", got.ID, got.Payload)
	}
}

func TestScalarDirectFrame(t *testing.T) {
	// A varint-valued oneof field: id 240 carrying the value 100.
	msg := frame{ID: 240, WT: 0, Payload: putVarint(nil, 100)}.encodeDirect()
	got, err := decodeDirect(msg)
	if err != nil {
		t.Fatalf("decodeDirect: %v", err)
	}
	if got.ID != 240 || got.WT != 0 {
		t.Errorf("got id %d wire type %d, want 240 and 0", got.ID, got.WT)
	}
	v, n := decodeVarint(got.Payload)
	if n == 0 || v != 100 {
		t.Errorf("payload varint = %d (%d bytes), want 100", v, n)
	}
}

func TestBareFramingStillWorks(t *testing.T) {
	msg := frame{ID: 9, WT: 2}.encodeEnvelope()
	b := testBus(msg, framingLenEnv)
	f, err := b.next()
	if err != nil {
		t.Fatalf("next: %v", err)
	}
	if b.fr != framingBare {
		t.Errorf("detected %s, want %s", b.fr, framingBare)
	}
	if f.ID != 9 {
		t.Errorf("id = %d, want 9", f.ID)
	}
}

// TestSharedNumbering pins the claim that lets one id table serve both builds.
func TestSharedNumbering(t *testing.T) {
	table := resolveTable(buildLinux)
	e, ok := table.byID[3]
	if !ok {
		t.Fatalf("linux table has no id 3")
	}
	if e.Name != "alice_state" {
		t.Errorf("linux id 3 = %q, want alice_state", e.Name)
	}
	if _, ok := table.byID[sharedIDLimit+1]; ok {
		t.Errorf("linux table inherited max id %d", sharedIDLimit+1)
	}
}

func TestSilentServiceKeepsBuildLayout(t *testing.T) {
	b := testBus(nil, framingLenDirect)
	if _, err := b.next(); !errors.Is(err, io.EOF) {
		t.Fatalf("next on empty stream: %v", err)
	}
	if b.fr != framingLenDirect {
		t.Errorf("silent service settled on %s, want %s", b.fr, framingLenDirect)
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
		if got := wtFor(kind); got != want {
			t.Errorf("wtFor(%q) = %d, want %d", kind, got, want)
		}
	}
}

// TestDecodeFixedWidthValues covers the body of the alice_state frame aliced
// sends on Station Max (08 00 | 21 <8 bytes> | 28 01), where field 4 is a
// 64-bit value the schema-free decoder used to render as an empty node.
func TestDecodeFixedWidthValues(t *testing.T) {
	body := unhexStatic(maxAliceStateHex)
	kids, err := decodeMsg(body, 0)
	if err != nil {
		t.Fatalf("decodeMsg: %v", err)
	}
	if len(kids) != 3 {
		t.Fatalf("decoded %d fields, want 3: %+v", len(kids), kids)
	}
	got := kids[1]
	if got.Field != 4 || got.WT != 1 {
		t.Fatalf("second field = %d wire %d, want 4 wire 1", got.Field, got.WT)
	}
	const bits = 0x4069834dc0000000
	if got.Val != bits {
		t.Errorf("fixed64 bits = %#x, want %#x", got.Val, bits)
	}
	if got.Float != math.Float64frombits(bits) {
		t.Errorf("double = %v, want %v", got.Float, math.Float64frombits(bits))
	}
	if kids[2].Field != 5 || kids[2].Val != 1 {
		t.Errorf("third field = %d/%d, want 5/1", kids[2].Field, kids[2].Val)
	}

	var buf bytes.Buffer
	printTree(kids, "", &buf)
	if !strings.Contains(buf.String(), "(fixed64 0x4069834dc0000000)") {
		t.Errorf("rendered tree does not show the fixed64 bits:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "5: 1") {
		t.Errorf("rendered tree lost the varint field:\n%s", buf.String())
	}
}

func TestDecodeFixed32Float(t *testing.T) {
	kids, err := decodeMsg(unhex(t, "150000803f"), 0)
	if err != nil {
		t.Fatalf("decodeMsg: %v", err)
	}
	if len(kids) != 1 || kids[0].Field != 2 || kids[0].WT != 5 {
		t.Fatalf("decoded %+v, want one field 2 wire 5", kids)
	}
	if kids[0].Val != 0x3f800000 || kids[0].Float != 1 {
		t.Errorf("fixed32 = %#x (%v), want 0x3f800000 (1)", kids[0].Val, kids[0].Float)
	}
	var buf bytes.Buffer
	printTree(kids, "", &buf)
	if got := strings.TrimSpace(buf.String()); got != "2: 1 (fixed32 0x3f800000)" {
		t.Errorf("rendered %q", got)
	}
}

// TestDecodeNegativeVarint covers wifi_list signal levels, which arrive as
// 64-bit two's complement varints and otherwise read as huge numbers.
func TestDecodeNegativeVarint(t *testing.T) {
	kids, err := decodeMsg(unhex(t, "18c0ffffffffffffffff01"), 0)
	if err != nil {
		t.Fatalf("decodeMsg: %v", err)
	}
	if len(kids) != 1 || kids[0].Field != 3 {
		t.Fatalf("decoded %+v, want one field 3", kids)
	}
	if kids[0].Sint != -64 {
		t.Errorf("signed = %d, want -64 (unsigned %#x)", kids[0].Sint, kids[0].Val)
	}
	var buf bytes.Buffer
	printTree(kids, "", &buf)
	if !strings.Contains(buf.String(), "3: -64") {
		t.Errorf("rendered %q", buf.String())
	}
}
