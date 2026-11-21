package station_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/skrashevich/quasarc/protocol"
	"github.com/skrashevich/quasarc/station"
)

func listen(t *testing.T) (net.Listener, station.Client) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln, station.Client{Port: ln.Addr().(*net.TCPAddr).Port}
}

func TestTypedAliceState(t *testing.T) {
	for _, layout := range []protocol.Framing{protocol.Direct, protocol.Envelope} {
		t.Run(layout.String(), func(t *testing.T) {
			ln, client := listen(t)
			server := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err == nil {
					defer conn.Close()
					bus := protocol.NewBus(conn, layout, layout)
					err = bus.WriteFrame(protocol.Frame{ID: 3, WT: 2, Payload: []byte{8, 1, 40, 1}})
				}
				server <- err
			}()
			state, err := client.AliceState(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if err := <-server; err != nil {
				t.Fatal(err)
			}
			if state.State != "listening" || state.Code != 1 || !state.HasStartupRequirements {
				t.Fatalf("unexpected Alice state %+v", state)
			}
		})
	}
}

func TestCancelSnapshotWhileReading(t *testing.T) {
	ln, client := listen(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := client.WiFiStatus(ctx); result <- err }()
	conn, err := ln.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation failed to interrupt the service read")
	}
}
