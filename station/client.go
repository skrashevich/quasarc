// Package station reads service snapshots from the Quasar RPC bus.
package station

import (
	"context"
	"fmt"
	"net"
	"time"
)

// Client selects a station and its service configuration. The zero value uses
// localhost and discovers service ports from the local quasar.cfg.
type Client struct {
	Host       string
	Port       int    // Override the port of the service being queried.
	ConfigPath string // Override configuration discovery when Port is zero.
}

// SupportsSnapshot reports whether name identifies a readable snapshot.
func SupportsSnapshot(name string) bool {
	_, ok := actions[name]
	return ok
}

// Snapshot reads one named status, using the CLI action names. Each call opens
// its own connection. Without a context deadline the timeout is five seconds.
func (c Client) Snapshot(ctx context.Context, name string) (any, error) {
	if !SupportsSnapshot(name) {
		return nil, fmt.Errorf("unknown snapshot %q", name)
	}
	if c.Port < 0 || c.Port > 65535 {
		return nil, fmt.Errorf("invalid port %d", c.Port)
	}
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
	}
	return readActionBus(ctx, name, c.Host, c.Port, c.ConfigPath)
}

type serviceConn struct {
	net.Conn
	stop func() bool
}

func (c *serviceConn) Close() error {
	c.stop()
	return c.Conn.Close()
}

func dialService(ctx context.Context, addr string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetDeadline(deadline); err != nil {
			conn.Close()
			return nil, err
		}
	}
	return &serviceConn{conn, context.AfterFunc(ctx, func() { conn.Close() })}, nil
}

func snapshotAs[T any](ctx context.Context, c Client, name string) (T, error) {
	var zero T
	v, err := c.Snapshot(ctx, name)
	if err != nil {
		return zero, err
	}
	result, ok := v.(T)
	if !ok {
		return zero, fmt.Errorf("%s: unexpected snapshot type %T", name, v)
	}
	return result, nil
}

// AliceState reads the alice-state snapshot.
func (c Client) AliceState(ctx context.Context) (AliceState, error) {
	return snapshotAs[AliceState](ctx, c, "alice-state")
}

// WiFiStatus reads the wifi-status snapshot.
func (c Client) WiFiStatus(ctx context.Context) (WiFiStatus, error) {
	return snapshotAs[WiFiStatus](ctx, c, "wifi-status")
}

// WiFiList reads the wifi-list snapshot.
func (c Client) WiFiList(ctx context.Context) (WiFiList, error) {
	return snapshotAs[WiFiList](ctx, c, "wifi-list")
}

// SetupStatus reads the setup-status snapshot.
func (c Client) SetupStatus(ctx context.Context) (StateCode, error) {
	return snapshotAs[StateCode](ctx, c, "setup-status")
}

// BrickStatus reads the brick-status snapshot.
func (c Client) BrickStatus(ctx context.Context) (StateCode, error) {
	return snapshotAs[StateCode](ctx, c, "brick-status")
}

// GroupStatus reads the group-status snapshot.
func (c Client) GroupStatus(ctx context.Context) (GroupStatus, error) {
	return snapshotAs[GroupStatus](ctx, c, "group-status")
}

// NTPStatus reads the ntp-status snapshot.
func (c Client) NTPStatus(ctx context.Context) (NTPStatus, error) {
	return snapshotAs[NTPStatus](ctx, c, "ntp-status")
}

// MetricaUUID reads the metrica-uuid snapshot.
func (c Client) MetricaUUID(ctx context.Context) (MetricaUUID, error) {
	return snapshotAs[MetricaUUID](ctx, c, "metrica-uuid")
}

// UpdateStatus reads the update-status snapshot.
func (c Client) UpdateStatus(ctx context.Context) (UpdateStatus, error) {
	return snapshotAs[UpdateStatus](ctx, c, "update-status")
}

// NotificationStatus reads the notification-status snapshot.
func (c Client) NotificationStatus(ctx context.Context) (NotificationStatus, error) {
	return snapshotAs[NotificationStatus](ctx, c, "notification-status")
}

// BluetoothStatus reads the bluetooth-status snapshot.
func (c Client) BluetoothStatus(ctx context.Context) (*BluetoothStatus, error) {
	return snapshotAs[*BluetoothStatus](ctx, c, "bluetooth-status")
}

// AlarmStatus reads the alarm-status snapshot.
func (c Client) AlarmStatus(ctx context.Context) (AlarmStatus, error) {
	return snapshotAs[AlarmStatus](ctx, c, "alarm-status")
}
