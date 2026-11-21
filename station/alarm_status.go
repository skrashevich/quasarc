package station

import (
	"fmt"

	"github.com/skrashevich/quasarc/protocol"
)

type AlarmStatus struct {
	AlarmPlaying         *bool  `json:"alarm_playing"`
	TimerPlaying         *bool  `json:"timer_playing"`
	SetAlarmsTimersCount *int32 `json:"set_alarms_timers_count"`
	TimerCount           *int32 `json:"timer_count"`
	MinVolumeLevel       *int32 `json:"min_volume_level"`
	MaxVolumeLevel       *int32 `json:"max_volume_level"`
	VolumeRaiseStepMS    *int32 `json:"volume_raise_step_ms"`
}

func sdkAlarmStatus(f protocol.Frame) (any, error) {
	sdk, err := sdkFields(f)
	if err != nil || sdk == nil {
		return nil, err
	}
	alarms, err := sdk.message(5)
	if err != nil {
		return nil, err
	}
	result := AlarmStatus{
		AlarmPlaying:         alarms.optionalBool(2),
		TimerPlaying:         alarms.optionalBool(3),
		SetAlarmsTimersCount: alarms.optionalInt(4),
	}
	if count := result.SetAlarmsTimersCount; count != nil {
		if *count < 0 {
			return nil, fmt.Errorf("negative alarm and timer count: %d", *count)
		}
		// A positive combined count does not tell alarms and timers apart.
		if *count == 0 && result.TimerPlaying != nil && !*result.TimerPlaying {
			result.TimerCount = new(int32(0))
		}
	}
	return result, nil
}

// Max publishes TimersState and AlarmsState on the same subscription. Either
// may arrive first, in a compound packet or in separate packets.
type alarmSnapshot struct {
	result     AlarmStatus
	haveTimers bool
	haveAlarms bool
}

func (s *alarmSnapshot) accept(f protocol.Frame) (any, error) {
	switch f.ID {
	case 46:
		count, playing, err := timerStatus(f)
		if err != nil {
			return nil, err
		}
		s.result.TimerCount, s.result.TimerPlaying = &count, playing
		s.haveTimers = true
	case 47:
		alarms, err := unpack(f)
		if err != nil {
			return nil, err
		}
		settings, err := alarms.message(3)
		if err != nil {
			return nil, err
		}
		s.result.MinVolumeLevel = settings.optionalInt(2)
		s.result.MaxVolumeLevel = settings.optionalInt(1)
		s.result.VolumeRaiseStepMS = settings.optionalInt(3)
		s.haveAlarms = true
	}
	if s.haveTimers && s.haveAlarms {
		return s.result, nil
	}
	return nil, nil
}

func timerStatus(f protocol.Frame) (int32, *bool, error) {
	if f.WT != 2 {
		return 0, nil, fmt.Errorf("invalid timers_state wire type %d", f.WT)
	}
	var count int32
	playing, allKnown := false, true
	err := protocol.WalkFields(f.Payload, func(timer protocol.Frame) error {
		if timer.ID != 1 {
			return nil
		}
		values, err := unpack(timer)
		if err != nil {
			return err
		}
		count++
		if v := values.optionalBool(6); v != nil {
			playing = playing || *v
		} else {
			allKnown = false
		}
		return nil
	})
	if err != nil {
		return 0, nil, err
	}
	if playing || allKnown {
		return count, &playing, nil
	}
	return count, nil, nil
}
