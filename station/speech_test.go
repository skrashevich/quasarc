package station_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	alice "github.com/skrashevich/quasar-go-genproto/proto/alice/protos/endpoint/capabilities/alice"
	ipc "github.com/skrashevich/quasar-go-genproto/proto/yandex_io/protos"
	"github.com/skrashevich/quasarc/station"
	"google.golang.org/protobuf/proto"
)

func TestSpeakNativePlayback(t *testing.T) {
	const phrase = "Проверка голоса."
	start := ipc.Remoting_AliceCapabilityListenerMethod_TTS_STARTED
	end := ipc.Remoting_AliceCapabilityListenerMethod_TTS_COMPLETED
	failed := ipc.Remoting_AliceCapabilityListenerMethod_ON_ALICE_ERROR
	wrapped := ipc.QuasarMessage_TYPE_REMOTING.Enum()
	alternate := ipc.QuasarMessage_TYPE_ALICE_STATE.Enum()
	frame := func(method ipc.Remoting_AliceCapabilityListenerMethod_Method, id string, kind *ipc.QuasarMessage_Type) []byte {
		t.Helper()
		event := &ipc.Remoting_AliceCapabilityListenerMethod{Method: method.Enum(), RequestId: &id}
		call := &ipc.Remoting{RemoteObjectId: new("AliceCapability"), Method: &ipc.Remoting_AliceCapabilityListenerMethod_{AliceCapabilityListenerMethod: event}}
		msg := &ipc.QuasarMessage{Remoting: call}
		if kind != nil {
			payload, err := proto.Marshal(call)
			if err != nil {
				t.Fatal(err)
			}
			msg = &ipc.QuasarMessage{Type: kind, Payload: payload}
		}
		raw, err := proto.Marshal(msg)
		if err != nil {
			t.Fatal(err)
		}
		return append(binary.LittleEndian.AppendUint32(nil, uint32(len(raw))), raw...)
	}
	for _, tc := range []struct {
		name   string
		direct bool
		events [][]byte
		ok     bool
	}{
		{"Station 2 omits completion ID", false, [][]byte{frame(start, "one", wrapped), frame(end, "", wrapped)}, true},
		{"Max direct RPC", true, [][]byte{frame(start, "one", nil), frame(end, "one", nil)}, true},
		{"mixed callback envelopes", false, [][]byte{frame(start, "one", nil), frame(end, "one", alternate)}, true},
		{"completion before start", false, [][]byte{frame(end, "", wrapped)}, false},
		{"other request completed", false, [][]byte{frame(start, "one", wrapped), frame(end, "two", wrapped)}, false},
		{"overlapping speech", false, [][]byte{frame(start, "one", wrapped), frame(start, "two", wrapped), frame(end, "", wrapped)}, false},
		{"Alice error", false, [][]byte{frame(start, "one", wrapped), frame(failed, "", wrapped), frame(end, "", wrapped)}, false},
		{"truncated callback", false, [][]byte{{9, 0, 0, 0, 1}}, false},
		{"oversized callback", false, [][]byte{binary.LittleEndian.AppendUint32(nil, 512<<10+1)}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ln, forwarded := listen(t)
			cfg := filepath.Join(t.TempDir(), "quasar.cfg")
			bluetoothPort := 0
			if tc.direct {
				bluetoothPort = 42
			}
			if err := os.WriteFile(cfg, fmt.Appendf(nil, `{"aliced":{"port":%d},"bluetoothd":{"port":%d}}`, forwarded.Port, bluetoothPort), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- (station.Client{ConfigPath: cfg}).Speak(ctx, phrase) }()
			ln.(*net.TCPListener).SetDeadline(time.Now().Add(2 * time.Second))
			conn, err := ln.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			conn.SetDeadline(time.Now().Add(2 * time.Second))
			listener := readSpeechCall(t, conn, tc.direct).GetCapabilityAddListenerMethod()
			if listener.GetCapabilityId() != "AliceCapability" {
				t.Fatal("missing Alice subscription")
			}
			call := readSpeechCall(t, conn, tc.direct).GetAliceCapabilityMethod()
			d := call.GetVocalizeDirective()
			if call.GetMethod() != ipc.Remoting_AliceCapabilityMethod_START_VOCALIZE || !call.GetEvents() ||
				d.GetText() != phrase || d.GetVoice() != "" || d.GetTtsSource() != alice.TAliceCapability_TVocalizeDirective_Server ||
				d.GetSynthesisMode() != alice.TAliceCapability_TVocalizeDirective_Append {
				t.Fatal("invalid native speech request")
			}
			if _, err := conn.Write(bytes.Join(tc.events, nil)); err != nil {
				t.Fatal(err)
			}
			conn.Close()
			if err := <-result; (err == nil) != tc.ok || err != nil && strings.Contains(err.Error(), phrase) {
				t.Errorf("Speak returned %v, want success %v", err, tc.ok)
			}
		})
	}
}

// Decode the outgoing frames independently of the bus used by the client.
func readSpeechCall(t *testing.T, r io.Reader, direct bool) *ipc.Remoting {
	t.Helper()
	var n uint32
	if err := binary.Read(r, binary.LittleEndian, &n); err != nil || n == 0 || n > 4096 {
		t.Fatalf("bad RPC length %d (%v)", n, err)
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r, body); err != nil {
		t.Fatal(err)
	}
	var msg ipc.QuasarMessage
	if err := proto.Unmarshal(body, &msg); err != nil {
		t.Fatal(err)
	}
	call := msg.GetRemoting()
	if direct {
		if call == nil || msg.Type != nil || msg.Payload != nil {
			t.Fatal("expected direct Remoting field")
		}
	} else {
		if call != nil || msg.GetType() != ipc.QuasarMessage_TYPE_REMOTING {
			t.Fatal("expected typed Remoting payload")
		}
		call = new(ipc.Remoting)
		if err := proto.Unmarshal(msg.GetPayload(), call); err != nil {
			t.Fatal(err)
		}
	}
	if call.GetRemoteObjectId() != "AliceCapability" {
		t.Fatal("unexpected RPC target")
	}
	return call
}

func TestSpeakCancelsWhileWaitingForPlayback(t *testing.T) {
	ln, client := listen(t)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- client.Speak(ctx, "Проверка") }()
	ln.(*net.TCPListener).SetDeadline(time.Now().Add(3 * time.Second))
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(3 * time.Second))
	readSpeechCall(t, conn, false)
	readSpeechCall(t, conn, false)
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Speak returned %v after cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("still waiting after cancellation")
	}
}

func TestSpeakRejectsInvalidConfigBeforeDialing(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "quasar.cfg")
	for _, tc := range []struct{ input, want string }{
		{`{"aliced":{"port":-1}}`, "invalid aliced port"},
		{`{"aliced":{"port":65536}}`, "invalid aliced port"},
		{`{"aliced":{"port":"9875"}}`, "invalid aliced port"},
		{strings.Repeat(" ", 1<<20+1), "1 MiB"},
	} {
		if err := os.WriteFile(cfg, []byte(tc.input), 0600); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		err := (station.Client{ConfigPath: cfg}).Speak(ctx, "Проверка")
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("Speak returned %v, want %q", err, tc.want)
		}
	}
}
