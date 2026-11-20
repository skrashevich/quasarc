package main

import (
	"bytes"
	"encoding/json"
	"net"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestBluetoothAndAlarmCLI(t *testing.T) {
	field := func(id uint64, payload ...byte) []byte {
		return frame{ID: id, WT: 2, Payload: payload}.encodeDirect()
	}
	sdk := func(body []byte) []byte {
		return frame{ID: 85, WT: 2, Payload: field(34, body...)}.encodeEnvelope()
	}
	maxSettings := field(47, 10, 0, 26, 8, 8, 7, 16, 1, 24, 160, 156, 1)
	linuxPlayer := field(8, field(5, 8, 1, 32, 0)...)
	linuxSink := field(3, field(1, 8, 1, 16, 1)...)
	type testCase struct {
		name, command string
		packets       [][]byte
		want          string
	}
	cases := []testCase{
		{"Linux paused player", "bluetooth-status", [][]byte{sdk(linuxPlayer)},
			`{"powered":null,"connectable":null,"discoverable":null,"audio_connected":null,"playing":false}`},
		{"Linux explicit sink takes precedence", "bluetooth-status", [][]byte{sdk(append(linuxPlayer, linuxSink...))},
			`{"powered":null,"connectable":null,"discoverable":null,"audio_connected":true,"playing":true}`},
		{"Max ignores metadata and distinguishes pause from connection", "bluetooth-status", [][]byte{field(72, 42, 0), field(72, 16, 2, 24, 1)},
			`{"powered":null,"connectable":true,"discoverable":false,"audio_connected":null,"playing":false}`},
		{"Max discoverable scan mode", "bluetooth-status", [][]byte{field(72, 16, 2, 24, 2)},
			`{"powered":null,"connectable":true,"discoverable":true,"audio_connected":null,"playing":false}`},
		{"Max peer disconnect and future visibility", "bluetooth-status", [][]byte{field(72, 10, 2, 8, 2, 24, 7)},
			`{"powered":null,"connectable":null,"discoverable":null,"audio_connected":null,"playing":null}`},
		{"Max playing", "bluetooth-status", [][]byte{field(72, 10, 2, 8, 1, 16, 1, 24, 3)},
			`{"powered":null,"connectable":true,"discoverable":true,"audio_connected":true,"playing":true}`},
		{"Linux positive total", "alarm-status", [][]byte{sdk(field(5, 16, 1, 24, 0, 32, 4))},
			`{"alarm_playing":true,"timer_playing":false,"set_alarms_timers_count":4,"timer_count":null,"min_volume_level":null,"max_volume_level":null,"volume_raise_step_ms":null}`},
		{"Linux zero total", "alarm-status", [][]byte{sdk(field(5, 16, 0, 24, 0, 32, 0))},
			`{"alarm_playing":false,"timer_playing":false,"set_alarms_timers_count":0,"timer_count":0,"min_volume_level":null,"max_volume_level":null,"volume_raise_step_ms":null}`},
		{"Linux absent block", "alarm-status", [][]byte{sdk(nil)},
			`{"alarm_playing":null,"timer_playing":null,"set_alarms_timers_count":null,"timer_count":null,"min_volume_level":null,"max_volume_level":null,"volume_raise_step_ms":null}`},
		{"Max compound including empty timers", "alarm-status", [][]byte{append(field(46), maxSettings...)},
			`{"alarm_playing":null,"timer_playing":false,"set_alarms_timers_count":null,"timer_count":0,"min_volume_level":1,"max_volume_level":7,"volume_raise_step_ms":20000}`},
		{"Max split reversed with an unknown timer and a ringing timer", "alarm-status",
			[][]byte{maxSettings, field(72, 16, 1), field(46, 10, 0, 10, 2, 48, 1)},
			`{"alarm_playing":null,"timer_playing":true,"set_alarms_timers_count":null,"timer_count":2,"min_volume_level":1,"max_volume_level":7,"volume_raise_step_ms":20000}`},
		{"Max timer with omitted playing flag", "alarm-status", [][]byte{field(46, 10, 2, 48, 0, 10, 0), field(47)},
			`{"alarm_playing":null,"timer_playing":null,"set_alarms_timers_count":null,"timer_count":2,"min_volume_level":null,"max_volume_level":null,"volume_raise_step_ms":null}`},
		{"Max all timer flags explicitly false", "alarm-status", [][]byte{field(47), field(46, 10, 2, 48, 0)},
			`{"alarm_playing":null,"timer_playing":false,"set_alarms_timers_count":null,"timer_count":1,"min_volume_level":null,"max_volume_level":null,"volume_raise_step_ms":null}`},
		{"Max incomplete subscription", "alarm-status", [][]byte{maxSettings}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, err := net.Listen("tcp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			go func() {
				conn, err := server.Accept()
				if err != nil {
					return
				}
				defer conn.Close()
				for _, pkt := range tc.packets {
					if _, err := conn.Write(lengthPrefixed(pkt)); err != nil {
						return
					}
				}
			}()
			var out, diag bytes.Buffer
			port := strconv.Itoa(server.Addr().(*net.TCPAddr).Port)
			code := runAction([]string{tc.command, "-p", port, "-timeout", "1s"}, &out, &diag)
			if tc.want == "" {
				if code != 1 || out.Len() != 0 || diag.Len() == 0 {
					t.Fatalf("partial snapshot: exit %d, stdout %q, stderr %q", code, &out, &diag)
				}
			} else if code != 0 || strings.TrimSpace(out.String()) != tc.want {
				t.Fatalf("exit %d, stderr %q, got %s; want %s", code, &diag, &out, tc.want)
			}
		})
	}
}

func TestBluetoothSystemSources(t *testing.T) {
	var linux bluetoothResult
	linux.readLinuxController("hci0: Type: Primary\n BD Address: adapter\n UP RUNNING PSCAN ISCAN\n")
	if !reflect.DeepEqual(linux, bluetoothResult{Powered: new(true), Connectable: new(true), Discoverable: new(true)}) {
		t.Fatalf("controller: %+v", linux)
	}
	linux.readLinuxController(" DOWN\n")
	if *linux.Powered || *linux.Connectable || *linux.Discoverable {
		t.Fatal("controller is down")
	}
	const playback = "**** List of PLAYBACK Bluetooth Devices ****\nhci0: paired-device\n  A2DP (SBC): audio\n"
	const capture = "**** List of CAPTURE Bluetooth Devices ****\nhci0: audio-source\n  SCO (CVSD): audio\n"
	if hasA2DPCapture(playback+capture) || !hasA2DPCapture(playback+capture+"  A2DP (SBC): audio\n") {
		t.Fatal("expected only incoming A2DP to count as an audio connection")
	}
	const header = "Bluetooth Status\n  state: ON\n\n"
	const a2dp = "Profile: A2dpSinkService\n  StateMachine: name=A2dpSinkStateMachine state="
	const other = "Profile: HidHostService\n  StateMachine: name=A2dpSinkStateMachine state=Connected\n"
	for _, tc := range []struct {
		profiles string
		want     string
	}{
		{other, "null"}, {a2dp + "Disconnected\n" + other, "false"},
		{a2dp + "Pending\n", "null"}, {a2dp + "Connected\n", "true"},
	} {
		var android bluetoothResult
		android.readAndroidManager(header + tc.profiles)
		value, err := json.Marshal(android.AudioConnected)
		if err != nil || android.Powered == nil || !*android.Powered || string(value) != tc.want {
			t.Fatalf("profiles %q: connected %s, powered %v, error %v", tc.profiles, value, android.Powered, err)
		}
	}
}
