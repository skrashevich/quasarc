// Command quasarc is a portable client for the Yandex Station Quasar service
// bus. Each service exchanges length-prefixed protobuf messages over TCP.
// Android-based Station Max sends the QuasarMessage oneof field directly.
// Buildroot-based Station 2 and Midi wrap its body and ID in fields 252 and 253.
package main

import (
	"bufio"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	fieldPayload = 252
	fieldID      = 253

	maxPayload = 1 << 22
	maxDepth   = 8
)

const (
	buildMax   = "max"
	buildLinux = "linux"
)

// sharedIDLimit is the highest QuasarMessage field number in the Max schema
// dump. Raw frames captured on Station 2 with ids in this range decode into the
// Max message types, so the newer Buildroot build reuses that numbering and
// only wraps the body in fields 252/253; its own ids start above this limit and
// stay unnamed until a dump identifies them.
const sharedIDLimit = 157

type msgEntry struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
	Kind string `json:"kind"`
}

type buildTable struct {
	name string
	byID map[int]msgEntry
	byNm map[string]msgEntry
}

var builds = map[string]*buildTable{
	buildMax:   {name: buildMax, byID: map[int]msgEntry{}, byNm: map[string]msgEntry{}},
	buildLinux: {name: buildLinux, byID: map[int]msgEntry{}, byNm: map[string]msgEntry{}},
}

// verbose is set from -v and gates protocol-level chatter on stderr.
var verbose bool

func buildMaps(build string, entries []msgEntry) {
	b := builds[build]
	for _, e := range entries {
		b.byID[e.ID] = e
		b.byNm[strings.ToLower(e.Name)] = e
	}
}

// ------------------------------------------------------------------ protobuf

func putVarint(dst []byte, v uint64) []byte {
	for v >= 0x80 {
		dst = append(dst, byte(v)|0x80)
		v >>= 7
	}
	return append(dst, byte(v))
}

func readVarint(r *bufio.Reader) (uint64, error) {
	var v uint64
	for i := 0; i < 10; i++ {
		c, err := r.ReadByte()
		if err != nil {
			return 0, err
		}
		v |= uint64(c&0x7f) << (7 * i)
		if c < 0x80 {
			return v, nil
		}
	}
	return 0, errors.New("varint longer than 10 bytes")
}

// frame is one decoded bus message. WT records the wire type the body travelled
// with, which the Max layout needs in order to resend a frame unchanged.
type frame struct {
	raw     []byte // complete received protobuf message, excluding the length prefix
	ID      uint64
	WT      uint64
	Payload []byte
}

// wtFor maps a schema kind to the protobuf wire type of its value, used when
// sending a typed message to the Max build.
func wtFor(kind string) uint64 {
	switch kind {
	case "int32", "int64", "uint32", "uint64", "sint32", "sint64", "bool", "enum":
		return 0
	case "fixed64", "sfixed64", "double":
		return 1
	case "fixed32", "sfixed32", "float":
		return 5
	}
	return 2
}

// framing is how messages are separated and wrapped on the socket. Both builds
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
type framing int

const (
	framingAuto framing = iota
	framingLenEnv
	framingLenDirect
	framingBare
)

func parseFraming(s string) (framing, error) {
	switch strings.ToLower(s) {
	case "", "auto":
		return framingAuto, nil
	case "env", "envelope", "len", "length", "prefixed", "le32", "linux":
		return framingLenEnv, nil
	case "direct", "oneof", "max":
		return framingLenDirect, nil
	case "bare", "concat", "none":
		return framingBare, nil
	}
	return framingAuto, fmt.Errorf("unknown -framing %q (auto, env, direct, bare)", s)
}

func (f framing) String() string {
	switch f {
	case framingLenEnv:
		return "len+252/253"
	case framingLenDirect:
		return "len+oneof"
	case framingBare:
		return "bare+252/253"
	}
	return "auto"
}

// prefixed reports whether messages carry the uint32 length header.
func (f framing) prefixed() bool { return f != framingBare }

// envForm reports whether the body travels inside fields 252/253.
func (f framing) envForm() bool { return f != framingLenDirect }

