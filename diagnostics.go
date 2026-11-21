package main

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/skrashevich/quasarc/protocol"
	"github.com/skrashevich/quasarc/station"
)

const maxDepth = 8

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
		key, n := protocol.DecodeVarint(b)
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
			v, m := protocol.DecodeVarint(b)
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
			bits := binary.LittleEndian.Uint64(b[:8])
			b = b[8:]
			// Without a schema the field could be fixed64, sfixed64 or double,
			// so both readings are kept and the renderer shows each.
			out = append(out, node{Field: int(f), WT: 1, Val: bits, Float: math.Float64frombits(bits)})
		case 2:
			l, m := protocol.DecodeVarint(b)
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
			bits := binary.LittleEndian.Uint32(b[:4])
			b = b[4:]
			out = append(out, node{Field: int(f), WT: 5, Val: uint64(bits),
				Float: float64(math.Float32frombits(uint32(bits)))})
		default:
			return nil, fmt.Errorf("wire type %d", wt)
		}
	}
	return out, nil
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

func describe(id uint64, t *protocol.Table) (string, string) {
	if e, ok := t.ByID(int(id)); ok {
		return e.Name, e.Type
	}
	return "", ""
}

func readLoop(b *protocol.Bus, t *protocol.Table, out io.Writer, limit int, jsonOut, b64 bool) {
	n := 0
	for {
		f, err := b.ReadFrame()
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
			fmt.Fprintf(out, "Received message %s\n", base64.StdEncoding.EncodeToString(b.EncodeFrame(*f)))
		} else {
			_ = b.VisitMessages(f, func(part protocol.Frame) error { printMessage(part, t, out, jsonOut); return nil })
		}
		if limit > 0 && n >= limit {
			return
		}
	}
}

func printMessage(f protocol.Frame, t *protocol.Table, out io.Writer, jsonOut bool) {
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

func probe(out io.Writer, host string, ports map[string]int, t *protocol.Table, fr protocol.Framing, window time.Duration, verbose bool) {
	if window <= 0 || window > 3*time.Second {
		window = 3 * time.Second
	}
	var available []string
	for _, name := range sortedServices(ports) {
		port := ports[name]
		addr := net.JoinHostPort(host, strconv.Itoa(port))
		conn, err := net.DialTimeout("tcp", addr, 400*time.Millisecond)
		if err != nil {
			fmt.Fprintf(out, "%-24s %-6d closed (%v)\n", name, port, err)
			continue
		}
		available = append(available, fmt.Sprintf("%s (%d)", name, port))
		fmt.Fprintf(out, "%-24s %-6d open\n", name, port)
		probeService(out, conn, name, t, fr, window, verbose)
		conn.Close()
	}
	fmt.Fprintf(out, "Available services (%d/%d): %s\n", len(available), len(ports), strings.Join(available, ", "))
}

func probeService(out io.Writer, conn net.Conn, name string, t *protocol.Table, fr protocol.Framing, window time.Duration, verbose bool) {
	b := protocol.NewBus(conn, fr, station.DefaultFraming(t.Build()))
	deadline := time.Now().Add(window)
	// Reserve time for a request and reply when a service has no greeting.
	b.SetReadDeadline(time.Now().Add(window / 3))
	conn.SetWriteDeadline(deadline)
	layout := b.Framing()
	b.SetReadDeadline(deadline)
	if verbose {
		fmt.Fprintln(os.Stderr, name, "framing", layout)
	}
	snapshots := station.ServiceSnapshots(name, layout)
	for _, s := range snapshots {
		if id := s.RequestID(); id != 0 {
			if err := b.WriteFrame(protocol.Frame{ID: id, WT: 2}); err != nil {
				fmt.Fprintf(out, "  %s request: %v\n", s.Name, err)
			}
		}
	}
	seen := make(map[uint64]bool)
	done := make(map[string]bool)
	for time.Now().Before(deadline) {
		f, err := b.ReadFrame()
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) && !errors.Is(err, io.EOF) {
				fmt.Fprintf(out, "  read: %v\n", err)
			}
			break
		}
		err = b.VisitMessages(f, func(part protocol.Frame) error {
			if !seen[part.ID] {
				seen[part.ID] = true
				printMessage(part, t, out, false)
			}
			for _, s := range snapshots {
				if done[s.Name] {
					continue
				}
				v, err := s.Decode(part)
				if err != nil {
					fmt.Fprintf(out, "  %s: %v\n", s.Name, err)
					continue
				}
				if v == nil {
					continue
				}
				data, err := json.Marshal(v)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "  %s: %s\n", s.Name, data)
				done[s.Name] = true
			}
			return nil
		})
		if err != nil {
			fmt.Fprintf(out, "  decode: %v\n", err)
			break
		}
	}
	if len(seen) == 0 {
		fmt.Fprintln(out, "  no frames received")
	}
}
