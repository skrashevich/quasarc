package main

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

func TestMicHardwareSwitch(t *testing.T) {
	const sample = " gpio-50 (        |key) in  hi\n gpio-510 (      |key ) in  lo\n gpio-464 (     |MUTE_STATE ) in  hi\n gpio-482 ( |sysfs ) out hi\n"
	for _, c := range []struct {
		pin          micPin
		muted, valid bool
	}{
		{micPin{510, true}, true, true}, {micPin{464, false}, true, true},
		{micPin{50, true}, false, true}, {micPin{510, false}, false, true},
		{micPin{51, false}, false, false}, {micPin{482, false}, false, false},
	} {
		muted, err := c.pin.level(sample)
		if (err == nil) != c.valid || muted != c.muted {
			t.Errorf("%+v: got %v, %v", c.pin, muted, err)
		}
	}
}

func TestPhysicalMutePreventsEnableEvent(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "events")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	m := micInput{eventType: evKey, code: keyMicMute}
	if err := m.setLevel(f, false, true); err == nil {
		t.Fatal("physical mute accepted unmute")
	}
	if err := m.setLevel(f, true, true); err != nil {
		t.Fatal(err)
	}
	stat, err := f.Stat()
	if err != nil || stat.Size() != 0 {
		t.Fatal("wrote event while hardware mute was active")
	}
}

func TestMaxMicPinFromKeyTable(t *testing.T) {
	dir := t.TempDir()
	cells := func(name string, values ...uint32) {
		var data []byte
		for _, v := range values {
			data = binary.BigEndian.AppendUint32(data, v)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	cells("key_code", 582, keyMicMute)
	cells("key-gpios", 21, 2, 1, 21, 14, 1)
	ref, err := micGPIOSpec(dir, true)
	if err != nil || len(ref) != 12 || binary.BigEndian.Uint32(ref[4:]) != 14 {
		t.Fatalf("unexpected GPIO spec %x, %v", ref, err)
	}
	cells("key-gpios", 21, 2, 1)
	if _, err := micGPIOSpec(dir, true); err == nil {
		t.Fatal("accepted truncated GPIO table")
	}
}
