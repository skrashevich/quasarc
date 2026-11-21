package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/skrashevich/quasarc/protocol"
)

func TestCompoundMaxPacket(t *testing.T) {
	// request_id, configuration_state, first_time_setup_started, two channels.
	raw := []byte{0x0a, 0, 0x90, 0x06, 2, 0x80, 0x09, 0, 0xc2, 0x07, 1, 'a', 0xc2, 0x07, 1, 'b'}
	b := testBus(t, lengthPrefixed(raw), protocol.Envelope)
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
	b = testBus(t, lengthPrefixed(raw), protocol.Envelope)
	output.Reset()
	readLoop(b, resolveTable("max"), &output, 1, false, true)
	if want := "Received message " + base64.StdEncoding.EncodeToString(raw) + "\n"; output.String() != want {
		t.Fatalf("raw packet changed: %q", output.String())
	}
}
