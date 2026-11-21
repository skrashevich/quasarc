//go:build kuyase

// Black-box tests for the quasarc executable inside a Kuyase guest.
package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"testing"
	"time"
)

const cli = "/tmp/quasarc"

func TestMain(m *testing.M) {
	host, err := os.Hostname()
	if err != nil || (host != "qemu-station2" && host != "qemu-yandexmini") {
		fmt.Fprintln(os.Stderr, "run these tests in a Kuyase station VM")
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func output(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), cli, args...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("quasarc %v: %v", args, err)
	}
	return out
}

func TestFirmwareConfig(t *testing.T) {
	out := output(t, "-list")
	if !regexp.MustCompile(`(?m)^aliced[ \t]+9875$`).Match(out) {
		t.Fatalf("expected aliced on port 9875 in firmware config:\n%s", out)
	}
}

// Kuyase broadcasts LISTENING to subscribers when AliceCapability receives
// START_CONVERSATION (method 2). Only the Remoting body is fixed here. The CLI
// encodes the outer frame and the emulator independently generates the reply.
func startConversation(t *testing.T, layout string) {
	t.Helper()
	const body = "Og9BbGljZUNhcGFiaWxpdHlCDAgCGAEiBmJ1dHRvbg=="
	out := output(t, "-s", "aliced", "-framing", layout, "-i", "210", "-wt", "2",
		"-m", body, "-n", "1", "-d", "3s", "-json")
	var frame struct {
		ID   int
		Name string
		B64  string
	}
	if err := json.Unmarshal(out, &frame); err != nil {
		t.Fatalf("%s: invalid frame %q: %v", layout, out, err)
	}
	if frame.ID != 3 || frame.Name != "alice_state" || frame.B64 != "CAE=" {
		t.Fatalf("%s: expected AliceState LISTENING frame, got %s", layout, out)
	}
}

func TestAliceState(t *testing.T) {
	for _, layout := range []string{"direct", "env"} {
		t.Run(layout, func(t *testing.T) {
			// Establish the format that Kuyase uses for new subscribers.
			startConversation(t, layout)
			var stdout bytes.Buffer
			reader := exec.CommandContext(t.Context(), cli, "alice-state", "-timeout", "10s")
			reader.Stdout, reader.Stderr = &stdout, os.Stderr
			if err := reader.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- reader.Wait() }()

			// Stub aliced only broadcasts on a command. Keep triggering
			// the event until the separate snapshot process receives it.
			tick := time.NewTicker(200 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-tick.C:
					startConversation(t, layout)
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
					var snapshot struct {
						State   string
						Code    int
						Startup *bool `json:"has_startup_requirements"`
					}
					if err := json.Unmarshal(stdout.Bytes(), &snapshot); err != nil {
						t.Fatalf("invalid snapshot %q: %v", stdout.String(), err)
					}
					if snapshot.State != "listening" || snapshot.Code != 1 || snapshot.Startup == nil || *snapshot.Startup {
						t.Fatalf("unexpected alice-state snapshot: %s", stdout.String())
					}
					return
				}
			}
		})
	}
}

func TestSnapshotTimeout(t *testing.T) {
	// iohub_services accepts connections and stays silent without a request.
	cmd := exec.CommandContext(t.Context(), cli, "alice-state", "-p", "9903", "-timeout", "500ms")
	out, err := cmd.Output()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 1 || len(out) != 0 {
		t.Fatalf("expected exit 1 and empty stdout, got %v, %q", err, out)
	}
	if !bytes.Contains(exit.Stderr, []byte("deadline exceeded")) && !bytes.Contains(exit.Stderr, []byte("i/o timeout")) {
		t.Fatalf("expected a timeout diagnostic, got %q", exit.Stderr)
	}
}

func TestAudioSubscription(t *testing.T) {
	// A passive subscriber receives legacy direct frames from yio_audio.
	out := output(t, "-p", "9850", "-framing", "direct", "-n", "1", "-d", "5s", "-json")
	var frame struct {
		ID  int
		Len int
		B64 []byte
	}
	if err := json.Unmarshal(out, &frame); err != nil {
		t.Fatal(err)
	}
	if frame.ID != 158 || len(frame.B64) == 0 || len(frame.B64) != frame.Len || !bytes.Contains(frame.B64, []byte("raw_mic_0")) {
		t.Fatalf("unexpected yio_audio frame: id=%d, payload length=%d, reported length=%d", frame.ID, len(frame.B64), frame.Len)
	}
}
