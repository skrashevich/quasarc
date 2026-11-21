package station

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/skrashevich/quasarc/protocol"
)

const (
	BuildMax   = "max"
	BuildLinux = "linux"
)

func cfgCandidates() []string {
	if v := os.Getenv("QUASAR_CFG"); v != "" {
		return []string{v}
	}
	return []string{
		"/system/vendor/quasar/quasar.cfg",
		"/vendor/quasar/quasar.cfg",
		"./quasar.cfg",
	}
}

// LoadConfig reads service ports from path. An empty path uses QUASAR_CFG,
// the station system paths, then ./quasar.cfg. If discovery finds no file,
// it returns an empty path, nil ports and no error.
func LoadConfig(path string) (string, map[string]int, error) {
	if path != "" {
		p, err := parseCfg(path)
		if err != nil {
			return "", nil, err
		}
		return path, p, nil
	}
	for _, c := range cfgCandidates() {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		p, err := parseCfg(c)
		if err != nil {
			return c, nil, err
		}
		return c, p, nil
	}
	return "", nil, nil
}

// parseCfg reads the "<service>: { ..., \"port\": N, ... }" map. The file is
// JSON, but values contain ${MACRO} placeholders inside strings only, so a
// strict decode of the top level into raw values is enough.
func parseCfg(path string) (map[string]int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ports := map[string]int{}
	for name, raw := range top {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(raw, &obj); err != nil {
			continue
		}
		rm, ok := obj["port"]
		if !ok {
			continue
		}
		var p int
		if err := json.Unmarshal(rm, &p); err != nil {
			continue
		}
		if p > 0 && p < 65536 {
			ports[name] = p
		}
	}
	return ports, nil
}

// DetectBuild selects the diagnostic table from the configured services.
// A dedicated bluetoothd without iohub_clients is characteristic of Station Max.
// If discovery is inconclusive the CLI defaults to the Linux table.
func DetectBuild() string {
	_, ports, _ := LoadConfig("")
	if ports["bluetoothd"] != 0 && ports["iohub_clients"] == 0 {
		return BuildMax
	}
	return BuildLinux
}

// DefaultFraming selects a fallback layout for a silent service on the given build.
func DefaultFraming(build string) protocol.Framing {
	if build == BuildMax {
		return protocol.Direct
	}
	return protocol.Envelope
}
