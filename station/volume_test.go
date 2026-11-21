package station

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/skrashevich/quasarc/protocol"
)

func TestPlaybackPercent(t *testing.T) {
	wrap := func(id uint64, data []byte) []byte {
		return (protocol.Frame{ID: id, WT: 2, Payload: data}).EncodeDirect()
	}
	for _, p := range []float32{0, 55, 100} {
		state := binary.LittleEndian.AppendUint32([]byte{6<<3 | 5}, math.Float32bits(p))
		got, present, err := playbackPercent(wrap(1, wrap(11, state)))
		if err != nil || !present || got != float64(p) {
			t.Fatalf("read %v: %v %v %v", p, got, present, err)
		}
	}
	if _, present, err := playbackPercent(nil); err != nil || present {
		t.Fatal("missing volume interpreted as a valid level")
	}
}
