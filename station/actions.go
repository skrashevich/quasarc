package station

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"

	"github.com/skrashevich/quasarc/protocol"
)

type action struct {
	service           string
	request, response uint64
	decode            func(protocol.Frame) (any, error)
}

var actions = map[string]action{
	"alice-state":         {"aliced", 0, 3, aliceState},
	"wifi-status":         {"wifid", 0, 35, wifiStatus},
	"wifi-list":           {"wifid", 4, 5, func(f protocol.Frame) (any, error) { return wifiList(f.Payload) }},
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

// ServiceSnapshot describes one readable snapshot on a particular service and layout.
type ServiceSnapshot struct {
	Name   string
	action action
}

// ServiceSnapshots returns fresh decoders for one subscription, including any
// state needed to combine multiple incoming messages into a snapshot.
func ServiceSnapshots(service string, layout protocol.Framing) []ServiceSnapshot {
	var result []ServiceSnapshot
	for name, a := range actions {
		if layout == protocol.Direct {
			if direct := directAction(name); direct.service != "" {
				a = direct
			}
		}
		if a.service == service {
			result = append(result, ServiceSnapshot{name, a})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Name < result[j].Name })
	return result
}

// RequestID is zero for snapshots that the service publishes on subscription.
func (s ServiceSnapshot) RequestID() uint64 { return s.action.request }

// Decode returns nil until a matching snapshot has been received.
func (s ServiceSnapshot) Decode(f protocol.Frame) (any, error) {
	if s.action.response != 0 && f.ID != s.action.response {
		return nil, nil
	}
	return s.action.decode(f)
}

func readActionBus(ctx context.Context, name, host string, port int, cfg string) (any, error) {
	a, direct := actions[name], directAction(name)
	if port == 0 {
		_, ports, err := LoadConfig(cfg)
		if err != nil {
			return nil, err
		}
		port = ports[a.service]
		if direct.service != "" && port == 0 {
			port = ports[direct.service]
		}
		if port == 0 {
			return nil, fmt.Errorf("no port for %s in configuration", a.service)
		}
	}
	result, err := a.snapshot(ctx, direct, net.JoinHostPort(host, strconv.Itoa(port)))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return result, err
}

func (a action) snapshot(ctx context.Context, direct action, addr string) (any, error) {
	conn, err := dialService(ctx, addr)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	b := protocol.NewBus(conn, protocol.Auto, protocol.Envelope)
	b.SetReadDeadline(deadline)
	// Both services publish an initial state. Detect the layout from that frame,
	// including on forwarded connections where the local OS is unrelated.
	layout, err := b.AwaitFraming()
	if err != nil {
		return nil, err
	}
	if layout == protocol.Direct && direct.service != "" {
		a = direct
	}
	if a.request != 0 {
		if err := b.WriteFrame(protocol.Frame{ID: a.request, WT: 2}); err != nil {
			return nil, err
		}
	}
	for {
		f, err := b.ReadFrame()
		if err != nil {
			return nil, err
		}
		var result any
		err = b.VisitMessages(f, func(part protocol.Frame) error {
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

type StateCode struct {
	State string `json:"state"`
	Code  int32  `json:"code"`
}

type Network struct {
	SSID      *string `json:"ssid"`
	RSSI      *int32  `json:"rssi_dbm"`
	Frequency *int32  `json:"frequency_mhz"`
	Secure    *bool   `json:"secure"`
}

func wifiList(data []byte) (any, error) {
	networks := []Network{}
	err := protocol.WalkFields(data, func(f protocol.Frame) error {
		if f.ID != 1 || f.WT != 2 {
			return nil
		}
		var n Network
		err := protocol.WalkFields(f.Payload, func(field protocol.Frame) error {
			if field.ID == 1 && field.WT == 2 {
				ssid := string(field.Payload)
				n.SSID = &ssid
			}
			if field.WT == 0 {
				v, _ := protocol.DecodeVarint(field.Payload)
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
	return WiFiList{networks}, err
}

type WiFiList struct {
	Networks []Network `json:"networks"`
}
