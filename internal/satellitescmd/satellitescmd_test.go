package satellitescmd

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jclark/satpulse/gps/app/gpsio"
	"github.com/jclark/satpulse/gps/gpsprot"
	"github.com/jclark/satpulse/gps/gpsreg"
	"github.com/jclark/satpulse/gps/lib/nmeamsg"
	"github.com/jclark/satpulse/gps/lib/opt"
)

func TestParseFlags(t *testing.T) {
	tests := []struct {
		name      string
		args      []string
		expect    *flagVars
		expectErr bool
	}{
		{
			name:   "socket source",
			args:   []string{"--socket", "/run/sat.sock"},
			expect: &flagVars{socketPath: "/run/sat.sock", timeout: 5 * time.Second},
		},
		{
			name:   "tcp source with vendor and human",
			args:   []string{"--tcp", "host:2947", "--vendor", "u-blox", "--human"},
			expect: &flagVars{tcpAddr: "host:2947", vendor: mustVendor(t, "u-blox"), human: true, timeout: 5 * time.Second},
		},
		{
			name:   "file source with timeout",
			args:   []string{"--timeout", "2", "capture.jsonl"},
			expect: &flagVars{filePath: "capture.jsonl", timeout: 2 * time.Second},
		},
		{name: "no source", args: []string{}, expectErr: true},
		{name: "two sources", args: []string{"--socket", "/s", "--tcp", "h:1"}, expectErr: true},
		{name: "source and file", args: []string{"--socket", "/s", "f.jsonl"}, expectErr: true},
		{name: "negative timeout", args: []string{"--socket", "/s", "--timeout", "-1"}, expectErr: true},
		{name: "too many files", args: []string{"a", "b"}, expectErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := parseFlags("satellites", tc.args)
			if tc.expectErr {
				if err == nil && got != nil {
					t.Fatalf("expected error, got %+v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.expect) {
				t.Errorf("got  %+v\nwant %+v", got, tc.expect)
			}
		})
	}
}

func TestCollectorHandle(t *testing.T) {
	sats := &gpsprot.SatellitesMsg{}
	nav := &gpsprot.NavEpochMsg{}
	tests := []struct {
		name       string
		msgs       []gpsprot.Msg
		expectDone bool
	}{
		{name: "nav only", msgs: []gpsprot.Msg{nav}, expectDone: false},
		{name: "sats only", msgs: []gpsprot.Msg{sats}, expectDone: false},
		{name: "sats then nav", msgs: []gpsprot.Msg{sats, nav}, expectDone: true},
		{name: "nav then sats then nav", msgs: []gpsprot.Msg{nav, sats, nav}, expectDone: true},
		{name: "nav then sats", msgs: []gpsprot.Msg{nav, sats}, expectDone: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := &collector{}
			for _, m := range tc.msgs {
				c.handle(m, time.Time{})
			}
			if c.done != tc.expectDone {
				t.Errorf("done = %v, want %v", c.done, tc.expectDone)
			}
		})
	}
}

func TestWriteHuman(t *testing.T) {
	sats := &gpsprot.SatellitesMsg{
		SVs: []gpsprot.SVInfo{
			{
				ID:         gpsprot.SVID{GNSS: gpsprot.GPS, Num: 24},
				LookAngles: opt.Make(gpsprot.LookAngles{Azimuth: 200, Elevation: 30}),
				Signals:    []gpsprot.SignalInfo{{ID: "L1", CN0: 47, Used: true}},
				Used:       true,
			},
			{
				ID:      gpsprot.SVID{GNSS: gpsprot.GPS, Num: 12},
				Signals: []gpsprot.SignalInfo{{ID: "L1", CN0: 42}},
			},
			{
				// NMEA-style satellite with no per-signal band label.
				ID:      gpsprot.SVID{GNSS: gpsprot.GLO, Num: 9},
				Signals: []gpsprot.SignalInfo{{ID: "", CN0: 29}},
				Used:    true,
			},
		},
	}
	nav := &gpsprot.NavEpochMsg{
		FixLevel:    gpsprot.FixLevelCode,
		SolutionDim: gpsprot.SolutionDim3D,
		NumSVUsed:   opt.Make[uint16](1),
		NumSVInView: opt.Make[uint16](2),
		DOP: gpsprot.DOP{
			Pos:  opt.Make(2.38),
			Hor:  opt.Make(1.26),
			Vert: opt.Make(2.01),
		},
	}
	var buf bytes.Buffer
	if err := writeHuman(&buf, sats, nav); err != nil {
		t.Fatalf("writeHuman: %v", err)
	}
	got := buf.String()
	// NumSVUsed=1 and NumSVInView=2 are receiver-reported; NumSVTracked is
	// unset so it falls back to the count of SVs with a live signal (3).
	for _, want := range []string{"G12", "G24", "L1:47*", "L1:42", "R09", "P=2.4", "H=1.3", "V=2.0", "T=-", "1/2/3"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q:\n%s", want, got)
		}
	}
	// An unlabeled NMEA signal shows just the CN0, not a "?:" prefix.
	if strings.Contains(got, "?:") {
		t.Errorf("unlabeled signal should not render a %q prefix:\n%s", "?:", got)
	}
	if !strings.Contains(got, "29") {
		t.Errorf("expected bare CN0 %q for unlabeled signal:\n%s", "29", got)
	}
}

