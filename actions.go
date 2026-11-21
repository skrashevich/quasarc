package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"time"

	"github.com/skrashevich/quasarc/station"
)

func runAction(args []string, out, errOut io.Writer) int {
	switch args[0] {
	case "mic":
		return runMic(args[1:], out, errOut)
	case "volume":
		return runVolume(args[1:], out, errOut)
	}
	if !station.SupportsSnapshot(args[0]) {
		fmt.Fprintln(errOut, "available actions: volume, mic, alice-state, wifi-status, wifi-list, setup-status, ntp-status, brick-status, group-status, metrica-uuid, update-status, notification-status, bluetooth-status, alarm-status")
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
	client := station.Client{Host: *host, Port: *port, ConfigPath: *cfg}
	result, err := client.Snapshot(ctx, args[0])
	if err == nil {
		err = json.NewEncoder(out).Encode(result)
	}
	if err != nil {
		fmt.Fprintf(errOut, "%s: %v\n", args[0], err)
		return 1
	}
	return 0
}
