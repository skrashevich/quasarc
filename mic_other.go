//go:build !linux

package main

import "errors"

func localMic(string) (micResult, error) {
	return micResult{}, errors.New("run this command on the station")
}