// defaultFraming picks the layout for a service that never sends a frame, so
// that sending still works.
func defaultFraming(build string) framing {
	if build == buildMax {
		return framingLenDirect
	}
	return framingLenEnv
}

var (
	tagPayload = putVarint(nil, fieldPayload<<3|2) // e2 0f
	tagID      = putVarint(nil, fieldID<<3|0)      // a8 0f
)

// bus carries frames over one service connection.
type bus struct {
	c        net.Conn
	r        *bufio.Reader
	fr       framing
	def      framing
	deadline time.Time
}

func newBus(c net.Conn, fr, def framing) *bus {
	return &bus{c: c, r: bufio.NewReaderSize(c, 1<<16), fr: fr, def: def}
}

// setDeadline applies an absolute read deadline to every later next().
func (b *bus) setDeadline(t time.Time) {
	b.deadline = t
	if dc, ok := b.c.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = dc.SetReadDeadline(t)
	}
}

// detect settles the framing from the first bytes; a subscribed service pushes
// a frame right after connect, so a short bounded peek tells the layouts apart.
// A silent service keeps the layout implied by the selected build.
func (b *bus) detect() {
	if b.fr != framingAuto {
		return
	}
	peek := time.Now().Add(300 * time.Millisecond)
	if !b.deadline.IsZero() && b.deadline.Before(peek) {
		peek = b.deadline
	}
	if dc, ok := b.c.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = dc.SetReadDeadline(peek)
		defer func() { _ = dc.SetReadDeadline(b.deadline) }()
	}
	p, _ := b.r.Peek(6)
	switch {
	case len(p) >= 6 && le32(p[0:4]) >= 4 && le32(p[0:4]) <= maxPayload &&
		p[4] == tagPayload[0] && p[5] == tagPayload[1]:
		b.fr = framingLenEnv
	case len(p) >= 5 && le32(p[0:4]) >= 2 && le32(p[0:4]) <= maxPayload && plausibleTag(p[4]):
		b.fr = framingLenDirect
	case len(p) >= 2 && p[0] == tagPayload[0] && p[1] == tagPayload[1]:
		b.fr = framingBare
	default:
		b.fr = b.def
	}
	if verbose {
		fmt.Fprintf(os.Stderr, "framing %s\n", b.fr)
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

func le64(b []byte) uint64 {
	return le32(b) | le32(b[4:])<<32
}

// next returns the next frame.
func (b *bus) next() (*frame, error) {
	b.detect()
	if b.fr == framingBare {
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
	var f frame
	var err error
	if b.fr == framingLenEnv {
		f, err = decodeEnvelope(buf)
	} else {
		f, err = decodeDirect(buf)
	}
	if err != nil {
		return nil, fmt.Errorf("in %d byte message: %w", n, err)
	}
	f.raw = buf
	return &f, nil
}

// nextBare reads one envelope message without a length header.
func (b *bus) nextBare() (*frame, error) {
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
	f := frame{Payload: payload, WT: 2}
	if p, err := b.r.Peek(len(tagID)); err == nil && bytesHavePrefix(p, tagID) {
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

// write sends one frame in the layout the connection settled on.
func (b *bus) write(f frame) error {
	if b.fr.prefixed() {
		msg := b.encodeFrame(f)
		n := len(msg)
		msg = append([]byte{byte(n), byte(n >> 8), byte(n >> 16), byte(n >> 24)}, msg...)
		_, err := b.c.Write(msg)
		return err
	}
	_, err := b.c.Write(b.encodeFrame(f))
	return err
}

// encodeFrame renders a frame in the layout the connection settled on.
func (b *bus) encodeFrame(f frame) []byte {
	if f.raw != nil {
		return f.raw
	}
	if b.fr.envForm() {
		return f.encodeEnvelope()
	}
	return f.encodeDirect()
}

// writeRaw sends bytes exactly as given.
func (b *bus) writeRaw(raw []byte) error {
	_, err := b.c.Write(raw)
	return err
}

// encodeEnvelope renders the newer build's message: field 252 holds the body,
// field 253 the id.
func (f frame) encodeEnvelope() []byte {
	b := putVarint(nil, fieldPayload<<3|2)
	b = putVarint(b, uint64(len(f.Payload)))
	b = append(b, f.Payload...)
	b = putVarint(b, fieldID<<3|0)
	return putVarint(b, f.ID)
}

// encodeDirect renders the Max build's message, where the body is the value of
// the oneof field numbered by the id. An empty body is sent as an empty
// length-delimited value because scalars are carried as raw value bytes.
func (f frame) encodeDirect() []byte {
	wt := f.WT
	if wt == 0 && len(f.Payload) == 0 {
		wt = 2
	}
	b := putVarint(nil, f.ID<<3|wt)
	if wt == 2 {
		b = putVarint(b, uint64(len(f.Payload)))
	}
	return append(b, f.Payload...)
}

// decodeEnvelope parses a {252 body, 253 id} message.
func decodeEnvelope(b []byte) (frame, error) {
	var f frame
	f.WT = 2
	seen := false
	for len(b) > 0 {
		key, n := decodeVarint(b)
		if n == 0 {
			return f, errors.New("bad tag varint")
		}
		b = b[n:]
		fn, wt := key>>3, key&7
		switch {
		case fn == fieldPayload && wt == 2:
			l, m := decodeVarint(b)
			if m == 0 {
				return f, errors.New("bad payload length")
			}
			b = b[m:]
			if l > uint64(len(b)) {
				return f, errors.New("payload length overrun")
			}
			f.Payload = b[:int(l)]
			b = b[int(l):]
			seen = true
		case fn == fieldID && wt == 0:
			v, m := decodeVarint(b)
			if m == 0 {
				return f, errors.New("bad id varint")
			}
			b = b[m:]
			f.ID = v
		default:
			k, err := skipBytes(b, wt)
			if err != nil {
				return f, err
			}
			b = b[k:]
		}
	}
	if !seen && f.ID == 0 {
		return f, errors.New("no field 252 and no id in message")
	}
	return f, nil
}

// walkFields exposes the wire value without a bytes field's length prefix.
func walkFields(data []byte, visit func(frame) error) error {
	for len(data) > 0 {
		key, n := decodeVarint(data)
		if n == 0 || key>>3 == 0 || key>>3 >= 1<<29 {
			return errors.New("invalid protobuf field")
		}
		data = data[n:]
		wt := key & 7
		size, err := skipBytes(data, wt)
		if err != nil {
			return err
		}
		value := data[:size]
		if wt == 2 {
			_, prefix := decodeVarint(value)
			value = value[prefix:]
		}
		if err := visit(frame{ID: key >> 3, WT: wt, Payload: value}); err != nil {
			return err
		}
		data = data[size:]
	}
	return nil
}

// request_id is metadata and may precede or follow the selected message.
func decodeDirect(data []byte) (frame, error) {
	var selected frame
	err := walkFields(data, func(f frame) error {
		if selected.ID == 0 || selected.ID == 1 {
			selected = f
		}
		return nil
	})
	if err == nil && selected.ID == 0 {
		err = errors.New("empty message")
	}
	return selected, err
}

// visitMessages yields every logical message from one transport frame. A Max
// packet may contain repeated IDs. Field 1 is request metadata unless alone.
func (b *bus) visitMessages(f *frame, visit func(frame) error) error {
	if b.fr != framingLenDirect || f.ID == 1 {
		return visit(*f)
	}
	return walkFields(f.raw, func(part frame) error {
		if part.ID == 1 {
			return nil
		}
		return visit(part)
	})
}

func skipBytes(b []byte, wt uint64) (int, error) {
	switch wt {
	case 0:
		_, n := decodeVarint(b)
		if n == 0 {
			return 0, errors.New("bad varint")
		}
		return n, nil
	case 1:
		if len(b) < 8 {
			return 0, errors.New("short fixed64")
		}
		return 8, nil
	case 2:
		l, n := decodeVarint(b)
		if n == 0 {
			return 0, errors.New("bad length")
		}
		if l > uint64(len(b)-n) {
			return 0, errors.New("length overrun")
		}
		return n + int(l), nil
	case 5:
		if len(b) < 4 {
			return 0, errors.New("short fixed32")
		}
		return 4, nil
	default:
		return 0, fmt.Errorf("unsupported wire type %d", wt)
	}
}

func bytesHavePrefix(p, want []byte) bool {
	if len(p) < len(want) {
		return false
	}
	for i := range want {
		if p[i] != want[i] {
			return false
		}
	}
	return true
}

// node is a decoded protobuf element used for schema-free rendering.
type node struct {
	Field int     `json:"f"`
	WT    byte    `json:"-"`
	Val   uint64  `json:"v,omitempty"`
	Sint  int64   `json:"i,omitempty"`
	Float float64 `json:"d,omitempty"`
	Str   string  `json:"s,omitempty"`
	B64   string  `json:"b64,omitempty"`
	Kids  []node  `json:"k,omitempty"`
}

func decodeMsg(b []byte, depth int) ([]node, error) {
	var out []node
	for len(b) > 0 {
		key, n := decodeVarint(b)
		if n == 0 {
			return nil, errors.New("bad varint")
		}
		b = b[n:]
		f, wt := key>>3, key&7
		if f == 0 || f > 1<<20 {
			return nil, errors.New("bad field number")
		}
		switch wt {
		case 0:
			v, m := decodeVarint(b)
			if m == 0 {
				return nil, errors.New("bad varint value")
			}
			b = b[m:]
			n2 := node{Field: int(f), WT: 0, Val: v}
			// Negative int32/int64 fields arrive as a 64-bit two's complement
			// varint, which reads as a huge unsigned number without this.
			if int64(v) < 0 {
				n2.Sint = int64(v)
			}
			out = append(out, n2)
		case 1:
			if len(b) < 8 {
				return nil, errors.New("short fixed64")
			}
			bits := le64(b[:8])
			b = b[8:]
			// Without a schema the field could be fixed64, sfixed64 or double,
			// so both readings are kept and the renderer shows each.
			out = append(out, node{Field: int(f), WT: 1, Val: bits, Float: math.Float64frombits(bits)})
		case 2:
			l, m := decodeVarint(b)
			if m == 0 {
				return nil, errors.New("bad length")
			}
			b = b[m:]
			if l > uint64(len(b)) {
				return nil, errors.New("length overrun")
			}
			data := b[:int(l)]
			b = b[int(l):]
			n2 := node{Field: int(f), WT: 2}
			if depth < maxDepth && len(data) > 0 {
				if kids, err := decodeMsg(data, depth+1); err == nil && len(kids) > 0 && !looksTextual(data) {
					n2.Kids = kids
					out = append(out, n2)
					continue
				}
			}
			if s, ok := asText(data); ok {
				n2.Str = s
			} else {
				n2.B64 = base64.StdEncoding.EncodeToString(data)
			}
			out = append(out, n2)
		case 5:
			if len(b) < 4 {
				return nil, errors.New("short fixed32")
			}
			bits := le32(b[:4])
			b = b[4:]
			out = append(out, node{Field: int(f), WT: 5, Val: bits,
				Float: float64(math.Float32frombits(uint32(bits)))})
		default:
			return nil, fmt.Errorf("wire type %d", wt)
		}
	}
	return out, nil
}

func decodeVarint(b []byte) (uint64, int) {
	var v uint64
	for i := 0; i < len(b) && i < 10; i++ {
		if i == 9 && b[i] > 1 {
			return 0, 0
		}
		v |= uint64(b[i]&0x7f) << (7 * i)
		if b[i] < 0x80 {
			return v, i + 1
		}
	}
	return 0, 0
}

func looksTextual(b []byte) bool {
	for _, c := range b {
		if c < 0x20 || c > 0x7e {
			if c != '\t' && c != '\n' && c != '\r' {
				return false
			}
		}
	}
	return len(b) > 0
}

func asText(b []byte) (string, bool) {
	if len(b) > 4096 {
		return "", false
	}
	if !looksTextual(b) {
		return "", false
	}
	return string(b), true
}

// hexStr renders scalar frame bodies, which the Max layout carries as raw bytes.
func hexStr(b []byte) string { return hex.EncodeToString(b) }

// hexdump shows the raw socket bytes, used to check the framing assumptions.
func hexdump(w io.Writer, b []byte) {
	for i := 0; i < len(b); i += 16 {
		end := i + 16
		if end > len(b) {
			end = len(b)
		}
		line := fmt.Sprintf("%04x  ", i)
		for j := i; j < i+16; j++ {
			if j < end {
				line += fmt.Sprintf("%02x ", b[j])
			} else {
				line += "   "
			}
			if j == i+7 {
				line += " "
			}
		}
		line += " |"
		for _, c := range b[i:end] {
			if c >= 0x20 && c <= 0x7e {
				line += string(rune(c))
			} else {
				line += "."
			}
		}
		fmt.Fprintln(w, line+"|")
	}
}

func printTree(kids []node, indent string, w io.Writer) {
	for _, k := range kids {
		switch {
		case len(k.Kids) > 0:
			fmt.Fprintf(w, "%s%d {\n", indent, k.Field)
			printTree(k.Kids, indent+"  ", w)
			fmt.Fprintf(w, "%s}\n", indent)
		case k.Str != "":
			fmt.Fprintf(w, "%s%d: %q\n", indent, k.Field, k.Str)
		case k.B64 != "":
			fmt.Fprintf(w, "%s%d: bytes(%d) %s\n", indent, k.Field, len(k.B64)*3/4, k.B64)
		case k.WT == 2:
			fmt.Fprintf(w, "%s%d: bytes(0)\n", indent, k.Field)
		case k.WT == 1:
			fmt.Fprintf(w, "%s%d: %g (fixed64 0x%016x)\n", indent, k.Field, k.Float, k.Val)
		case k.WT == 5:
			fmt.Fprintf(w, "%s%d: %g (fixed32 0x%08x)\n", indent, k.Field, k.Float, uint32(k.Val))
		case k.Sint != 0:
			fmt.Fprintf(w, "%s%d: %d (varint 0x%016x)\n", indent, k.Field, k.Sint, k.Val)
		default:
			fmt.Fprintf(w, "%s%d: %d\n", indent, k.Field, k.Val)
		}
	}
}

// ------------------------------------------------------------------ quasar.cfg

type service struct {
	Name string
	Port int
}

func cfgCandidates() []string {
	if v := os.Getenv("QUASAR_CFG"); v != "" {
		return []string{v}
	}
	return []string{
		"/system/vendor/quasar/quasar.cfg",
		"/vendor/quasar/quasar.cfg",
		"./quasar.cfg",
	}
}

func loadCfg(path string) (string, map[string]int, error) {
	if path != "" {
		p, err := parseCfg(path)
		if err != nil {
			return "", nil, err
		}
		return path, p, nil
	}
	for _, c := range cfgCandidates() {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		p, err := parseCfg(c)
		if err != nil {
			return c, nil, err
		}
		return c, p, nil
	}
	return "", nil, nil
}

// parseCfg reads the "<service>: { ..., \"port\": N, ... }" map. The file is
// JSON, but values contain ${MACRO} placeholders inside strings only, so a
// strict decode of the top level into raw values is enough.
func parseCfg(path string) (map[string]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ports := map[string]int{}
	for name, raw := range top {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		rm, ok := obj["port"]
		if !ok {
			continue
		}
		var p int
		if err := json.Unmarshal(rm, &p); err != nil {
			continue
		}
		if p > 0 && p < 65536 {
			ports[name] = p
		}
	}
	return ports, nil
}

func listServices(ports map[string]int) {
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if ports[names[i]] == ports[names[j]] {
			return names[i] < names[j]
		}
		return ports[names[i]] < ports[names[j]]
	})
	for _, n := range names {
		fmt.Printf("%-24s %d\n", n, ports[n])
	}
	fmt.Printf("# %d services with a tcp port\n", len(names))
}

// ------------------------------------------------------------------ build detect

// detectBuild picks the id -> type table. Max ships Android's linker64 and a
// QuasarMessage that stops at field 157; the Buildroot columns run the newer
// build with ids above that range.
func detectBuild() string {
	for _, p := range []string{"/system/bin/linker64", "/system/bin/linker"} {
		if _, err := os.Stat(p); err == nil {
			return buildMax
		}
	}
	if _, err := os.Stat("/system/vendor/quasar/quasar_launcher2"); err == nil {
		if _, err := os.Stat("/system/bin/sh"); err == nil {
			return buildLinux
		}
	}
	return buildLinux
}

func resolveTable(flag string) *buildTable {
	name := flag
	if name == "" {
		name = detectBuild()
	}
	t, ok := builds[name]
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown -build %q (want %s or %s)\n", name, buildMax, buildLinux)
		os.Exit(2)
	}
	if t.name == buildLinux {
		for id, e := range builds[buildMax].byID {
			if id > sharedIDLimit {
				continue
			}
			if _, ok := t.byID[id]; !ok {
				t.byID[id] = e
			}
			if _, ok := t.byNm[strings.ToLower(e.Name)]; !ok {
				t.byNm[strings.ToLower(e.Name)] = e
			}
		}
	}
	return t
}

func printMap(t *buildTable) {
	ids := make([]int, 0, len(t.byID))
	for i := range t.byID {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	for _, i := range ids {
		e := t.byID[i]
		fmt.Printf("%-4d %-32s %s\n", i, e.Name, e.Type)
	}
	fmt.Printf("# build %s: %d ids known\n", t.name, len(ids))
}

// ------------------------------------------------------------------ main

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(runAction(os.Args[1:], os.Stdout, os.Stderr))
	}
	var (
		host      = flag.String("h", "127.0.0.1", "host of the quasar bus")
		port      = flag.Int("p", 0, "tcp port (overrides -s)")
		service_  = flag.String("s", "", "service name resolved from quasar.cfg")
		cfgPath   = flag.String("C", "", "quasar.cfg path (default: auto-detect)")
		sendB64   = flag.String("m", "", "message body, base64 (empty body = send nothing)")
		idFlag    = flag.Int("i", -1, "message id (QuasarMessage oneof field number)")
		typeFlag  = flag.String("t", "", "message name, resolved to an id via the built-in map")
		rawMode   = flag.Bool("raw", false, "-m holds exact TCP bytes including any length prefix")
		dur       = flag.Duration("d", 10*time.Second, "read window after subscribing/sending (0 = no limit)")
		count     = flag.Int("n", 0, "stop after N frames (0 = no limit)")
		buildFlag = flag.String("build", "", "id map: "+buildMax+" or "+buildLinux+" (default: auto)")
		jsonOut   = flag.Bool("json", false, "one JSON object per frame")
		noDecode  = flag.Bool("b64", false, "print base64 of the whole frame like the stock client")
		dumpN     = flag.Int("dump", 0, "read up to N raw socket bytes and hexdump them instead of parsing frames")
		listFlag  = flag.Bool("list", false, "list services and ports from quasar.cfg")
		mapFlag   = flag.Bool("map", false, "print the id -> message map")
		probeFlag = flag.Bool("probe", false, "connect to every service and report its first frame")
		framingF  = flag.String("framing", "auto", "frame layout: auto, env (len+252/253), direct (len+oneof), bare")
		wtFlag    = flag.String("wt", "", "wire type override for sending: 0, 1, 2 or 5")
		verboseF  = flag.Bool("v", false, "report the detected framing on stderr")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `quasarc - Yandex Station quasar bus client (Station Max / Station 2 / Station Midi)

  quasarc alice-state | wifi-status | wifi-list   return one JSON snapshot
  quasarc volume get | volume set PERCENT       read or change playback volume
  quasarc setup-status | ntp-status | brick-status | group-status
  quasarc mic get | mic mute | mic unmute       microphone control on the station
  quasarc metrica-uuid | update-status | notification-status
  quasarc bluetooth-status | alarm-status
  quasarc bluetooth on | bluetooth off          allow or block incoming Bluetooth connections
  quasarc wifi-list -help           show action options

  quasarc -s aliced                 subscribe to a service, print decoded frames
  quasarc -s aliced -list           list every service and its port
  quasarc -probe                    poll every service and report its first frame
  quasarc -s aliced -t alice_state -m <base64>   send a message
  quasarc -p 9924 -b64              raw frames only, stock-client style

`)
		flag.PrintDefaults()
	}
	flag.Parse()

	verbose = *verboseF
	fr, ferr := parseFraming(*framingF)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, "error:", ferr)
		os.Exit(2)
	}
	table := resolveTable(*buildFlag)
	cfgFile, ports, err := loadCfg(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	if *listFlag {
		if cfgFile == "" {
			fmt.Fprintln(os.Stderr, "no quasar.cfg found (use -C)")
			os.Exit(1)
		}
		fmt.Printf("# %s\n", cfgFile)
		listServices(ports)
		return
	}
	if *mapFlag {
		printMap(table)
		return
	}

	if *probeFlag {
		if len(ports) == 0 {
			fmt.Fprintln(os.Stderr, "no services found (use -C)")
			os.Exit(1)
		}
		probe(*host, ports, table, fr, *dur)
		return
	}

	p := *port
	if p == 0 && *service_ != "" {
		if ports == nil {
			fmt.Fprintln(os.Stderr, "no quasar.cfg found, use -p")
			os.Exit(1)
		}
		v, ok := ports[*service_]
		if !ok {
			fmt.Fprintf(os.Stderr, "service %q not found in %s\n", *service_, cfgFile)
			os.Exit(1)
		}
		p = v
	}
	if p == 0 {
		fmt.Fprintln(os.Stderr, "no port: use -s <service> or -p <port>")
		flag.Usage()
		os.Exit(2)
	}

	var out *os.File = os.Stdout
	defer func() { _ = out.Sync() }()

	addr := net.JoinHostPort(*host, strconv.Itoa(p))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "connected %s\n", addr)

	if *dumpN > 0 {
		if *dur > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(*dur))
		}
		b := make([]byte, *dumpN)
		n, rerr := io.ReadAtLeast(conn, b, *dumpN)
		if n < 0 {
			n = 0
		}
		fmt.Fprintf(os.Stderr, "read %d bytes (%v)\n", n, rerr)
		hexdump(os.Stdout, b[:n])
		return
	}

	link := newBus(conn, fr, defaultFraming(table.name))

	if *rawMode {
		if *sendB64 == "" {
			fmt.Fprintln(os.Stderr, "-raw needs -m")
			os.Exit(2)
		}
		raw, err := base64.StdEncoding.DecodeString(*sendB64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "-m is not base64:", err)
			os.Exit(2)
		}
		if err := link.writeRaw(raw); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "sent %d raw bytes\n", len(raw))
	} else if *sendB64 != "" || *idFlag >= 0 || *typeFlag != "" {
		id := *idFlag
		name := ""
		if *typeFlag != "" {
			e, ok := table.byNm[strings.ToLower(*typeFlag)]
			if !ok {
				fmt.Fprintf(os.Stderr, "unknown message %q for build %s (see -map)\n", *typeFlag, table.name)
				os.Exit(1)
			}
			id = e.ID
			name = e.Name
		}
		if id < 0 {
			fmt.Fprintln(os.Stderr, "need -i <id> or -t <name>")
			os.Exit(2)
		}
		body, err := base64.StdEncoding.DecodeString(*sendB64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "-m is not base64:", err)
			os.Exit(2)
		}
		f := frame{ID: uint64(id), Payload: body}
		switch {
		case *wtFlag != "":
			wt, cerr := strconv.ParseUint(*wtFlag, 10, 64)
			if cerr != nil || wt > 5 {
				fmt.Fprintln(os.Stderr, "-wt must be 0, 1, 2 or 5")
				os.Exit(2)
			}
			f.WT = wt
		default:
			if e, ok := table.byID[id]; ok {
				f.WT = wtFor(e.Kind)
			}
		}
		link.detect()
		if err := link.write(f); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "sent id=%d %s (%d bytes, %s framing)\n", id, name, len(body), link.fr)
	}

	deadline := time.Time{}
	if *dur > 0 {
		deadline = time.Now().Add(*dur)
	}
	link.setDeadline(deadline)
	readLoop(link, table, out, *count, *jsonOut, *noDecode)
}

