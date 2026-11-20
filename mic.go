package main

import (
	"encoding/json"
	"fmt"
	"io"
)

type micResult struct {
	Muted         bool `json:"muted"`
	HardwareMuted bool `json:"hardware_muted"`
}

func runMic(args []string, out, errOut io.Writer) int {
	const usage = "usage: quasarc mic get|mute|unmute"
	if len(args) == 1 && (args[0] == "-help" || args[0] == "--help") {
		fmt.Fprintln(out, usage)
		return 0
	}
	if len(args) != 1 || (args[0] != "get" && args[0] != "mute" && args[0] != "unmute") {
		fmt.Fprintln(errOut, usage)
		return 2
	}
	result, err := localMic(args[0])
	if err == nil {
		err = json.NewEncoder(out).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(errOut, "mic:", err)
		return 1
	}
	return 0
}

func maxMicState(f frame) (any, error) {
	ff, err := unpack(f)
	if err != nil {
		return nil, err
	}
	n, ok := ff.number(1)
	if !ok || (n != 0 && n != 1) {
		return nil, fmt.Errorf("invalid audiod MicsState")
	}
	return n == 1, nil
}
