package station

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"

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
	return loadConfig(path, parseCfg)
}

func loadConfig(path string, readPorts func(string) (map[string]int, error)) (string, map[string]int, error) {
	if path != "" {
		p, err := readPorts(path)
		if err != nil {
			return "", nil, err
		}
		return path, p, nil
	}
	for _, c := range cfgCandidates() {
		if _, err := os.Stat(c); err != nil {
			continue
		}
		p, err := readPorts(c)
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
	return configPorts(path, data, "")
}

// For speech, follow firmware config symlinks but reject special and oversized
// files. A malformed aliced port must not silently select the default service.
func readSpeechConfig(path string) (map[string]int, error) {
	const limit = 1 << 20
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > limit {
		return nil, errors.New("expected a regular station configuration of at most 1 MiB")
	}
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, errors.New("station configuration exceeds 1 MiB")
	}
	return configPorts(path, data, "aliced")
}

func configPorts(path string, data []byte, checkedService string) (map[string]int, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	ports := map[string]int{}
	for name, raw := range top {
		var obj map[string]json.RawMessage
		var p int
		err := json.Unmarshal(raw, &obj)
		if field := obj["port"]; err == nil && field != nil {
			err = json.Unmarshal(field, &p)
		}
		if err != nil || p < 0 || p > 65535 {
			if checkedService != "" && name == checkedService {
				return nil, fmt.Errorf("%s: invalid %s port", path, name)
			}
			continue
		}
		if p > 0 {
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
	return BuildFromPorts(ports)
}

// BuildFromPorts identifies the firmware family from a service configuration.
func BuildFromPorts(ports map[string]int) string {
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
