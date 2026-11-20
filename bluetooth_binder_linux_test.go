//go:build linux && (arm64 || amd64)

package main

import (
	"testing"

	"github.com/AndroidGoLab/binder/binder"
	"github.com/AndroidGoLab/binder/parcel"
)

func TestAndroid9BluetoothKeepsDiscoveryTimeout(t *testing.T) {
	for _, tc := range []struct {
		initial, target int32
		enable, write   bool
	}{
		{scanNone, scanConnectable, true, true},
		{scanConnectable, scanNone, false, true},
		{scanConnectableDiscoverable, scanConnectableDiscoverable, true, false},
	} {
		wrote := false
		err := updateAndroid9Adapter(tc.enable, func(code binder.TransactionCode, p *parcel.Parcel) (int32, error) {
			policy, err := p.ReadInt32()
			name, nameErr := p.ReadString16()
			if err != nil || nameErr != nil || policy != -1<<31 || name != "android.bluetooth.IBluetooth" {
				t.Fatalf("invalid API 28 interface header: %d %q", policy, name)
			}
			switch code {
			case 12: // IBluetooth.getScanMode()
				return tc.initial, nil
			case 14: // IBluetooth.getDiscoverableTimeout()
				return 37, nil
			case 13: // IBluetooth.setScanMode(mode, duration)
				mode, err := p.ReadInt32()
				timeout, timeoutErr := p.ReadInt32()
				if err != nil || timeoutErr != nil || mode != tc.target || timeout != 37 || p.Position() != p.Len() {
					t.Fatalf("setScanMode(%d, %d), want (%d, 37)", mode, timeout, tc.target)
				}
				wrote = true
				return 1, nil
			default:
				t.Fatalf("unexpected transaction %d", code)
				return 0, nil
			}
		})
		if err != nil || wrote != tc.write {
			t.Fatalf("enable=%t from %d: wrote=%t, err=%v", tc.enable, tc.initial, wrote, err)
		}
	}
}