func TestWriteHumanDerivedCounts(t *testing.T) {
	// Mimics a u-blox NAV-SAT epoch: no NumSV* counts, per-SV used flags.
	sats := &gpsprot.SatellitesMsg{
		UsedValidity: gpsprot.SatelliteUsedSV,
		SVs: []gpsprot.SVInfo{
			{ID: gpsprot.SVID{GNSS: gpsprot.GPS, Num: 6}, Signals: []gpsprot.SignalInfo{{ID: "L1 C/A", CN0: 38}}, Used: true},
			{ID: gpsprot.SVID{GNSS: gpsprot.GPS, Num: 9}, Signals: []gpsprot.SignalInfo{{ID: "L1 C/A", CN0: 12}}},
			{ID: gpsprot.SVID{GNSS: gpsprot.GPS, Num: 4}, Signals: []gpsprot.SignalInfo{{ID: "L1 C/A", CN0: 0}}},
		},
	}
	nav := &gpsprot.NavEpochMsg{FixLevel: gpsprot.FixLevelCode, SolutionDim: gpsprot.SolutionDim3D}
	var buf bytes.Buffer
	if err := writeHuman(&buf, sats, nav); err != nil {
		t.Fatalf("writeHuman: %v", err)
	}
	// used=1 (one Used SV), view=3 (all reported), tracked=2 (CN0>0).
	if !strings.Contains(buf.String(), "used/view/tracked: 1/3/2") {
		t.Errorf("derived counts wrong:\n%s", buf.String())
	}
}

// satSummary is a deterministic projection of a decoded snapshot, used to
// assert the end-to-end pipeline without comparing every NavEpoch field.
type satSummary struct {
	NumSVs    int
	CN0s      []uint8
	PDOP      float64
	HDOP      float64
	VDOP      float64
	NumSVUsed uint16
}

func TestProcessLogEndToEnd(t *testing.T) {
	gsv := sentence("GPGSV,1,1,03,12,21,318,42,24,30,200,47,32,17,93,37,1")
	svSlots := []string{"12", "24", "32", "", "", "", "", "", "", "", "", ""}
	gsa := sentence("GNGSA,A,3," + strings.Join(svSlots, ",") + ",2.38,1.26,2.01,1")
	gga1 := sentence("GNGGA,083559.00,3957.7995,N,11619.0286,E,1,03,1.26,100.0,M,-8.0,M,,")
	gga2 := sentence("GNGGA,083600.00,3957.7995,N,11619.0286,E,1,03,1.26,100.0,M,-8.0,M,,")
	t1 := time.Unix(1000, 0).UTC()
	t2 := time.Unix(1001, 0).UTC()
	log := makeLog([]logLine{
		{t1, gga1}, {t1, gsv}, {t1, gsa}, // epoch 1
		{t2, gga2}, // epoch 2 boundary flushes epoch 1's NavEpoch
	})

	pktProcs := gpsreg.CreatePacketProcessors(gpsreg.VendorUnknown)
	c := &collector{}
	gpsprot.SetAllMsgHandlers(pktProcs, &gpsprot.GenericHandler{Handle: c.handle})
	if err := processLog(context.Background(), discardLogger(), strings.NewReader(log), pktProcs, c); err != nil {
		t.Fatalf("processLog: %v", err)
	}
	if c.sats == nil {
		t.Fatal("no SatellitesMsg captured")
	}
	if c.nav == nil {
		t.Fatal("no NavEpochMsg captured")
	}

	got := satSummary{NumSVs: len(c.sats.SVs)}
	for _, sv := range c.sats.SVs {
		for _, sig := range sv.Signals {
			got.CN0s = append(got.CN0s, sig.CN0)
		}
	}
	sort.Slice(got.CN0s, func(i, j int) bool { return got.CN0s[i] < got.CN0s[j] })
	got.PDOP = c.nav.DOP.Pos.Get()
	got.HDOP = c.nav.DOP.Hor.Get()
	got.VDOP = c.nav.DOP.Vert.Get()
	got.NumSVUsed = c.nav.NumSVUsed.Get()

	want := satSummary{
		NumSVs:    3,
		CN0s:      []uint8{37, 42, 47},
		PDOP:      2.38,
		HDOP:      1.26,
		VDOP:      2.01,
		NumSVUsed: 3,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestEmitJSONRoundTrip(t *testing.T) {
	c := &collector{
		sats: &gpsprot.SatellitesMsg{
			SVs: []gpsprot.SVInfo{{
				ID:      gpsprot.SVID{GNSS: gpsprot.GPS, Num: 12},
				Signals: []gpsprot.SignalInfo{{ID: "L1", CN0: 42}},
			}},
		},
		nav: &gpsprot.NavEpochMsg{NumSVUsed: opt.Make[uint16](3), DOP: gpsprot.DOP{Pos: opt.Make(2.38)}},
	}
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := c.emit(bw, false); err != nil {
		t.Fatalf("emit: %v", err)
	}
	bw.Flush()
	var got snapshot
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := snapshot{Satellites: c.sats, NavEpoch: c.nav}
	if !reflect.DeepEqual(&got, &want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

type logLine struct {
	t   time.Time
	sen string
}

func makeLog(lines []logLine) string {
	var b strings.Builder
	for _, l := range lines {
		entry := gpsio.PacketLogEntry{
			T:     gpsio.TimeMicro(l.t),
			Tag:   "NMEA",
			Ascii: l.sen,
		}
		data, err := json.Marshal(&entry)
		if err != nil {
			panic(err)
		}
		b.Write(data)
		b.WriteByte('\n')
	}
	return b.String()
}

func sentence(payload string) string {
	return fmt.Sprintf("$%s*%02X\r\n", payload, nmeamsg.Checksum([]byte(payload)))
}

func mustVendor(t *testing.T, name string) gpsreg.Vendor {
	t.Helper()
	v, err := gpsreg.ParseVendor(name)
	if err != nil {
		t.Fatalf("ParseVendor(%q): %v", name, err)
	}
	return v
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
