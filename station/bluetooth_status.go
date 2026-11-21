package station

import (
	"github.com/skrashevich/quasarc/protocol"
)

type BluetoothStatus struct {
	Powered        *bool `json:"powered"`
	Connectable    *bool `json:"connectable"`
	Discoverable   *bool `json:"discoverable"`
	AudioConnected *bool `json:"audio_connected"`
	Playing        *bool `json:"playing"`
}

func sdkBluetoothStatus(f protocol.Frame) (any, error) {
	sdk, err := sdkFields(f)
	if err != nil || sdk == nil {
		return nil, err
	}
	sink, err := sdk.message(3, 1)
	if err != nil {
		return nil, err
	}
	result := &BluetoothStatus{Playing: sink.optionalBool(1), AudioConnected: sink.optionalBool(2)}
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

func bluetoothStatus(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	connection, err := ff.message(1)
	if err != nil {
		return nil, err
	}
	result := new(BluetoothStatus)
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
