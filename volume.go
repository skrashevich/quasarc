package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"

	"github.com/skrashevich/quasarc/station"
)

func runVolume(args []string, out, errOut io.Writer) int {
	const usage = "usage: quasarc volume get | quasarc volume set PERCENT (integer 0..100)"
	var target *int
	switch {
	case len(args) == 1 && (args[0] == "-help" || args[0] == "--help"):
		fmt.Fprintln(out, usage)
		return 0
	case len(args) == 1 && args[0] == "get":
	case len(args) == 2 && args[0] == "set":
		n, err := strconv.Atoi(args[1])
		if err != nil || n < 0 || n > 100 {
			fmt.Fprintln(errOut, usage)
			return 2
		}
		target = &n
	default:
		fmt.Fprintln(errOut, usage)
		return 2
	}
	ctx := context.Background()
	var percent float64
	var err error
	if target == nil {
		percent, err = station.Volume(ctx)
	} else {
		percent, err = station.SetVolume(ctx, *target)
	}
	if err == nil {
		err = json.NewEncoder(out).Encode(struct {
			Percent float64 `json:"percent"`
		}{percent})
	}
	if err != nil {
		fmt.Fprintln(errOut, "volume:", err)
		return 1
	}
	return 0
}
