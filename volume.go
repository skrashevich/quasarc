package main

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

func runVolume(args []string, out, errOut io.Writer) int {
	const usage = "usage: quasarc volume get | quasarc volume set PERCENT (integer 0..100)"
	var target *int
	switch {
	case len(args) == 1 && (args[0] == "-help" || args[0] == "--help"):
		fmt.Fprintln(out, usage)
		return 0
	case len(args) == 1 && args[0] == "get":
	case len(args) == 2 && args[0] == "set":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 || n > 100 {
			fmt.Fprintln(errOut, usage)
			return 2
		}
		target = &n
	default:
		fmt.Fprintln(errOut, usage)
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	var percent float64
	var err error
	if detectBuild() == buildMax {
		percent, err = androidVolume(ctx, target)
	} else {
		percent, err = stationVolume(ctx, target)
	}
	if err == nil {
		err = json.NewEncoder(out).Encode(struct {
			Percent float64 `json:"percent"`
		}{percent})
	}
	if err != nil {
		fmt.Fprintln(errOut, "volume:", err)
		return 1
	}
	return 0
}

var mediaLevel = regexp.MustCompile(`volume is ([0-9]+) in range \[([0-9]+)\.\.([0-9]+)\]`)

func androidVolume(ctx context.Context, target *int) (float64, error) {
	read := func() (int, int, int, error) {
		output, err := exec.CommandContext(ctx, "/system/bin/media", "volume", "--stream", "3", "--get").CombinedOutput()
		if err != nil {
			return 0, 0, 0, fmt.Errorf("read STREAM_MUSIC: %w", err)
		}
		m := mediaLevel.FindString(string(output))
		if m == "" {
			return 0, 0, 0, errors.New("missing STREAM_MUSIC volume")
		}
		var current, low, high int
		if _, err := fmt.Sscanf(m, "volume is %d in range [%d..%d]", &current, &low, &high); err != nil || high <= low || current < low || current > high {
			return 0, 0, 0, errors.New("invalid STREAM_MUSIC range")
		}
		return current, low, high, nil
	}
	current, low, high, err := read()
	if err != nil {
		return 0, err
	}
	if target != nil {
		wanted := low + int(math.Round(float64(*target)*float64(high-low)/100))
		if current != wanted {
			if err := exec.CommandContext(ctx, "/system/bin/media", "volume", "--stream", "3", "--set", strconv.Itoa(wanted)).Run(); err != nil {
				return 0, err
			}
			current, low, high, err = read()
			if err != nil {
				return 0, err
			}
			if current != wanted {
				return 0, fmt.Errorf("STREAM_MUSIC returned index %d after requesting %d", current, wanted)
			}
		}
	}
	return 100 * float64(current-low) / float64(high-low), nil
}

// The capability snapshot carries its playback volume at fields 1 / 11 / 6.
func playbackPercent(data []byte) (float64, bool, error) {
	for _, id := range []uint64{1, 11} {
		var child []byte
		err := walkFields(data, func(f frame) error {
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
	err := walkFields(data, func(f frame) error {
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

func volumeDirective(percent int) frame {
	bytesField := func(id uint64, body []byte) []byte { return (frame{ID: id, WT: 2, Payload: body}).encodeDirect() }
	// TDirective.VolumeSet, percent_level (float32), source BUTTON.
	payload := binary.LittleEndian.AppendUint32([]byte{13}, math.Float32bits(float32(percent)))
	payload = append(payload, 24, 1)
	directive := bytesField(1, []byte("directive"))
	directive = append(directive, bytesField(2, []byte("volume_set"))...)
	directive = append(directive, 32, 1) // ignore_answer
	directive = append(directive, bytesField(15, payload)...)
	directive = putVarint(directive, 20<<3)
	directive = putVarint(directive, 254)
	return frame{ID: 88, WT: 2, Payload: bytesField(95, bytesField(1, bytesField(1, directive)))}
}

func stationVolume(ctx context.Context, target *int) (float64, error) {
	_, ports, err := loadCfg("")
	if err != nil {
		return 0, err
	}
	open := func(service string) (*bus, error) {
		port := ports[service]
		if port == 0 {
			return nil, fmt.Errorf("missing %s in quasar.cfg", service)
		}
		var d net.Dialer
		conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, err
		}
		deadline, _ := ctx.Deadline()
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
		return newBus(conn, framingLenEnv, framingLenEnv), nil
	}
	state, err := open("stereo_pair")
	if err != nil {
		return 0, err
	}
	defer state.c.Close()
	// This firmware exposes five-percentage-point playback steps.
	if target != nil {
		step := ((*target + 2) / 5) * 5
		target = &step
	}
	sent := false
	for {
		f, err := state.next()
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
			defer control.c.Close()
			if err := control.write(volumeDirective(*target)); err != nil {
				return 0, err
			}
			sent = true
		}
	}
}
