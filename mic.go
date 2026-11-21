package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/skrashevich/quasarc/station"
)

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
	var result station.MicStatus
	var err error
	if args[0] == "get" {
		result, err = station.Microphone()
	} else {
		result, err = station.SetMicrophoneMuted(args[0] == "mute")
	}
	if err == nil {
		err = json.NewEncoder(out).Encode(result)
	}
	if err != nil {
		fmt.Fprintln(errOut, "mic:", err)
		return 1
	}
	return 0
}
