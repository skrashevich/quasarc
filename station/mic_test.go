package station

import (
	"errors"
	"testing"

	"github.com/skrashevich/quasarc/protocol"
)

func TestEnvironmentMicPresence(t *testing.T) {
	for _, tc := range []struct {
		payload []byte
		muted   bool
		missing bool
	}{
		{nil, false, true},
		{[]byte{0xd0, 1, 0}, false, false},
		{[]byte{0xd0, 1, 1}, true, false},
	} {
		state := protocol.Frame{ID: 4, WT: 2, Payload: tc.payload}
		device := protocol.Frame{ID: 1, WT: 2, Payload: state.EncodeDirect()}
		got, err := environmentMicState(protocol.Frame{ID: 207, WT: 2, Payload: device.EncodeDirect()})
		if tc.missing {
			if !errors.Is(err, errors.ErrUnsupported) {
				t.Fatalf("missing flag: got %v, %v", got, err)
			}
		} else if err != nil || got != tc.muted {
			t.Fatalf("payload %x: got %v, %v", tc.payload, got, err)
		}
	}
}