func describe(id uint64, t *buildTable) (string, string) {
	if e, ok := t.byID[int(id)]; ok {
		return e.Name, e.Type
	}
	return "", ""
}

func readLoop(b *bus, t *buildTable, out io.Writer, limit int, jsonOut, b64 bool) {
	n := 0
	for {
		f, err := b.next()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				return
			}
			if err != io.EOF {
				fmt.Fprintln(os.Stderr, "read:", err)
			}
			return
		}
		n++
		if b64 {
			fmt.Fprintf(out, "Received message %s\n", base64.StdEncoding.EncodeToString(b.encodeFrame(*f)))
		} else {
			_ = b.visitMessages(f, func(part frame) error { printMessage(part, t, out, jsonOut); return nil })
		}
		if limit > 0 && n >= limit {
			return
		}
	}
}

func printMessage(f frame, t *buildTable, out io.Writer, jsonOut bool) {
	name, typ := describe(f.ID, t)
	if jsonOut {
		rec := struct {
			ID   uint64 `json:"id"`
			Name string `json:"name,omitempty"`
			Type string `json:"type,omitempty"`
			Len  int    `json:"len"`
			B64  string `json:"b64"`
			Tree []node `json:"tree,omitempty"`
		}{f.ID, name, typ, len(f.Payload), base64.StdEncoding.EncodeToString(f.Payload), nil}
		if f.WT == 2 {
			if kids, derr := decodeMsg(f.Payload, 0); derr == nil {
				rec.Tree = kids
			}
		}
		_ = json.NewEncoder(out).Encode(rec)
	} else {
		head := fmt.Sprintf("id=%d", f.ID)
		if name != "" {
			head += " " + name
		}
		if typ != "" {
			head += " (" + typ + ")"
		}
		if f.WT != 2 {
			head += fmt.Sprintf(" wt=%d", f.WT)
		}
		fmt.Fprintf(out, "%s  %d bytes\n", head, len(f.Payload))
		if f.WT == 2 {
			if kids, derr := decodeMsg(f.Payload, 0); derr == nil && len(kids) > 0 {
				printTree(kids, "  ", out)
			} else if len(f.Payload) > 0 {
				fmt.Fprintf(out, "  %s\n", base64.StdEncoding.EncodeToString(f.Payload))
			}
		} else {
			fmt.Fprintf(out, "  %s\n", hexStr(f.Payload))
		}
	}
}

func probe(host string, ports map[string]int, t *buildTable, fr framing, dur time.Duration) {
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if ports[names[i]] == ports[names[j]] {
			return names[i] < names[j]
		}
		return ports[names[i]] < ports[names[j]]
	})
	window := dur
	if window > 3*time.Second {
		window = 3 * time.Second
	}
	for _, name := range names {
		addr := net.JoinHostPort(host, strconv.Itoa(ports[name]))
		conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
		if err != nil {
			fmt.Printf("%-24s %-6d closed\n", name, ports[name])
			continue
		}
		b := newBus(conn, fr, defaultFraming(t.name))
		var dl time.Time
		if window > 0 {
			dl = time.Now().Add(window)
		}
		b.setDeadline(dl)
		f, err := b.next()
		_ = conn.Close()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				fmt.Printf("%-24s %-6d open, silent\n", name, ports[name])
			} else {
				fmt.Printf("%-24s %-6d open, no frame (%v)\n", name, ports[name], err)
			}
			continue
		}
		msg, typ := describe(f.ID, t)
		fmt.Printf("%-24s %-6d id=%d %s %s (%d bytes)\n", name, ports[name], f.ID, msg, typ, len(f.Payload))
	}
}
