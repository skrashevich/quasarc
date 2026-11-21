package station

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"net"
	"testing"

	"github.com/skrashevich/quasarc/protocol"
)

func TestWiFiSnapshotAcrossBuilds(t *testing.T) {
	// WifiList with one access point, SSID "test" and RSSI -64.
	info := []byte{0x0a, 4, 't', 'e', 's', 't', 0x18}
	info = protocol.AppendVarint(info, ^uint64(63))
	list := protocol.Frame{ID: 1, WT: 2, Payload: info}.EncodeDirect()
	msg := protocol.Frame{ID: 5, WT: 2, Payload: list}
	for _, tc := range []struct {
		name   string
		body   []byte
		layout protocol.Framing
	}{
		{"android metadata first", append([]byte{0x0a, 0}, msg.EncodeDirect()...), protocol.Direct},
		{"android metadata last", append(msg.EncodeDirect(), 0x0a, 0), protocol.Direct},
		{"linux envelope", msg.EncodeEnvelope(), protocol.Envelope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, writer := net.Pipe()
			defer reader.Close()
			go func() {
				defer writer.Close()
				_, _ = writer.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(tc.body))))
				_, _ = writer.Write(tc.body)
			}()
			b := protocol.NewBus(reader, protocol.Auto, protocol.Envelope)
			f, err := b.ReadFrame()
			if err != nil {
				t.Fatal(err)
			}
			if f.ID != 5 || b.Framing() != tc.layout || !bytes.Equal(b.EncodeFrame(*f), tc.body) {
				t.Fatalf("id=%d framing=%v raw frame matches=%v", f.ID, b.Framing(), bytes.Equal(b.EncodeFrame(*f), tc.body))
			}
			result, err := wifiList(f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result)
			if err != nil || string(data) != `{"networks":[{"ssid":"test","rssi_dbm":-64,"frequency_mhz":null,"secure":null}]}` {
				t.Fatalf("wifi-list = %s, %v", data, err)
			}
		})
	}
}

func TestStatusPayloads(t *testing.T) {
	cases := []struct {
		name   string
		decode func(protocol.Frame) (any, error)
		f      protocol.Frame
		want   string
	}{
		{"Max scalar setup", setupStatus, protocol.Frame{ID: 98, WT: 0, Payload: []byte{2}}, `{"state":"configured","code":2}`},
		{"Linux wrapped setup", setupStatus, protocol.Frame{ID: 98, WT: 2, Payload: []byte{8, 2}}, `{"state":"configured","code":2}`},
		{"unknown brick code", brickStatus, protocol.Frame{ID: 66, WT: 0, Payload: []byte{99}}, `{"state":"unknown","code":99}`},
		{"NTP false", ntpStatus, protocol.Frame{WT: 2, Payload: []byte{8, 0}}, `{"synchronized":false}`},
		{"unrelated IO event", groupEvent, protocol.Frame{WT: 2, Payload: []byte{8, 1}}, `null`},
		{"wrapped group", groupEvent, protocol.Frame{WT: 2, Payload: []byte{0xfa, 3, 6, 8, 0, 50, 2, 8, 1}}, `{"role":"stand_alone","microphone_group_state":"listens"}`},
		{"missing signal", wifiStatus, protocol.Frame{WT: 2, Payload: []byte{8, 3}}, `{"state":"connected","code":3,"signal":null}`},
		{"Alice defaults", aliceState, protocol.Frame{WT: 2}, `{"state":"idle","code":0,"has_startup_requirements":false}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tc.decode(tc.f)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(result)
			if err != nil || string(data) != tc.want {
				t.Fatalf("got %s, %v", data, err)
			}
		})
	}
}
