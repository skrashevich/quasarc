package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Linux input-event-codes.h. These are absolute control levels on the two
// stations: a press followed immediately by a release restores the old level.
const (
	evKey        = 1
	evSwitch     = 5
	keyMicMute   = 248
	swMuteDevice = 14
)

type micInput struct {
	device          string
	eventType, code uint16
	pin             micPin
	audiod          string
}

// The physical GPIO input and the evdev state are distinct. A software event
// changes only evdev; the physical line must independently permit unmuting.
type micPin struct {
	number    int
	activeLow bool
}

func findMicInput() (micInput, error) {
	android := detectBuild() == buildMax
	name := "gpio-keys"
	input := micInput{eventType: evSwitch, code: swMuteDevice}
	if android {
		name = "gpio_keypad"
		input.eventType, input.code = evKey, keyMicMute
		_, ports, err := loadCfg("")
		if err != nil {
			return input, err
		}
		if ports["audiod"] == 0 {
			return input, errors.New("missing audiod in quasar.cfg")
		}
		input.audiod = net.JoinHostPort("127.0.0.1", strconv.Itoa(ports["audiod"]))
	}
	events, err := filepath.Glob("/sys/class/input/event*")
	if err != nil {
		return input, err
	}
	for _, event := range events {
		label, err := os.ReadFile(filepath.Join(event, "device/name"))
		if err != nil || strings.TrimSpace(string(label)) != name {
			continue
		}
		node := filepath.Join(event, "device/device/of_node")
		ref, err := micGPIOSpec(node, android)
		if err != nil {
			return input, err
		}
		input.pin, err = resolveMicPin(ref)
		if err != nil {
			return input, err
		}
		input.device = filepath.Join("/dev/input", filepath.Base(event))
		return input, nil
	}
	return input, fmt.Errorf("microphone input %q not found", name)
}

// A GPIO specifier is three big-endian device-tree cells: controller phandle,
// bank-relative line number and flags. Max lists one specifier per key code.
func micGPIOSpec(node string, android bool) ([]byte, error) {
	if !android {
		ref, err := os.ReadFile(filepath.Join(node, "mute_state/gpios"))
		if err != nil {
			return nil, err
		}
		if len(ref) != 12 {
			return nil, errors.New("invalid mute_state GPIO specifier")
		}
		return ref, nil
	}
	codes, err := os.ReadFile(filepath.Join(node, "key_code"))
	if err != nil {
		return nil, err
	}
	refs, err := os.ReadFile(filepath.Join(node, "key-gpios"))
	if err != nil {
		return nil, err
	}
	if len(codes)%4 != 0 || len(refs) != 3*len(codes) {
		return nil, errors.New("invalid keypad device-tree properties")
	}
	for i := 0; i < len(codes); i += 4 {
		if binary.BigEndian.Uint32(codes[i:]) == keyMicMute {
			return refs[3*i : 3*i+12], nil
		}
	}
	return nil, errors.New("keypad has no KEY_MICMUTE")
}

func resolveMicPin(ref []byte) (micPin, error) {
	bank := binary.BigEndian.Uint32(ref)
	offset := binary.BigEndian.Uint32(ref[4:])
	flags := binary.BigEndian.Uint32(ref[8:])
	nodes, err := filepath.Glob("/sys/class/gpio/gpiochip*/device/of_node/*/gpio-controller")
	if err != nil {
		return micPin{}, err
	}
	for _, node := range nodes {
		handle, err := os.ReadFile(filepath.Join(filepath.Dir(node), "phandle"))
		if err != nil || len(handle) != 4 || binary.BigEndian.Uint32(handle) != bank {
			continue
		}
		chip, _, _ := strings.Cut(node, "/device/")
		base, err := os.ReadFile(filepath.Join(chip, "base"))
		if err != nil {
			return micPin{}, err
		}
		n, err := strconv.Atoi(strings.TrimSpace(string(base)))
		if err != nil {
			return micPin{}, err
		}
		return micPin{n + int(offset), flags&1 != 0}, nil
	}
	return micPin{}, errors.New("cannot resolve microphone switch GPIO")
}

func (p micPin) physicalMute() (bool, error) {
	data, err := os.ReadFile("/sys/kernel/debug/gpio")
	if err != nil {
		return false, fmt.Errorf("read physical microphone switch: %w", err)
	}
	return p.level(string(data))
}

