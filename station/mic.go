package station

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/skrashevich/quasarc/protocol"
)

type MicStatus struct {
	Muted         bool  `json:"muted"`
	HardwareMuted *bool `json:"hardware_muted"`
}

func maxMicState(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	n, ok := ff.number(1)
	if !ok || (n != 0 && n != 1) {
		return nil, fmt.Errorf("invalid audiod MicsState")
	}
	return n == 1, nil
}

// environmentMicState reads EnvironmentMessage.device_info.speaker_device_state.mics_muted.
func environmentMicState(f protocol.Frame) (any, error) {
	root, err := unpack(f)
	if err != nil {
		return nil, err
	}
	state, err := root.message(1, 4)
	if err != nil {
		return nil, err
	}
	n, ok := state.number(26)
	if !ok {
		return nil, fmt.Errorf("aliced did not publish mics_muted: %w", errors.ErrUnsupported)
	}
	if n != 0 && n != 1 {
		return nil, fmt.Errorf("aliced: invalid mics_muted value %d", n)
	}
	return n == 1, nil
}

// Microphone reads an explicit mute indication published by the station.
// The physical switch is unknown on these RPC interfaces.
func Microphone() (MicStatus, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, ports, err := LoadConfig("")
	if err != nil {
		return MicStatus{}, err
	}
	port := ports["audiod"]
	a := action{service: "audiod", response: 99, decode: maxMicState}
	if port == 0 {
		port = ports["aliced"]
		if port == 0 {
			return MicStatus{}, fmt.Errorf("microphone status requires audiod or aliced: %w", errors.ErrUnsupported)
		}
		a = action{service: "aliced", response: 207, decode: environmentMicState}
	}
	v, err := a.snapshot(ctx, action{}, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return MicStatus{}, err
	}
	return MicStatus{Muted: v.(bool)}, nil
}

// SetMicrophoneMuted returns ErrUnsupported until a station mute RPC is available.
func SetMicrophoneMuted(bool) (MicStatus, error) {
	return MicStatus{}, fmt.Errorf("setting microphone mute via Quasar RPC: %w", errors.ErrUnsupported)
}
