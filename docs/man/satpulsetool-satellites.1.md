# NAME

satpulsetool-satellites - show satellite info from a tap socket or packet log

# SYNOPSIS

**satpulsetool** [*global options*] **satellites**
(**\-\-socket** *path* \| **\-\-tcp** *host:port* \| *file*\|**\-**)\
&nbsp;&nbsp;&nbsp;&nbsp;[**\-h**\|**\-\-help**] [**\-\-vendor** *name*] [**\-\-human**] [**\-\-timeout** *seconds*]

# DESCRIPTION

The **satpulsetool** **satellites** command reports satellite information for one navigation epoch: the satellite counts (used, in view, tracked), the dilution-of-precision values, and the per-satellite carrier-to-noise density ratio (CN0).

It obtains data by tapping a live packet stream or by reading a recorded JSONL packet log.
A running **satpulsed** owns the GPS serial port, so live data is taken from a socket that **satpulsed** exposes with a **\[\[proxy.socket\]\]** or **\[\[proxy.tcp\]\]** table.
A proxy with **protocol = "NMEA"** is a read-only NMEA tap; a proxy with no protocol taps the full stream, including binary protocols.

The command reads until one complete navigation epoch has been decoded, prints the result, and exits.
By default it prints JSON with **satellites** and **navEpoch** objects.
With **\-\-human** it prints an aligned summary line and a per-satellite table instead.

Exactly one of **\-\-socket**, **\-\-tcp**, or a *file* argument must be given.
Use **\-** as *file* to read a packet log from standard input.

## DOP and protocols

NMEA reports only PDOP, HDOP, and VDOP (from the GSA sentence).
TDOP and GDOP are available only from a binary protocol, such as the u-blox UBX-NAV-DOP message.
To obtain TDOP and GDOP, tap the full (unfiltered) stream and select the receiver with **\-\-vendor**.

# OPTIONS

**\-h**, **\-\-help**
: Show usage help for the **satellites** command.

**\-\-socket** *path*
: Tap a Unix-domain socket exposed by a **satpulsed** proxy.

**\-\-tcp** *host:port*
: Tap a TCP endpoint exposed by a **satpulsed** proxy.

**\-\-vendor** *name*
: Restrict packet formats to those used by a receiver vendor, which is required to decode binary protocols.
The value is case-insensitive.
Typical values are **u\-blox**, **Unicore**, **NovAtel**, **Bynav**, **SinoGNSS**, **Allystar**, **Techtotop**, and **Zhongke**.
If this option is omitted, all supported packet formats are recognized, which is sufficient for NMEA.

**\-\-human**
: Print an aligned table instead of JSON.

**\-\-timeout** *seconds*
: Give up after this many seconds if no complete navigation epoch arrives.
The default is 5 seconds.
A value of 0 waits indefinitely.
This option has no effect when reading a packet log.

# EXAMPLES

Configure a read-only NMEA tap socket in **satpulse.toml**:

    [[proxy.socket]]
    path = "/run/satpulse/nmea.sock"
    protocol = "NMEA"

Show satellite info as JSON from that tap:

    satpulsetool satellites --socket /run/satpulse/nmea.sock

Show an aligned table instead:

    satpulsetool satellites --socket /run/satpulse/nmea.sock --human

Tap a full TCP stream and decode u-blox binary messages to include TDOP and GDOP:

    satpulsetool satellites --tcp localhost:2947 --vendor u-blox

Read satellite info from a recorded packet log:

    satpulsetool satellites capture.jsonl

# SEE ALSO

**satpulsetool(1)**, **satpulsetool-scan(1)**, **satpulsetool-gps(1)**, **satpulse.toml(5)**, **satpulsed(8)**
