//go:build linux && (arm64 || amd64)

package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"

	"github.com/AndroidGoLab/binder/binder"
	"github.com/AndroidGoLab/binder/kernelbinder"
	"github.com/AndroidGoLab/binder/logger"
	"github.com/AndroidGoLab/binder/parcel"
)

// These AIDL methods and Parcel headers belong to Android 9 (API 28).
// Later framework versions have different transaction IDs and arguments.
const (
	btManagerInterface = "android.bluetooth.IBluetoothManager"
	btAdapterInterface = "android.bluetooth.IBluetooth"

	checkService           binder.TransactionCode = 2 // IServiceManager
	registerAdapter        binder.TransactionCode = 1 // IBluetoothManager
	unregisterAdapter      binder.TransactionCode = 2
	getScanMode            binder.TransactionCode = 12 // IBluetooth
	setScanMode            binder.TransactionCode = 13
	getDiscoverableTimeout binder.TransactionCode = 14

	scanNone                    int32 = 20
	scanConnectable             int32 = 21
	scanConnectableDiscoverable int32 = 23
)

func writeAndroidBluetoothMode(ctx context.Context, enabled bool) error {
	output, err := exec.CommandContext(ctx, "/system/bin/getprop", "ro.build.version.sdk").Output()
	if err != nil {
		return fmt.Errorf("read Android API level: %w", err)
	}
	if strings.TrimSpace(string(output)) != "28" {
		return requestBluetoothVisibility(ctx, enabled)
	}
	// Binder waits for synchronous system-service replies in the kernel.
	// Bound the caller's wait even when that service stops responding.
	done := make(chan error, 1)
	go func() { done <- setAndroid9ScanMode(ctx, enabled) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Android registers this short-lived observer to return a handle to the
// current adapter. Its callbacks are one-way power notifications.
type bluetoothManagerObserver struct{}

func (bluetoothManagerObserver) Descriptor() string {
	return "android.bluetooth.IBluetoothManagerCallback"
}
func (bluetoothManagerObserver) OnTransaction(context.Context, binder.TransactionCode, *parcel.Parcel) (*parcel.Parcel, error) {
	return nil, nil
}

func android9Parcel(descriptor string) *parcel.Parcel {
	p := parcel.New()
	p.WriteInt32(-1 << 31) // StrictMode.PENALTY_GATHER, followed directly by the interface name on API 28.
	p.WriteString16(descriptor)
	return p
}

func android9Handle(p *parcel.Parcel) (uint32, error) {
	handle, present, err := p.ReadNullableStrongBinderLegacy()
	if err == nil && (!present || handle == 0) {
		err = errors.New("service returned no Binder handle")
	}
	return handle, err
}

func setAndroid9ScanMode(ctx context.Context, enabled bool) (err error) {
	// Keep routine driver shutdown messages out of the CLI's error stream.
	ctx = logger.CtxWithLogger(ctx, logger.FromCtx(ctx).WithLevel(logger.LevelError))
	observer := binder.NewStubBinder(bluetoothManagerObserver{})
	defer runtime.KeepAlive(observer)
	driver, err := kernelbinder.Open(ctx)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, driver.Close(ctx)) }()

	lookup := android9Parcel("android.os.IServiceManager")
	lookup.WriteString16("bluetooth_manager")
	reply, err := driver.Transact(ctx, 0, checkService, 0, lookup)
	if err != nil {
		return fmt.Errorf("look up bluetooth_manager: %w", err)
	}
	manager, err := android9Handle(reply) // The native service manager returns just the object.
	if err != nil {
		return fmt.Errorf("bluetooth_manager: %w", err)
	}
	call := func(handle uint32, code binder.TransactionCode, args *parcel.Parcel) (*parcel.Parcel, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reply, err := driver.Transact(ctx, handle, code, 0, args)
		if err == nil {
			err = binder.ReadStatus(reply)
		}
		return reply, err
	}
	callback := android9Parcel(btManagerInterface)
	listenCtx, stopListening := context.WithCancel(ctx)
	defer stopListening()
	binder.WriteBinderToParcel(listenCtx, callback, observer, driver)
	// API 28 flat_binder_object ends before the newer four-byte stability tag.
	callback = parcel.FromBytesWithObjects(callback.Data()[:callback.Len()-4], callback.Objects())
	reply, err = call(manager, registerAdapter, callback)
	if err != nil {
		return fmt.Errorf("register Bluetooth adapter: %w", err)
	}
	defer func() {
		stopListening()
		_, unregisterErr := call(manager, unregisterAdapter, callback)
		err = errors.Join(err, unregisterErr)
	}()
	adapter, err := android9Handle(reply)
	if err != nil {
		return fmt.Errorf("Bluetooth adapter: %w", err)
	}
	return updateAndroid9Adapter(enabled, func(code binder.TransactionCode, args *parcel.Parcel) (int32, error) {
		r, err := call(adapter, code, args)
		if err != nil {
			return 0, err
		}
		return r.ReadInt32()
	})
}

func updateAndroid9Adapter(enabled bool, integer func(binder.TransactionCode, *parcel.Parcel) (int32, error)) error {
	mode, err := integer(getScanMode, android9Parcel(btAdapterInterface))
	if err != nil {
		return fmt.Errorf("getScanMode: %w", err)
	}
	if mode != scanNone && mode != scanConnectable && mode != scanConnectableDiscoverable {
		return fmt.Errorf("unexpected Bluetooth scan mode %d", mode)
	}
	if (mode != scanNone) == enabled {
		return nil
	}
	timeout, err := integer(getDiscoverableTimeout, android9Parcel(btAdapterInterface))
	if err != nil {
		return fmt.Errorf("getDiscoverableTimeout: %w", err)
	}
	if timeout < 0 {
		return fmt.Errorf("unknown Bluetooth discovery timeout %d", timeout)
	}
	target := scanNone
	if enabled {
		target = scanConnectable
	}
	request := android9Parcel(btAdapterInterface)
	request.WriteInt32(target)
	request.WriteInt32(timeout)
	ok, err := integer(setScanMode, request)
	if err != nil {
		return fmt.Errorf("setScanMode: %w", err)
	}
	if ok != 1 {
		return fmt.Errorf("Bluetooth adapter rejected scan mode %d", target)
	}
	return nil
}
