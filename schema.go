package main

import (
	"fmt"
	"os"

	"github.com/skrashevich/quasarc/protocol"
	"github.com/skrashevich/quasarc/station"
)

func resolveTable(name string) *protocol.Table {
	if name == "" {
		name = station.DetectBuild()
	}
	t, ok := protocol.MessageTable(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "unknown -build %q (want %s or %s)\n", name, station.BuildMax, station.BuildLinux)
		os.Exit(2)
	}
	return t
}

func printMap(t *protocol.Table) {
	entries := t.Messages()
	for _, e := range entries {
		fmt.Printf("%-4d %-32s %s\n", e.ID, e.Name, e.Type)
	}
	fmt.Printf("# build %s: %d ids known\n", t.Build(), len(entries))
}
