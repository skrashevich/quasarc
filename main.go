// Command quasarc is a portable client for the Yandex Station Quasar service
// bus. Each service exchanges length-prefixed protobuf messages over TCP.
// Android-based Station Max sends the QuasarMessage oneof field directly.
// Buildroot-based Station 2 and Midi wrap its body and ID in fields 252 and 253.
package main

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/skrashevich/quasarc/protocol"
	"github.com/skrashevich/quasarc/station"
)

func sortedServices(ports map[string]int) []string {
	names := make([]string, 0, len(ports))
	for n := range ports {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if ports[names[i]] == ports[names[j]] {
			return names[i] < names[j]
		}
		return ports[names[i]] < ports[names[j]]
	})
	return names
}

func listServices(ports map[string]int) {
	names := sortedServices(ports)
	for _, n := range names {
		fmt.Printf("%-24s %d\n", n, ports[n])
	}
	fmt.Printf("# %d services with a tcp port\n", len(names))
}

func main() {
	if len(os.Args) > 1 && !strings.HasPrefix(os.Args[1], "-") {
		os.Exit(runAction(os.Args[1:], os.Stdout, os.Stderr))
	}
	var (
		host      = flag.String("h", "127.0.0.1", "host of the quasar bus")
		port      = flag.Int("p", 0, "tcp port (overrides -s)")
		service_  = flag.String("s", "", "service name resolved from quasar.cfg")
		cfgPath   = flag.String("C", "", "quasar.cfg path (default: auto-detect)")
		sendB64   = flag.String("m", "", "message body, base64 (empty body = send nothing)")
		idFlag    = flag.Int("i", -1, "message id (QuasarMessage oneof field number)")
		typeFlag  = flag.String("t", "", "message name, resolved to an id via the built-in map")
		rawMode   = flag.Bool("raw", false, "-m holds exact TCP bytes including any length prefix")
		dur       = flag.Duration("d", 10*time.Second, "read window after subscribing/sending (0 = no limit)")
		count     = flag.Int("n", 0, "stop after N frames (0 = no limit)")
		buildFlag = flag.String("build", "", "id map: "+station.BuildMax+" or "+station.BuildLinux+" (default: auto)")
		jsonOut   = flag.Bool("json", false, "one JSON object per frame")
		noDecode  = flag.Bool("b64", false, "print base64 of the whole frame like the stock client")
		dumpN     = flag.Int("dump", 0, "read up to N raw socket bytes and hexdump them instead of parsing frames")
		listFlag  = flag.Bool("list", false, "list services and ports from quasar.cfg")
		mapFlag   = flag.Bool("map", false, "print the id -> message map")
		probeFlag = flag.Bool("probe", false, "list reachable services, decode messages and query readable snapshots")
		framingF  = flag.String("framing", "auto", "frame layout: auto, env (len+252/253), direct (len+oneof), bare")
		wtFlag    = flag.String("wt", "", "wire type override for sending: 0, 1, 2 or 5")
		verboseF  = flag.Bool("v", false, "report the detected framing on stderr")
	)
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `quasarc - Yandex Station quasar bus client (Station Max / Station 2)

  quasarc alice-state | wifi-status | wifi-list   return one JSON snapshot
  quasarc setup-status | ntp-status | brick-status | group-status
  quasarc metrica-uuid | update-status | notification-status
  quasarc bluetooth-status | alarm-status
  quasarc volume get | volume set PERCENT       station playback level
  quasarc mic get | mic mute | mic unmute       microphone status and RPC availability
  quasarc wifi-list -help           show action options

  quasarc -s aliced                 subscribe to a service, print decoded frames
  quasarc -s aliced -list           list every service and its port
  quasarc -probe                    list reachable services and show RPC data (up to 3s per service)
  quasarc -s aliced -t alice_state -m <base64>   send a message
  quasarc -p 9924 -b64              raw frames only, stock-client style

`)
		flag.PrintDefaults()
	}
	flag.Parse()

	verbose := *verboseF
	fr, ferr := protocol.ParseFraming(*framingF)
	if ferr != nil {
		fmt.Fprintln(os.Stderr, "error:", ferr)
		os.Exit(2)
	}
	table := resolveTable(*buildFlag)
	cfgFile, ports, err := station.LoadConfig(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "config:", err)
		os.Exit(1)
	}

	if *listFlag {
		if cfgFile == "" {
			fmt.Fprintln(os.Stderr, "no quasar.cfg found (use -C)")
			os.Exit(1)
		}
		fmt.Printf("# %s\n", cfgFile)
		listServices(ports)
		return
	}
	if *mapFlag {
		printMap(table)
		return
	}

	if *probeFlag {
		if len(ports) == 0 {
			fmt.Fprintln(os.Stderr, "no services found (use -C)")
			os.Exit(1)
		}
		probe(os.Stdout, *host, ports, table, fr, *dur, verbose)
		return
	}

	p := *port
	if p == 0 && *service_ != "" {
		if ports == nil {
			fmt.Fprintln(os.Stderr, "no quasar.cfg found, use -p")
			os.Exit(1)
		}
		v, ok := ports[*service_]
		if !ok {
			fmt.Fprintf(os.Stderr, "service %q not found in %s\n", *service_, cfgFile)
			os.Exit(1)
		}
		p = v
	}
	if p == 0 {
		fmt.Fprintln(os.Stderr, "no port: use -s <service> or -p <port>")
		flag.Usage()
		os.Exit(2)
	}

	var out *os.File = os.Stdout
	defer func() { _ = out.Sync() }()

	addr := net.JoinHostPort(*host, strconv.Itoa(p))
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		fmt.Fprintf(os.Stderr, "dial %s: %v\n", addr, err)
		os.Exit(1)
	}
	defer conn.Close()
	fmt.Fprintf(os.Stderr, "connected %s\n", addr)

	if *dumpN > 0 {
		if *dur > 0 {
			_ = conn.SetReadDeadline(time.Now().Add(*dur))
		}
		b := make([]byte, *dumpN)
		n, rerr := io.ReadAtLeast(conn, b, *dumpN)
		if n < 0 {
			n = 0
		}
		fmt.Fprintf(os.Stderr, "read %d bytes (%v)\n", n, rerr)
		hexdump(os.Stdout, b[:n])
		return
	}

	link := protocol.NewBus(conn, fr, station.DefaultFraming(table.Build()))

	if *rawMode {
		if *sendB64 == "" {
			fmt.Fprintln(os.Stderr, "-raw needs -m")
			os.Exit(2)
		}
		raw, err := base64.StdEncoding.DecodeString(*sendB64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "-m is not base64:", err)
			os.Exit(2)
		}
		if err := link.WriteRaw(raw); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "sent %d raw bytes\n", len(raw))
	} else if *sendB64 != "" || *idFlag >= 0 || *typeFlag != "" {
		id := *idFlag
		name := ""
		if *typeFlag != "" {
			e, ok := table.ByName(*typeFlag)
			if !ok {
				fmt.Fprintf(os.Stderr, "unknown message %q for build %s (see -map)\n", *typeFlag, table.Build())
				os.Exit(1)
			}
			id = e.ID
			name = e.Name
		}
		if id < 0 {
			fmt.Fprintln(os.Stderr, "need -i <id> or -t <name>")
			os.Exit(2)
		}
		body, err := base64.StdEncoding.DecodeString(*sendB64)
		if err != nil {
			fmt.Fprintln(os.Stderr, "-m is not base64:", err)
			os.Exit(2)
		}
		f := protocol.Frame{ID: uint64(id), Payload: body}
		switch {
		case *wtFlag != "":
			wt, cerr := strconv.ParseUint(*wtFlag, 10, 64)
			if cerr != nil || wt > 5 {
				fmt.Fprintln(os.Stderr, "-wt must be 0, 1, 2 or 5")
				os.Exit(2)
			}
			f.WT = wt
		default:
			if e, ok := table.ByID(id); ok {
				f.WT = e.WireType()
			}
		}
		if verbose && fr == protocol.Auto {
			fmt.Fprintln(os.Stderr, "framing", link.Framing())
			verbose = false
		}
		if err := link.WriteFrame(f); err != nil {
			fmt.Fprintln(os.Stderr, "write:", err)
			os.Exit(1)
		}
		fmt.Fprintf(os.Stderr, "sent id=%d %s (%d bytes, %s framing)\n", id, name, len(body), link.Framing())
	}

	deadline := time.Time{}
	if *dur > 0 {
		deadline = time.Now().Add(*dur)
	}
	link.SetReadDeadline(deadline)
	if verbose && fr == protocol.Auto {
		fmt.Fprintln(os.Stderr, "framing", link.Framing())
	}
	readLoop(link, table, out, *count, *jsonOut, *noDecode)
}
