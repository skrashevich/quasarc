package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSetBluetoothModeConfirmsController(t *testing.T) {
	for _, target := range []bool{false, true} {
		reads, writes := 0, 0
		read := func(context.Context) (*bluetoothResult, error) {
			reads++
			// The first read after the write still sees the old state.
			state := !target
			if reads >= 3 {
				state = target
			}
			return &bluetoothResult{Powered: new(true), Connectable: &state}, nil
		}
		write := func(_ context.Context, value bool) error {
			writes++
			if value != target {
				t.Fatalf("wrote %t, want %t", value, target)
			}
			return nil
		}
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		snapshot, err := setBluetoothMode(ctx, target, read, write)
		if err != nil || *snapshot.Connectable != target || writes != 1 || reads != 3 {
			t.Fatalf("target %t: reads=%d writes=%d error=%v", target, reads, writes, err)
		}
		if _, err := setBluetoothMode(ctx, target, read, write); err != nil || writes != 1 {
			t.Fatalf("repeated target %t: writes=%d error=%v", target, writes, err)
		}
		cancel()
	}
}

func TestSetBluetoothModeErrors(t *testing.T) {
	for _, src := range []bluetoothResult{
		{Powered: new(false), Connectable: new(false)},
		{Powered: new(true), Connectable: nil},
	} {
		read := func(context.Context) (*bluetoothResult, error) { return &src, nil }
		if _, err := setBluetoothMode(t.Context(), true, read, nil); err == nil {
			t.Fatal("expected error before write")
		}
	}
	read := func(context.Context) (*bluetoothResult, error) {
		return &bluetoothResult{Powered: new(true), Connectable: new(true)}, nil
	}
	ctx, cancel := context.WithCancel(t.Context())
	write := func(context.Context, bool) error { cancel(); return nil }
	if result, err := setBluetoothMode(ctx, false, read, write); result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("an unconfirmed change returned %+v, %v", result, err)
	}
}

func TestAndroidScanModeIsAuthoritative(t *testing.T) {
	for _, tc := range []struct {
		mode                      string
		connectable, discoverable bool
	}{
		{"SCAN_MODE_NONE", false, false},
		{"SCAN_MODE_CONNECTABLE", true, false},
		{"SCAN_MODE_CONNECTABLE_DISCOVERABLE", true, true},
	} {
		state := bluetoothResult{Connectable: new(!tc.connectable), Discoverable: new(!tc.discoverable)}
		if !state.readAndroidManager("Bluetooth Status\n  state: ON\n\nAdapterProperties\n  ScanMode: "+tc.mode+"\n") ||
			*state.Connectable != tc.connectable || *state.Discoverable != tc.discoverable {
			t.Fatalf("wrong controller state for %s", tc.mode)
		}
	}
	var unknown bluetoothResult
	if unknown.readAndroidManager("Bluetooth Status\n  state: ON\nAdapterProperties\n  ScanMode: ERROR\nProfile: HidHostService\n  ScanMode: SCAN_MODE_NONE\n") {
		t.Fatal("accepted an unknown or unrelated scan mode")
	}
	if !unknown.readAndroidManager("Bluetooth Status\n  state: OFF\n") || *unknown.Connectable {
		t.Fatal("powered-off adapter can not accept connections")
	}
}

func TestBluetoothModeCLIArguments(t *testing.T) {
	for _, args := range [][]string{nil, {"enable"}, {"on", "off"}, {"off", "-p", "9892"}} {
		var out, diag bytes.Buffer
		if code := runAction(append([]string{"bluetooth"}, args...), &out, &diag); code != 2 || out.Len() != 0 || diag.Len() == 0 {
			t.Fatalf("invalid arguments %q: exit %d", args, code)
		}
	}
	var help, diag bytes.Buffer
	if code := runAction([]string{"bluetooth", "--help"}, &help, &diag); code != 0 || !strings.Contains(help.String(), "on|off") {
		t.Fatalf("help: exit %d, %q", code, &help)
	}
}

func TestQuasarBluetoothVisibilityRequest(t *testing.T) {
	server, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	cfg := filepath.Join(t.TempDir(), "quasar.cfg")
	if err := os.WriteFile(cfg, fmt.Appendf(nil, `{"bluetoothd":{"port":%d}}`, server.Addr().(*net.TCPAddr).Port), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUASAR_CFG", cfg)
	for _, enable := range []bool{false, true} {
		received := make(chan error, 1)
		go func() {
			conn, err := server.Accept()
			if err != nil {
				received <- err
				return
			}
			defer conn.Close()
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			b := newBus(conn, framingLenDirect, framingLenDirect)
			if err := b.write(frame{ID: 72, WT: 2, Payload: []byte{16, 2, 24, 1}}); err != nil {
				received <- err
				return
			}
			request, err := b.next()
			want := []byte{34, 4, 42, 2, 8, 0}
			if enable {
				want[5] = 1
			}
			if err == nil && (request.ID != 69 || !bytes.Equal(request.Payload, want)) {
				err = fmt.Errorf("unexpected visibility request %+v", request)
			}
			received <- err
		}()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		err := requestBluetoothVisibility(ctx, enable)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		if err := <-received; err != nil {
			t.Fatal(err)
		}
	}
}
