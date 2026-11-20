//go:build !linux || (!arm64 && !amd64)

package main

import "context"

func writeAndroidBluetoothMode(ctx context.Context, enabled bool) error {
	return requestBluetoothVisibility(ctx, enabled)
}
