package station

import (
	"fmt"

	"github.com/skrashevich/quasarc/protocol"
)

type fields map[uint64]protocol.Frame

func unpack(f protocol.Frame) (fields, error) {
	if f.WT != 2 {
		return nil, fmt.Errorf("message %d has wire type %d, expected bytes", f.ID, f.WT)
	}
	values := fields{}
	err := protocol.WalkFields(f.Payload, func(field protocol.Frame) error { values[field.ID] = field; return nil })
	return values, err
}

func (ff fields) number(id uint64) (int32, bool) {
	f, ok := ff[id]
	if !ok || f.WT != 0 {
		return 0, false
	}
	v, n := protocol.DecodeVarint(f.Payload)
	return int32(v), n != 0
}

// message follows optional embedded messages. A missing parent has no fields.
func (ff fields) message(path ...uint64) (fields, error) {
	for _, id := range path {
		field, ok := ff[id]
		if !ok {
			return nil, nil
		}
		var err error
		ff, err = unpack(field)
		if err != nil {
			return nil, err
		}
	}
	return ff, nil
}

func (ff fields) optionalBool(id uint64) *bool {
	v, ok := ff.number(id)
	if !ok {
		return nil
	}
	return new(v != 0)
}

func (ff fields) optionalInt(id uint64) *int32 {
	v, ok := ff.number(id)
	if !ok {
		return nil
	}
	return &v
}

func enumName(code int32, names ...string) string {
	if code >= 0 && int(code) < len(names) && names[code] != "" {
		return names[code]
	}
	return "unknown"
}

func aliceState(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	code, _ := ff.number(1)
	startup, _ := ff.number(5)
	return AliceState{StateCode{enumName(code, "idle", "listening", "speaking", "busy", "", "shazam", "none"), code}, startup != 0}, nil
}

func wifiStatus(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	code, _ := ff.number(1)
	var signal *string
	if n, ok := ff.number(3); ok {
		value := enumName(n, "weak", "good", "strong", "excellent")
		signal = &value
	}
	return WiFiStatus{StateCode{enumName(code, "not_connected", "connecting", "connected_no_internet", "connected", "not_chosen"), code}, signal}, nil
}

// Max sends scalar enums directly. Linux wraps the enum in field 1.
func statusCode(f protocol.Frame) (int32, error) {
	if f.WT == 0 {
		v, n := protocol.DecodeVarint(f.Payload)
		if n != 0 {
			return int32(v), nil
		}
	}
	ff, err := unpack(f)
	if err != nil {
		return 0, err
	}
	if code, ok := ff.number(1); ok {
		return code, nil
	}
	return 0, fmt.Errorf("missing status code in message %d", f.ID)
}

func setupStatus(f protocol.Frame) (any, error) {
	code, err := statusCode(f)
	return StateCode{enumName(code, "unknown", "configuring", "configured", "data_reset_wait_confirm", "data_reset_in_progress"), code}, err
}

func brickStatus(f protocol.Frame) (any, error) {
	code, err := statusCode(f)
	return StateCode{enumName(code, "", "brick", "not_brick", "unknown"), code}, err
}

func ntpStatus(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	value, ok := ff.number(1)
	if !ok {
		return nil, fmt.Errorf("missing NTP synchronization flag")
	}
	return NTPStatus{value != 0}, nil
}

func groupEvent(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	group, ok := ff[63]
	if !ok {
		return nil, nil
	}
	return groupStatus(group)
}

func groupStatus(f protocol.Frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	result := GroupStatus{}
	if code, ok := ff.number(1); ok {
		label := enumName(code, "stand_alone", "leader", "follower")
		result.Role = &label
	}
	if group, ok := ff[6]; ok {
		nested, err := unpack(group)
		if err != nil {
			return nil, err
		}
		if code, ok := nested.number(1); ok {
			label := enumName(code, "not_listens", "listens", "long_listening")
			result.MicrophoneGroupState = &label
		}
	}
	return result, nil
}

type AliceState struct {
	StateCode
	HasStartupRequirements bool `json:"has_startup_requirements"`
}

type WiFiStatus struct {
	StateCode
	Signal *string `json:"signal"`
}

type NTPStatus struct {
	Synchronized bool `json:"synchronized"`
}

type GroupStatus struct {
	Role                 *string `json:"role"`
	MicrophoneGroupState *string `json:"microphone_group_state"`
}
