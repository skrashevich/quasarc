package protocol

import (
	"bufio"
	"errors"
	"fmt"
)

// AppendVarint appends an unsigned protobuf varint.
func AppendVarint(dst []byte, v uint64) []byte {
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

// Frame is one decoded bus message. WT records the wire type the body travelled
// with, which the Max layout needs in order to resend a frame unchanged.
type Frame struct {
	raw     []byte // complete received protobuf message, excluding the length prefix
	ID      uint64
	WT      uint64
	Payload []byte
}

// EncodeEnvelope renders the newer build's message: field 252 holds the body,
// field 253 the id.
func (f Frame) EncodeEnvelope() []byte {
	b := AppendVarint(nil, fieldPayload<<3|2)
	b = AppendVarint(b, uint64(len(f.Payload)))
	b = append(b, f.Payload...)
	b = AppendVarint(b, fieldID<<3|0)
	return AppendVarint(b, f.ID)
}

// EncodeDirect renders the Max build's message, where the body is the value of
// the oneof field numbered by the id. An empty body is sent as an empty
// length-delimited value because scalars are carried as raw value bytes.
func (f Frame) EncodeDirect() []byte {
	wt := f.WT
	if wt == 0 && len(f.Payload) == 0 {
		wt = 2
	}
	b := AppendVarint(nil, f.ID<<3|wt)
	if wt == 2 {
		b = AppendVarint(b, uint64(len(f.Payload)))
	}
	return append(b, f.Payload...)
}

// DecodeEnvelope parses a {252 body, 253 id} message.
func DecodeEnvelope(b []byte) (Frame, error) {
	var f Frame
	f.WT = 2
	seen := false
	for len(b) > 0 {
		key, n := DecodeVarint(b)
		if n == 0 {
			return f, errors.New("bad tag varint")
		}
		b = b[n:]
		fn, wt := key>>3, key&7
		switch {
		case fn == fieldPayload && wt == 2:
			l, m := DecodeVarint(b)
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
			v, m := DecodeVarint(b)
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

// WalkFields exposes the wire value without a bytes field's length prefix.
func WalkFields(data []byte, visit func(Frame) error) error {
	for len(data) > 0 {
		key, n := DecodeVarint(data)
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
			_, prefix := DecodeVarint(value)
			value = value[prefix:]
		}
		if err := visit(Frame{ID: key >> 3, WT: wt, Payload: value}); err != nil {
			return err
		}
		data = data[size:]
	}
	return nil
}

// DecodeDirect selects the first message other than request metadata. The request_id field is metadata and may precede or follow the selected message.
func DecodeDirect(data []byte) (Frame, error) {
	var selected Frame
	err := WalkFields(data, func(f Frame) error {
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

func skipBytes(b []byte, wt uint64) (int, error) {
	switch wt {
	case 0:
		_, n := DecodeVarint(b)
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
		l, n := DecodeVarint(b)
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

// DecodeVarint returns the unsigned value and bytes consumed, or (0, 0) on invalid input.
func DecodeVarint(b []byte) (uint64, int) {
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
