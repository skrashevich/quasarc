package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

func runBluetooth(args []string, out, errOut io.Writer) int {
	const usage = "usage: quasarc bluetooth on|off (allow or block incoming Bluetooth Classic connections)"
	if len(args) == 1 && (args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintln(out, usage)
		return 0
	}
	if len(args) != 1 || (args[0] != "on" && args[0] != "off") {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	result, err := setBluetoothMode(ctx, args[0] == "on", localBluetoothSnapshot, writeBluetoothMode)
	if err == nil {
		err = json.NewEncoder(out).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(errOut, "bluetooth:", err)
		return 1
	}
	return 0
}

func localBluetoothSnapshot(ctx context.Context) (*bluetoothResult, error) {
	value, local, err := readActionBus(ctx, "bluetooth-status", "127.0.0.1", 0, "")
	if err != nil {
		return nil, err
	}
	if !local {
		return nil, errors.New("run bluetooth on/off on the station with its system quasar.cfg")
	}
	bt := value.(*bluetoothResult)
	if err := bt.readLocal(ctx); err != nil {
		return nil, fmt.Errorf("read Bluetooth controller: %w", err)
	}
	return bt, nil
}

// Only a fresh controller snapshot can confirm the write. In particular, the
// Max Quasar daemon can acknowledge a request while Android keeps accepting peers.
func setBluetoothMode(ctx context.Context, enabled bool,
	read func(context.Context) (*bluetoothResult, error),
	write func(context.Context, bool) error) (*bluetoothResult, error) {
	sent := false
	for {
		state, err := read(ctx)
		if err != nil {
			return nil, err
		}
		if state.Connectable == nil {
			return nil, errors.New("Bluetooth controller did not report connectability")
		}
		if *state.Connectable == enabled {
			return state, nil
		}
		if !sent {
			if enabled && state.Powered != nil && !*state.Powered {
				return nil, errors.New("Bluetooth adapter is powered off")
			}
			if err := write(ctx, enabled); err != nil {
				return nil, err
			}
			sent = true
			continue
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for connectable=%t: %w", enabled, ctx.Err())
		case <-time.After(100 * time.Millisecond):
		}
	}
}

func writeBluetoothMode(ctx context.Context, enabled bool) error {
	if detectBuild() == buildMax {
		return writeAndroidBluetoothMode(ctx, enabled)
	}
	mode := "off"
	if enabled {
		mode = "on"
	}
	cmd := exec.CommandContext(ctx, "/usr/bin/bluetoothctl", "--timeout", "1", "mgmt.connectable", mode)
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("bluetoothctl: %w (%s)", err, strings.TrimSpace(string(output)))
	}
	return nil
}

// Other Android firmware can implement BluetoothRequest.sink_request.set_visibility
// directly. The caller verifies the change against the controller in bluetooth-status.
func requestBluetoothVisibility(ctx context.Context, enabled bool) error {
	_, ports, err := loadCfg("")
	if err != nil {
		return err
	}
	port := ports["bluetoothd"]
	if port == 0 {
		return errors.New("no port for bluetoothd in configuration")
	}
	var dialer net.Dialer
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			return err
		}
	}
	b := newBus(conn, framingLenDirect, framingLenDirect)
	if _, err := b.next(); err != nil { // consume the subscription's initial state
		return err
	}
	visibility := byte(0) // NONE
	if enabled {
		visibility = 1 // CONNECTABLE
	}
	return b.write(frame{ID: 69, WT: 2, Payload: []byte{0x22, 4, 0x2a, 2, 0x08, visibility}})
}
