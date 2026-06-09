// Package satellitescmd implements the satellites subcommand of satpulsetool.
// It taps a live packet stream (a proxy socket or TCP port exposed by
// satpulsed) or reads a JSONL packet log, decodes one navigation epoch, and
// reports satellite counts, DOP, and per-satellite CN0.
package satellitescmd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

	"github.com/jclark/satpulse/gps/app/cmd"
	"github.com/jclark/satpulse/gps/app/gpsio"
	"github.com/jclark/satpulse/gps/gpsprot"
	"github.com/jclark/satpulse/gps/gpsreg"
	"github.com/jclark/satpulse/gps/scan"
	"github.com/spf13/pflag"
)

const summary = `[-h|--help] (--socket path | --tcp host:port | file|-) [--vendor name] [--human] [--timeout seconds]`

type flagVars struct {
	socketPath string
	tcpAddr    string
	filePath   string
	vendor     gpsreg.Vendor
	human      bool
	timeout    time.Duration
}

// Cmd implements the satellites subcommand. It connects to a packet source,
// decodes one navigation epoch, and reports the satellite snapshot.
func Cmd(logWriter io.Writer, logLevel slog.Level, progName string, cmdName string, args []string) (usage string, err error) {
	v, usageFunc, err := parseFlags(cmdName, args)
	if v == nil {
		if usageFunc != nil {
			usage = usageFunc(progName)
		}
		return usage, err
	}
	lg := cmd.NewLogger(logWriter, logLevel)
	ctx := context.Background()
	ctx, cancel := cmd.CancelOnSignal(ctx, lg)
	defer cancel()
	out := bufio.NewWriter(os.Stdout)
	err = run(ctx, lg, v, out)
	if flushErr := out.Flush(); err == nil {
		err = flushErr
	}
	return "", err
}

func parseFlags(cmdName string, args []string) (*flagVars, func(string) string, error) {
	help := false
	socketPath := ""
	tcpAddr := ""
	vendorStr := ""
	human := false
	timeout := 5.0
	flags := pflag.NewFlagSet(cmdName, pflag.ContinueOnError)
	flags.BoolVarP(&help, "help", "h", false, "show help")
	flags.StringVar(&socketPath, "socket", "", "tap a Unix-socket `path` exposed by a proxy")
	flags.StringVar(&tcpAddr, "tcp", "", "tap a TCP `host:port` exposed by a proxy")
	flags.StringVar(&vendorStr, "vendor", "", "GPS receiver `vendor` name (needed for binary protocols, e.g. u-blox)")
	flags.BoolVar(&human, "human", false, "print an aligned table instead of JSON")
	flags.Float64Var(&timeout, "timeout", timeout, "give up after this many `seconds` with no complete epoch (0 waits indefinitely)")
	usageFunc := cmd.UsageFunc(cmdName, summary, flags)
	if err := flags.Parse(args); err != nil {
		return nil, usageFunc, err
	}
	if help {
		return nil, usageFunc, nil
	}
	filePath := ""
	if flags.NArg() == 1 {
		filePath = flags.Arg(0)
	} else if flags.NArg() > 1 {
		return nil, usageFunc, fmt.Errorf("expected at most one file argument")
	}
	if n := boolCount(socketPath != "", tcpAddr != "", filePath != ""); n != 1 {
		return nil, usageFunc, fmt.Errorf("specify exactly one of --socket, --tcp, or a file argument")
	}
	if timeout < 0 {
		return nil, usageFunc, fmt.Errorf("--timeout must be >= 0")
	}
	vendor, err := gpsreg.ParseVendor(vendorStr)
	if err != nil {
		return nil, usageFunc, err
	}
	return &flagVars{
		socketPath: socketPath,
		tcpAddr:    tcpAddr,
		filePath:   filePath,
		vendor:     vendor,
		human:      human,
		timeout:    time.Duration(timeout * float64(time.Second)),
	}, usageFunc, nil
}

func boolCount(bs ...bool) int {
	n := 0
	for _, b := range bs {
		if b {
			n++
		}
	}
	return n
}

// snapshot is the JSON output: the most recent satellites view paired with
// the navigation-epoch summary (counts and DOP) that closed the epoch.
type snapshot struct {
	Satellites *gpsprot.SatellitesMsg `json:"satellites,omitempty"`
	NavEpoch   *gpsprot.NavEpochMsg   `json:"navEpoch,omitempty"`
}

// errNoData reports that the source ended before a usable snapshot arrived.
var errNoData = errors.New("no satellite data received before the source closed")

func run(ctx context.Context, lg *slog.Logger, v *flagVars, out *bufio.Writer) error {
	pktProcs := gpsreg.CreatePacketProcessors(v.vendor)
	c := &collector{}
	gpsprot.SetAllMsgHandlers(pktProcs, &gpsprot.GenericHandler{Handle: c.handle})
	var err error
	if v.filePath != "" {
		err = driveLog(ctx, lg, v.filePath, pktProcs, c)
	} else {
		err = driveStream(ctx, lg, v, pktProcs, c)
	}
	if err != nil {
		return err
	}
	if c.sats == nil && c.nav == nil {
		return errNoData
	}
	return c.emit(out, v.human)
}

