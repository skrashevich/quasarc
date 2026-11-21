package main

import (
	"bytes"
	"testing"
)

func TestInvalidVolumeDoesNotAccessDevice(t *testing.T) {
	for _, s := range []string{"-1", "101", "9.5", "NaN", ""} {
		var out, diagnostic bytes.Buffer
		if code := runVolume([]string{"set", s}, &out, &diagnostic); code != 2 || out.Len() != 0 || diagnostic.Len() == 0 {
			t.Fatalf("unexpected response for %q", s)
		}
	}
}
