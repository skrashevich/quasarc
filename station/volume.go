package station

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"strconv"
	"time"

	"github.com/skrashevich/quasarc/protocol"
)

// The capability snapshot carries its playback volume at fields 1 / 11 / 6.
func playbackPercent(data []byte) (float64, bool, error) {
	for _, id := range []uint64{1, 11} {
		var child []byte
		err := protocol.WalkFields(data, func(f protocol.Frame) error {
			if f.ID == id && f.WT == 2 {
				child = f.Payload
			}
			return nil
		})
		if err != nil || child == nil {
			return 0, false, err
		}
		data = child
	}
	var percent float64
	var present bool
	err := protocol.WalkFields(data, func(f protocol.Frame) error {
		if f.ID == 6 && f.WT == 5 {
			percent = float64(math.Float32frombits(binary.LittleEndian.Uint32(f.Payload)))
			if math.IsNaN(percent) || percent < 0 || percent > 100 {
				return errors.New("invalid playback volume")
			}
			present = true
		}
		return nil
	})
	return percent, present, err
}

func volumeDirective(percent int) protocol.Frame {
	bytesField := func(id uint64, body []byte) []byte {
		return (protocol.Frame{ID: id, WT: 2, Payload: body}).EncodeDirect()
	}
	// TDirective.VolumeSet, percent_level (float32), source BUTTON.
	payload := binary.LittleEndian.AppendUint32([]byte{13}, math.Float32bits(float32(percent)))
	payload = append(payload, 24, 1)
	directive := bytesField(1, []byte("directive"))
	directive = append(directive, bytesField(2, []byte("volume_set"))...)
	directive = append(directive, 32, 1) // ignore_answer
	directive = append(directive, bytesField(15, payload)...)
	directive = protocol.AppendVarint(directive, 20<<3)
	directive = protocol.AppendVarint(directive, 254)
	return protocol.Frame{ID: 88, WT: 2, Payload: bytesField(95, bytesField(1, bytesField(1, directive)))}
}

func stationVolume(ctx context.Context, target *int) (float64, error) {
	_, ports, err := LoadConfig("")
	if err != nil {
		return 0, err
	}
	if ports["stereo_pair"] == 0 && ports["volume_setd"] != 0 {
		return maxVolume(ctx, ports["volume_setd"], target)
	}
	open := func(service string) (*protocol.Bus, error) {
		port := ports[service]
		if port == 0 {
			return nil, fmt.Errorf("volume RPC requires %s in quasar.cfg: %w", service, errors.ErrUnsupported)
		}
		conn, err := dialService(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		return protocol.NewBus(conn, protocol.Envelope, protocol.Envelope), nil
	}
	state, err := open("stereo_pair")
	if err != nil {
		return 0, err
	}
	defer state.Close()
	// This firmware exposes five-percentage-point playback steps.
	if target != nil {
		step := ((*target + 2) / 5) * 5
		target = &step
	}
	sent := false
	for {
		f, err := state.ReadFrame()
		if err != nil {
			return 0, fmt.Errorf("read playback volume: %w", err)
		}
		if f.ID != 198 {
			continue
		}
		percent, ok, err := playbackPercent(f.Payload)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		if target == nil || math.Abs(percent-float64(*target)) < 0.01 {
			return percent, nil
		}
		if !sent {
			control, err := open("iohub_clients")
			if err != nil {
				return 0, err
			}
			defer control.Close()
			if err := control.WriteFrame(volumeDirective(*target)); err != nil {
				return 0, err
			}
			sent = true
		}
	}
}

// Volume returns the local playback volume as a percentage.
func Volume(ctx context.Context) (float64, error) { return volume(ctx, nil) }

// SetVolume sets the local playback level and returns the confirmed percentage.
func SetVolume(ctx context.Context, percent int) (float64, error) {
	if percent < 0 || percent > 100 {
		return 0, errors.New("volume must be between 0 and 100")
	}
	return volume(ctx, &percent)
}

func volume(ctx context.Context, target *int) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	return stationVolume(ctx, target)
}

// VolumeSetService exposes the user-facing Alice level (0..10) in VolumeState,
// while its write takes separate speaker, HDMI maximum and Alice levels.
func maxVolume(ctx context.Context, port int, target *int) (float64, error) {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	conn, err := dialService(ctx, addr)
	if err != nil {
		return 0, err
	}
	defer conn.Close()
	bus := protocol.NewBus(conn, protocol.Direct, protocol.Direct)
	var wanted int
	if target != nil {
		wanted = (*target + 5) / 10
	}
	sent := false
	for {
		f, err := bus.ReadFrame()
		if err != nil {
			return 0, fmt.Errorf("volume_setd: %w", err)
		}
		level := -1
		err = bus.VisitMessages(f, func(part protocol.Frame) error {
			if part.ID != 42 {
				return nil
			}
			ff, err := unpack(part)
			if err != nil {
				return err
			}
			value, ok := ff.number(7)
			if !ok || value < 0 || value > 10 {
				return fmt.Errorf("volume_setd: invalid Alice volume level")
			}
			level = int(value)
			return nil
		})
		if err != nil {
			return 0, err
		}
		if level < 0 {
			continue
		}
		if target == nil || level == wanted {
			return float64(level * 10), nil
		}
		if !sent {
			// The stock Android service uses a 0..100 media stream for the speaker.
			body := []byte{8, byte(wanted * 10), 16, 100, 24, byte(wanted)}
			if err := bus.WriteFrame(protocol.Frame{ID: 78, WT: 2, Payload: body}); err != nil {
				return 0, err
			}
			sent = true
		}
	}
}