// collector accumulates messages from the decode pipeline. A SatellitesMsg
// provides the per-satellite view; the following NavEpochMsg closes the epoch
// and supplies counts and DOP. Once both are present the snapshot is complete.
type collector struct {
	sats *gpsprot.SatellitesMsg
	nav  *gpsprot.NavEpochMsg
	done bool
}

func (c *collector) handle(msg gpsprot.Msg, _ time.Time) {
	switch m := msg.(type) {
	case *gpsprot.SatellitesMsg:
		c.sats = m
	case *gpsprot.NavEpochMsg:
		c.nav = m
		if c.sats != nil {
			c.done = true
		}
	}
}

// idleGap matches the replay default: a read gap longer than this flushes any
// epoch buffered by the protocol processors.
const idleGap = 150 * time.Millisecond

func driveLog(ctx context.Context, lg *slog.Logger, path string, pktProcs map[gpsprot.Tag]gpsprot.PacketProcessor, c *collector) error {
	var input io.Reader
	if path == "-" {
		input = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		input = f
	}
	return processLog(ctx, lg, input, pktProcs, c)
}

// processLog drives the decode pipeline from a JSONL packet log.
func processLog(ctx context.Context, lg *slog.Logger, input io.Reader, pktProcs map[gpsprot.Tag]gpsprot.PacketProcessor, c *collector) error {
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 256*1024), 256*1024)
	var tPrev time.Time
	for scanner.Scan() {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var entry gpsio.PacketLogEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			lg.Warn("skipping invalid JSONL line", "err", err)
			continue
		}
		if entry.Out || entry.Tag == "" {
			continue
		}
		pp, ok := pktProcs[entry.Tag]
		if !ok {
			continue
		}
		t := time.Time(entry.T)
		if !tPrev.IsZero() && t.Sub(tPrev) > idleGap {
			idle(pktProcs, tPrev)
		}
		if _, err := pp.ProcessPacket(entry.Data(), t); err != nil {
			lg.Warn("error processing packet", "tag", entry.Tag, "err", err)
		}
		if c.done {
			return nil
		}
		tPrev = t
	}
	if !tPrev.IsZero() {
		idle(pktProcs, tPrev)
	}
	return scanner.Err()
}

func driveStream(ctx context.Context, lg *slog.Logger, v *flagVars, pktProcs map[gpsprot.Tag]gpsprot.PacketProcessor, c *collector) error {
	r, closeFn, err := openStream(v)
	if err != nil {
		return err
	}
	defer closeFn()
	var deadline time.Time
	if v.timeout > 0 {
		deadline = time.Now().Add(v.timeout)
	}
	scanner := scan.New(r, 16, gpsreg.CreatePacketFormats(v.vendor))
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		pkt, err := scanner.Scan()
		switch {
		case pkt.IsInterPacketTimeout():
			idle(pktProcs, pkt.TRead)
		case len(pkt.Data) != 0:
			if pp, ok := pktProcs[pkt.Tag()]; ok {
				if _, perr := pp.ProcessPacket(pkt.Data, pkt.TRead); perr != nil {
					lg.Warn("error processing packet", "tag", pkt.Tag(), "err", perr)
				}
			}
		}
		if c.done {
			return nil
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if !deadline.IsZero() && time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s waiting for a complete navigation epoch", v.timeout)
		}
	}
}

// openStream dials the configured socket or TCP source and returns a reader
// that yields periodic inter-packet timeouts, so the scan loop stays
// responsive to the deadline and to flushing.
func openStream(v *flagVars) (io.Reader, func(), error) {
	if v.socketPath != "" {
		conn, err := gpsio.OpenSocket(v.socketPath)
		if err != nil {
			return nil, nil, err
		}
		return conn, func() { conn.Close() }, nil
	}
	conn, err := net.Dial("tcp", v.tcpAddr)
	if err != nil {
		return nil, nil, err
	}
	return &timeoutReader{conn: conn}, func() { conn.Close() }, nil
}

// timeoutReader sets a short read deadline before each read so that an idle
// TCP source surfaces as an inter-packet timeout rather than blocking.
type timeoutReader struct {
	conn net.Conn
}

func (t *timeoutReader) Read(p []byte) (int, error) {
	t.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	return t.conn.Read(p)
}

func idle(pktProcs map[gpsprot.Tag]gpsprot.PacketProcessor, t time.Time) {
	for _, p := range pktProcs {
		p.Idle(t)
	}
}

func (c *collector) emit(out *bufio.Writer, human bool) error {
	if human {
		return writeHuman(out, c.sats, c.nav)
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(snapshot{Satellites: c.sats, NavEpoch: c.nav})
}
