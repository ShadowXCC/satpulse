package satellitescmd

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/jclark/satpulse/gps/gpsprot"
	"github.com/jclark/satpulse/gps/lib/opt"
)

// writeHuman prints an aligned summary line followed by a per-satellite table.
func writeHuman(out io.Writer, sats *gpsprot.SatellitesMsg, nav *gpsprot.NavEpochMsg) error {
	// The receiver's own counts are preferred, but many (e.g. NMEA, u-blox
	// NAV-SAT) report only "used", so fall back to counts derived from the
	// satellite list: in view = satellites reported, tracked = those with a
	// live signal (CN0 > 0).
	usedFallback, inViewFallback, trackedFallback := -1, -1, -1
	if sats != nil {
		usedFallback = countUsed(sats)
		inViewFallback = len(sats.SVs)
		trackedFallback = countTracked(sats.SVs)
	}
	if nav != nil {
		fmt.Fprintf(out, "Fix: %s %s  SVs used/view/tracked: %s/%s/%s\n",
			nav.FixLevel, nav.SolutionDim,
			svCount(nav.NumSVUsed, usedFallback),
			svCount(nav.NumSVInView, inViewFallback),
			svCount(nav.NumSVTracked, trackedFallback))
		fmt.Fprintf(out, "DOP: P=%s H=%s V=%s T=%s G=%s\n",
			optDOP(nav.DOP.Pos), optDOP(nav.DOP.Hor), optDOP(nav.DOP.Vert),
			optDOP(nav.DOP.Time), optDOP(nav.DOP.Geom))
	}
	if sats == nil || len(sats.SVs) == 0 {
		return nil
	}
	svs := make([]gpsprot.SVInfo, len(sats.SVs))
	copy(svs, sats.SVs)
	sort.Slice(svs, func(i, j int) bool { return svs[i].ID.String() < svs[j].ID.String() })
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SV\tElev\tAzim\tCN0\tUsed")
	for _, sv := range svs {
		elev, azim := "-", "-"
		if sv.LookAngles.IsSet() {
			la := sv.LookAngles.Get()
			elev = fmt.Sprintf("%d", la.Elevation)
			azim = fmt.Sprintf("%d", la.Azimuth)
		}
		used := ""
		if svUsed(sv, sats.UsedValidity) {
			used = "*"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", sv.ID, elev, azim, signalCN0s(sv.Signals), used)
	}
	return tw.Flush()
}

// svUsed reports whether a satellite contributes to the solution, honoring
// where the protocol records the "used" flag (per-SV or per-signal).
func svUsed(sv gpsprot.SVInfo, validity gpsprot.SatelliteUsedValidity) bool {
	if validity == gpsprot.SatelliteUsedSignal {
		for _, sig := range sv.Signals {
			if sig.Used {
				return true
			}
		}
		return false
	}
	return sv.Used
}

// countUsed returns the number of satellites used in the solution, or -1 if
// the protocol doesn't record usage.
func countUsed(sats *gpsprot.SatellitesMsg) int {
	if sats.UsedValidity == gpsprot.SatelliteUsedInvalid {
		return -1
	}
	n := 0
	for _, sv := range sats.SVs {
		if svUsed(sv, sats.UsedValidity) {
			n++
		}
	}
	return n
}

// countTracked returns the number of satellites with at least one live signal.
func countTracked(svs []gpsprot.SVInfo) int {
	n := 0
	for _, sv := range svs {
		for _, sig := range sv.Signals {
			if sig.CN0 > 0 {
				n++
				break
			}
		}
	}
	return n
}

// signalCN0s formats per-signal CN0 as "L1:44 L5:40"; a used signal is
// marked with a trailing '*'.
func signalCN0s(sigs []gpsprot.SignalInfo) string {
	if len(sigs) == 0 {
		return "-"
	}
	parts := make([]string, len(sigs))
	for i, s := range sigs {
		mark := ""
		if s.Used {
			mark = "*"
		}
		if s.ID == "" {
			// NMEA GSV without a per-signal band: show just the CN0.
			parts[i] = fmt.Sprintf("%d%s", s.CN0, mark)
		} else {
			parts[i] = fmt.Sprintf("%s:%d%s", s.ID, s.CN0, mark)
		}
	}
	return strings.Join(parts, " ")
}

// svCount formats a satellite count, preferring the receiver-reported value
// and otherwise using a derived fallback (negative means unavailable).
func svCount(v opt.Val[uint16], fallback int) string {
	if v.IsSet() {
		return fmt.Sprintf("%d", v.Get())
	}
	if fallback >= 0 {
		return fmt.Sprintf("%d", fallback)
	}
	return "-"
}

func optDOP(v opt.Val[float64]) string {
	if v.IsZero() {
		return "-"
	}
	return fmt.Sprintf("%.1f", v.Get())
}
