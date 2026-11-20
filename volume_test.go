package main

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"
)

func TestPlaybackPercent(t *testing.T) {
	wrap := func(id uint64, data []byte) []byte { return (frame{ID: id, WT: 2, Payload: data}).encodeDirect() }
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

func TestInvalidVolumeDoesNotAccessDevice(t *testing.T) {
	for _, s := range []string{"-1", "101", "9.5", "NaN", ""} {
		var out, diagnostic bytes.Buffer
		if code := runVolume([]string{"set", s}, &out, &diagnostic); code != 2 || out.Len() != 0 || diagnostic.Len() == 0 {
			t.Fatalf("unexpected response for %q", s)
		}
	}
}
