package main

import (
	"bytes"
	"encoding/hex"
	"math"
	"net"
	"strings"
	"testing"

	"github.com/skrashevich/quasarc/protocol"
)

func unhex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad test hex %q: %v", s, err)
	}
	return b
}

// lengthPrefixed adds the uint32 little-endian header both builds put in front
// of every message.
func lengthPrefixed(msg []byte) []byte {
	n := len(msg)
	return append([]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, msg...)
}

func testBus(t *testing.T, data []byte, fallback protocol.Framing) *protocol.Bus {
	reader, writer := net.Pipe()
	t.Cleanup(func() { reader.Close() })
	go func() { defer writer.Close(); writer.Write(data) }()
	return protocol.NewBus(reader, protocol.Auto, fallback)
}

// TestDecodeFixedWidthValues covers the body of the alice_state frame aliced
// sends on Station Max (08 00 | 21 <8 bytes> | 28 01), where field 4 is a
// 64-bit value the schema-free decoder used to render as an empty node.
func TestDecodeFixedWidthValues(t *testing.T) {
	body := unhex(t, "080021000000c04d8369402801")
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