func (p micPin) level(report string) (bool, error) {
	want := fmt.Sprintf("gpio-%d ", p.number)
	for line := range strings.Lines(report) {
		if !strings.HasPrefix(strings.TrimSpace(line), want) {
			continue
		}
		_, state, _ := strings.Cut(line, ")")
		words := strings.Fields(state)
		if len(words) >= 2 && words[0] == "in" && (words[1] == "hi" || words[1] == "lo") {
			return (words[1] == "hi") != p.activeLow, nil
		}
	}
	return false, errors.New("physical microphone switch level unavailable")
}

func (m micInput) softwareMute(file *os.File, deadline time.Time) (bool, error) {
	if m.audiod != "" {
		a := action{service: "audiod", response: 99, decode: maxMicState}
		timeout := time.Until(deadline)
		if timeout <= 0 {
			return false, os.ErrDeadlineExceeded
		}
		v, err := a.snapshot(action{}, m.audiod, timeout)
		if err != nil {
			return false, err
		}
		return v.(bool), nil
	}
	// EVIOCGSW(4) from linux/input.h, with bit 14 for SW_MUTE_DEVICE.
	var bits uint32
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, file.Fd(), 0x8004451b, uintptr(unsafe.Pointer(&bits)))
	if errno != 0 {
		return false, errno
	}
	return bits&(1<<swMuteDevice) != 0, nil
}

func (m micInput) send(file *os.File, mute bool) error {
	// syscall.Timeval follows the running binary's evdev ABI (ARM or ARM64).
	type inputEvent struct {
		Time       syscall.Timeval
		Type, Code uint16
		Value      int32
	}
	events := [2]inputEvent{{Type: m.eventType, Code: m.code}, {}} // SYN_REPORT
	if mute {
		events[0].Value = 1
	}
	return binary.Write(file, binary.NativeEndian, events)
}

// On Station 2 the audio input controller closes PDM capture on mute and
// reopens it on unmute. A switch echo alone does not confirm that action.
func pdmCaptureStatus() (string, error) {
	infos, err := filepath.Glob("/proc/asound/card*/pcm*c/info")
	if err != nil {
		return "", err
	}
	for _, path := range infos {
		data, err := os.ReadFile(path)
		if err != nil {
			return "", err
		}
		for line := range strings.Lines(string(data)) {
			if strings.HasPrefix(line, "id: PDM-dummy ") {
				return filepath.Join(filepath.Dir(path), "sub0/status"), nil
			}
		}
	}
	return "", errors.New("PDM microphone capture status unavailable")
}

// setLevel checks the actual switch before emitting the absolute key/switch
// level. Repeating an absolute level is idempotent, even if audiod's most
// recent report still reflects a previous request.
func (m micInput) setLevel(file *os.File, muted, physical bool) error {
	if physical {
		if !muted {
			return errors.New("microphone is muted by its physical switch")
		}
		return nil
	}
	return m.send(file, muted)
}

func localMic(command string) (micResult, error) {
	m, err := findMicInput()
	if err != nil {
		return micResult{}, err
	}
	access := os.O_RDONLY
	if command != "get" {
		access = os.O_RDWR
	}
	file, err := os.OpenFile(m.device, access, 0)
	if err != nil {
		return micResult{}, err
	}
	defer file.Close()
	var capture string
	if m.audiod == "" {
		capture, err = pdmCaptureStatus()
		if err != nil {
			return micResult{}, err
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	want := command == "mute"
	if command != "get" {
		// Make sure the independent observation source is reachable before writing.
		if _, err := m.softwareMute(file, deadline); err != nil {
			return micResult{}, err
		}
		physical, err := m.pin.physicalMute()
		if err != nil {
			return micResult{}, err
		}
		if err := m.setLevel(file, want, physical); err != nil {
			return micResult{}, err
		}
	}
	for time.Now().Before(deadline) {
		software, err := m.softwareMute(file, deadline)
		if err != nil {
			return micResult{}, err
		}
		physical, err := m.pin.physicalMute()
		if err != nil {
			return micResult{}, err
		}
		result := micResult{Muted: physical || software, HardwareMuted: physical}
		if physical {
			if command == "unmute" {
				if !software {
					if err := m.send(file, true); err != nil {
						return result, err
					}
				}
				return result, errors.New("microphone physical switch changed to mute")
			}
			return result, nil
		}
		if command == "get" || software == want {
			if capture == "" {
				return result, nil
			}
			data, err := os.ReadFile(capture)
			if err != nil {
				return result, err
			}
			line, _, _ := strings.Cut(string(data), "\n")
			if (software && line == "closed") || (!software && line == "state: RUNNING") {
				return result, nil
			}
		}
		time.Sleep(60 * time.Millisecond)
	}
	return micResult{}, errors.New("station did not confirm the microphone state")
}
