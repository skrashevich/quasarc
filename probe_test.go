package main

import (
	"bytes"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/skrashevich/quasarc/protocol"
)

func TestProbeRequestsAndDisplaysSnapshots(t *testing.T) {
	for _, layout := range []protocol.Framing{protocol.Direct, protocol.Envelope} {
		t.Run(layout.String(), func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer ln.Close()
			served := make(chan error, 1)
			go func() {
				conn, err := ln.Accept()
				if err != nil {
					served <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(time.Second))
				bus := protocol.NewBus(conn, layout, layout)
				if err := bus.WriteFrame(protocol.Frame{ID: 35, WT: 2, Payload: []byte{8, 3}}); err != nil {
					served <- err
					return
				}
				f, err := bus.ReadFrame()
				if err != nil {
					served <- err
					return
				}
				if f.ID != 4 {
					served <- fmt.Errorf("unexpected request %d", f.ID)
					return
				}
				served <- bus.WriteFrame(protocol.Frame{ID: 5, WT: 2, Payload: []byte{10, 5, 10, 3, 'L', 'A', 'N'}})
			}()
			var out bytes.Buffer
			port := ln.Addr().(*net.TCPAddr).Port
			probe(&out, "127.0.0.1", map[string]int{"wifid": port}, resolveTable("linux"), protocol.Auto, time.Second, false)
			if err := <-served; err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"id=35", "id=5", "wifi-status:", `"state":"connected"`, "wifi-list:", `"ssid":"LAN"`, fmt.Sprintf("Available services (1/1): wifid (%d)", port)} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("missing %q in %s", want, out.String())
				}
			}
		})
	}
}
