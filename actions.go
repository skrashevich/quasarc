package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

type action struct {
	service           string
	request, response uint64
	decode            func(frame) (any, error)
}

var actions = map[string]action{
	"alice-state":         {"aliced", 0, 3, aliceState},
	"wifi-status":         {"wifid", 0, 35, wifiStatus},
	"wifi-list":           {"wifid", 4, 5, func(f frame) (any, error) { return wifiList(f.Payload) }},
	"setup-status":        {"firstrund", 0, 98, setupStatus},
	"ntp-status":          {"ntpd", 0, 143, ntpStatus},
	"brick-status":        {"brickd", 0, 235, brickStatus},
	"group-status":        {"iohub_clients", 0, 85, groupEvent},
	"metrica-uuid":        {"metricad", 0, 24, metricaUUID},
	"update-status":       {"iohub_clients", 0, 85, sdkUpdateStatus},
	"notification-status": {"iohub_clients", 0, 85, sdkNotificationStatus},
	"bluetooth-status":    {"iohub_clients", 0, 85, sdkBluetoothStatus},
	"alarm-status":        {"iohub_clients", 0, 85, sdkAlarmStatus},
}

func directAction(name string) action {
	switch name {
	case "brick-status":
		return action{"brickd", 0, 66, brickStatus}
	case "group-status":
		return action{"pilotd", 0, 81, groupStatus}
	case "update-status":
		return action{"updatesd", 0, 104, updateStatus}
	case "notification-status":
		return action{"notificationd", 0, 134, notificationStatus}
	case "bluetooth-status":
		return action{"bluetoothd", 0, 72, bluetoothStatus}
	case "alarm-status":
		// Each subscription collects its own timers and alarm settings.
		return action{"alarmd", 0, 0, new(alarmSnapshot).accept}
	default:
		return action{}
	}
}

func runAction(args []string, out, errOut io.Writer) int {
	if args[0] == "mic" {
		return runMic(args[1:], out, errOut)
	}
	if args[0] == "volume" {
		return runVolume(args[1:], out, errOut)
	}
	if args[0] == "bluetooth" {
		return runBluetooth(args[1:], out, errOut)
	}
	if _, ok := actions[args[0]]; !ok {
		fmt.Fprintln(errOut, "available actions: alice-state, wifi-status, wifi-list, volume, setup-status, ntp-status, brick-status, group-status, metrica-uuid, update-status, notification-status, bluetooth-status, bluetooth, alarm-status, mic")
		return 2
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	fs.SetOutput(errOut)
	host := fs.String("h", "127.0.0.1", "station host")
	port := fs.Int("p", 0, "service port (default from config)")
	cfg := fs.String("C", "", "station quasar.cfg")
	timeout := fs.Duration("timeout", 5*time.Second, "total connection and response timeout")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fs.NArg() != 0 || *timeout <= 0 || *port < 0 || *port > 65535 {
		fmt.Fprintln(errOut, "invalid arguments")
		return 2
	}
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	result, localStation, err := readActionBus(ctx, args[0], *host, *port, *cfg)
	if bt, ok := result.(*bluetoothResult); err == nil && ok && localStation {
		_ = bt.readLocal(ctx) // OS fields are optional in a read-only snapshot.
	}
	if err == nil {
		err = json.NewEncoder(out).Encode(result)
	}
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", args[0], err)
		return 1
	}
	return 0
}

func readActionBus(ctx context.Context, name, host string, port int, cfg string) (any, bool, error) {
	a, direct := actions[name], directAction(name)
	localStation := false
	if port == 0 {
		path, ports, err := loadCfg(cfg)
		if err != nil {
			return nil, false, err
		}
		port = ports[a.service]
		if direct.service != "" && port == 0 {
			port = ports[direct.service]
		}
		if port == 0 {
			return nil, false, fmt.Errorf("no port for %s in configuration", a.service)
		}
		localStation = (host == "localhost" || net.ParseIP(host).IsLoopback()) &&
			(path == "/system/vendor/quasar/quasar.cfg" || path == "/vendor/quasar/quasar.cfg")
	}
	deadline, _ := ctx.Deadline()
	if timeout := time.Until(deadline); timeout > 0 {
		result, err := a.snapshot(direct, net.JoinHostPort(host, strconv.Itoa(port)), timeout)
		return result, localStation, err
	}
	return nil, localStation, context.DeadlineExceeded
}

func (a action) snapshot(direct action, addr string, timeout time.Duration) (any, error) {
	deadline := time.Now().Add(timeout)
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, err
	}
	b := newBus(conn, framingAuto, defaultFraming(detectBuild()))
	b.setDeadline(deadline)
	// Both services publish an initial state. Detect the layout from that frame,
	// including on forwarded connections where the local OS is unrelated.
	_, err = b.r.Peek(6)
	if err != nil {
		return nil, err
	}
	b.detect()
	if b.fr == framingLenDirect && direct.service != "" {
		a = direct
	}
	if a.request != 0 {
		if err := b.write(frame{ID: a.request, WT: 2}); err != nil {
			return nil, err
		}
	}
	for {
		f, err := b.next()
		if err != nil {
			return nil, err
		}
		var result any
		err = b.visitMessages(f, func(part frame) error {
			// With response 0 the decoder selects and combines multiple IDs.
			if result != nil || (a.response != 0 && part.ID != a.response) {
				return nil
			}
			var decodeErr error
			result, decodeErr = a.decode(part)
			return decodeErr
		})
		if err != nil || result != nil {
			return result, err
		}
	}
}

type stateResult struct {
	State string `json:"state"`
	Code  int32  `json:"code"`
}

type network struct {
	SSID      *string `json:"ssid"`
	RSSI      *int32  `json:"rssi_dbm"`
	Frequency *int32  `json:"frequency_mhz"`
	Secure    *bool   `json:"secure"`
}

func wifiList(data []byte) (any, error) {
	networks := []network{}
	err := walkFields(data, func(f frame) error {
		if f.ID != 1 || f.WT != 2 {
			return nil
		}
		var n network
		err := walkFields(f.Payload, func(field frame) error {
			if field.ID == 1 && field.WT == 2 {
				ssid := string(field.Payload)
				n.SSID = &ssid
			}
			if field.WT == 0 {
				v, _ := decodeVarint(field.Payload)
				switch field.ID {
				case 2:
					secure := v != 0
					n.Secure = &secure
				case 3:
					rssi := int32(v)
					n.RSSI = &rssi
				case 5:
					freq := int32(v)
					n.Frequency = &freq
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		networks = append(networks, n)
		return nil
	})
	return struct {
		Networks []network `json:"networks"`
	}{networks}, err
}
