package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestWiFiSnapshotAcrossBuilds(t *testing.T) {
	// WifiList with one access point, SSID "test" and RSSI -64.
	info := []byte{0x0a, 4, 't', 'e', 's', 't', 0x18}
	info = putVarint(info, ^uint64(63))
	list := frame{ID: 1, WT: 2, Payload: info}.encodeDirect()
	msg := frame{ID: 5, WT: 2, Payload: list}
	for _, tc := range []struct {
		name   string
		body   []byte
		layout framing
	}{
		{"android metadata first", append([]byte{0x0a, 0}, msg.encodeDirect()...), framingLenDirect},
		{"android metadata last", append(msg.encodeDirect(), 0x0a, 0), framingLenDirect},
		{"linux envelope", msg.encodeEnvelope(), framingLenEnv},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newBus(&memConn{r: bytes.NewReader(lengthPrefixed(tc.body))}, framingAuto, framingLenEnv)
			f, err := b.next()
			if err != nil {
				t.Fatal(err)
			}
			if f.ID != 5 || b.fr != tc.layout || !bytes.Equal(b.encodeFrame(*f), tc.body) {
				t.Fatalf("id=%d framing=%v raw frame matches=%v", f.ID, b.fr, bytes.Equal(f.raw, tc.body))
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

func TestCompoundMaxPacket(t *testing.T) {
	// request_id, configuration_state, first_time_setup_started, two channels.
	raw := []byte{0x0a, 0, 0x90, 0x06, 2, 0x80, 0x09, 0, 0xc2, 0x07, 1, 'a', 0xc2, 0x07, 1, 'b'}
	b := testBus(lengthPrefixed(raw), framingLenEnv)
	var output bytes.Buffer
	readLoop(b, resolveTable("max"), &output, 1, true, false)
	dec := json.NewDecoder(&output)
	for _, id := range []int{98, 144, 120, 120} {
		var msg struct{ ID int }
		if err := dec.Decode(&msg); err != nil || msg.ID != id {
			t.Fatalf("expected %d, got %+v (%v)", id, msg, err)
		}
	}
	if dec.More() {
		t.Fatal("unexpected extra message")
	}
	b = testBus(lengthPrefixed(raw), framingLenEnv)
	output.Reset()
	readLoop(b, resolveTable("max"), &output, 1, false, true)
	if want := "Received message " + base64.StdEncoding.EncodeToString(raw) + "\n"; output.String() != want {
		t.Fatalf("raw packet changed: %q", output.String())
	}
}

func TestStatusPayloads(t *testing.T) {
	cases := []struct {
		name   string
		decode func(frame) (any, error)
		f      frame
		want   string
	}{
		{"Max scalar setup", setupStatus, frame{ID: 98, WT: 0, Payload: []byte{2}}, `{"state":"configured","code":2}`},
		{"Linux wrapped setup", setupStatus, frame{ID: 98, WT: 2, Payload: []byte{8, 2}}, `{"state":"configured","code":2}`},
		{"unknown brick code", brickStatus, frame{ID: 66, WT: 0, Payload: []byte{99}}, `{"state":"unknown","code":99}`},
		{"NTP false", ntpStatus, frame{WT: 2, Payload: []byte{8, 0}}, `{"synchronized":false}`},
		{"unrelated IO event", groupEvent, frame{WT: 2, Payload: []byte{8, 1}}, `null`},
		{"wrapped group", groupEvent, frame{WT: 2, Payload: []byte{0xfa, 3, 6, 8, 0, 50, 2, 8, 1}}, `{"role":"stand_alone","microphone_group_state":"listens"}`},
		{"missing signal", wifiStatus, frame{WT: 2, Payload: []byte{8, 3}}, `{"state":"connected","code":3,"signal":null}`},
		{"Alice defaults", aliceState, frame{WT: 2}, `{"state":"idle","code":0,"has_startup_requirements":false}`},
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
