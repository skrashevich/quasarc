package main

import (
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
)

type bluetoothResult struct {
	Powered        *bool `json:"powered"`
	Connectable    *bool `json:"connectable"`
	Discoverable   *bool `json:"discoverable"`
	AudioConnected *bool `json:"audio_connected"`
	Playing        *bool `json:"playing"`
}

func sdkBluetoothStatus(f frame) (any, error) {
	sdk, err := sdkFields(f)
	if err != nil || sdk == nil {
		return nil, err
	}
	sink, err := sdk.message(3, 1)
	if err != nil {
		return nil, err
	}
	result := &bluetoothResult{Playing: sink.optionalBool(1), AudioConnected: sink.optionalBool(2)}
	if result.Playing == nil {
		player, err := sdk.message(8, 5)
		if err != nil {
			return nil, err
		}
		if paused := player.optionalBool(1); paused != nil {
			result.Playing = new(!*paused)
		}
	}
	return result, nil
}

func bluetoothStatus(f frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	connection, err := ff.message(1)
	if err != nil {
		return nil, err
	}
	result := new(bluetoothResult)
	switch code, _ := connection.number(1); code {
	case 1: // CONNECTED
		result.AudioConnected = new(true)
	case 3: // DISCONNECTED_ALL. DISCONNECTED alone describes one peer.
		result.AudioConnected = new(false)
	}
	audio, hasAudio := ff.number(2)
	if audio == 1 || audio == 2 {
		result.Playing = new(audio == 1)
	}
	visibility, hasVisibility := ff.number(3)
	if hasVisibility && visibility >= 0 && visibility <= 3 {
		// Max reports DISCOVERABLE (2) for Android's CONNECTABLE_DISCOVERABLE.
		result.Connectable = new(visibility != 0)
		result.Discoverable = new(visibility&2 != 0)
	}
	if connection == nil && !hasAudio && !hasVisibility {
		return nil, nil // Track metadata and AVRCP events share this message ID.
	}
	return result, nil
}

// The bus can be forwarded. Only supplement it with OS state when it was
// opened on loopback using the station's own service configuration.
func (r *bluetoothResult) readLocal(ctx context.Context) error {
	if detectBuild() == buildMax {
		out, err := exec.CommandContext(ctx, "/system/bin/dumpsys", "bluetooth_manager").Output()
		if err != nil {
			return err
		}
		if !r.readAndroidManager(string(out)) {
			return errors.New("Android Bluetooth Manager did not report the adapter scan mode")
		}
		return nil
	}
	out, controllerErr := exec.CommandContext(ctx, "/usr/bin/hciconfig", "hci0").Output()
	if controllerErr == nil && !r.readLinuxController(string(out)) {
		controllerErr = errors.New("missing hci0 controller flags")
	}
	if r.AudioConnected != nil {
		return controllerErr
	}
	out, err := exec.CommandContext(ctx, "/usr/bin/hcitool", "-i", "hci0", "con").Output()
	if err == nil && strings.TrimSpace(string(out)) == "Connections:" {
		r.AudioConnected = new(false)
		return controllerErr
	}
	// An ACL connection alone could belong to a remote control. A capture
	// PCM for the A2DP profile specifically identifies incoming audio.
	if out, err := exec.CommandContext(ctx, "/usr/bin/bluealsa-aplay", "-l").Output(); err == nil && hasA2DPCapture(string(out)) {
		r.AudioConnected = new(true)
	}
	return controllerErr
}

func (r *bluetoothResult) readLinuxController(output string) bool {
	for line := range strings.SplitSeq(output, "\n") {
		flags := strings.Fields(line)
		if len(flags) == 0 || (flags[0] != "UP" && flags[0] != "DOWN") {
			continue
		}
		up := flags[0] == "UP"
		r.Powered = &up
		r.Connectable = new(up && slices.Contains(flags, "PSCAN"))
		r.Discoverable = new(up && slices.Contains(flags, "ISCAN"))
		return true
	}
	return false
}

func hasA2DPCapture(output string) bool {
	capture, adapter := false, false
	for line := range strings.SplitSeq(output, "\n") {
		switch {
		case strings.HasPrefix(line, "****"):
			capture = line == "**** List of CAPTURE Bluetooth Devices ****"
			adapter = false
		case !strings.HasPrefix(line, " "):
			adapter = strings.HasPrefix(line, "hci0:")
		case capture && adapter && strings.HasPrefix(line, "  A2DP ("):
			return true
		}
	}
	return false
}

func (r *bluetoothResult) readAndroidManager(output string) bool {
	var section string
	var haveMode, sinkProfile, disconnected, otherState bool
	var connected bool
	for line := range strings.SplitSeq(output, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") {
			section = line
		}
		switch section {
		case "Bluetooth Status":
			switch line {
			case "  state: ON":
				r.Powered = new(true)
			case "  state: OFF":
				r.Powered = new(false)
			}
		case "AdapterProperties":
			mode, ok := strings.CutPrefix(line, "  ScanMode: ")
			if ok {
				switch mode {
				case "SCAN_MODE_NONE", "SCAN_MODE_CONNECTABLE", "SCAN_MODE_CONNECTABLE_DISCOVERABLE":
					r.Connectable = new(mode != "SCAN_MODE_NONE")
					r.Discoverable = new(mode == "SCAN_MODE_CONNECTABLE_DISCOVERABLE")
					haveMode = true
				}
			}
		}
		if strings.HasPrefix(line, "Profile:") {
			sinkProfile = line == "Profile: A2dpSinkService"
		}
		if !sinkProfile {
			continue
		}
		state, ok := strings.CutPrefix(strings.TrimSpace(line), "StateMachine: name=A2dpSinkStateMachine state=")
		if ok {
			switch state {
			case "Connected":
				connected = true
			case "Disconnected":
				disconnected = true
			default:
				otherState = true
			}
		}
	}
	if r.AudioConnected == nil && (connected || (disconnected && !otherState)) {
		r.AudioConnected = &connected
	}
	if r.Powered != nil && !*r.Powered {
		r.Connectable, r.Discoverable = new(false), new(false)
		return true
	}
	return haveMode
}
