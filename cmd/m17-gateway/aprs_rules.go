package main

import (
	"errors"
	"fmt"
	"strings"

	fap "github.com/hessu/go-aprs-fap"
)

// The rules for what the APRS gate passes in each direction. They follow
// https://www.aprs-is.net/IGateDetails.aspx, and are kept free of state and
// I/O so that they can be tested on their own; aprs.go holds the state.

// tnc2 is an APRS packet in TNC2 monitor format, "SRC>DST,PATH:info".
type tnc2 struct {
	src  string
	dst  string
	path []string
	info string
}

var errNotTNC2 = errors.New("not a TNC2 line")

// parseTNC2 splits a TNC2 line into its parts. It checks only the shape:
// what an address may hold depends on which side it is going to.
func parseTNC2(line string) (tnc2, error) {
	var t tnc2
	if strings.ContainsAny(line, "\r\n\x00") {
		return t, fmt.Errorf("%w: control character", errNotTNC2)
	}
	header, info, ok := strings.Cut(line, ":")
	if !ok || info == "" {
		return t, fmt.Errorf("%w: no info field", errNotTNC2)
	}
	src, rest, ok := strings.Cut(header, ">")
	if !ok || src == "" || rest == "" {
		return t, fmt.Errorf("%w: no source or destination", errNotTNC2)
	}
	parts := strings.Split(rest, ",")
	for _, p := range parts {
		if p == "" || p == "*" {
			return t, fmt.Errorf("%w: empty address", errNotTNC2)
		}
	}
	t.src = src
	t.dst = parts[0]
	t.path = parts[1:]
	t.info = info
	return t, nil
}

// String formats t as a TNC2 line.
func (t tnc2) String() string {
	var b strings.Builder
	b.WriteString(t.src)
	b.WriteByte('>')
	b.WriteString(t.dst)
	for _, p := range t.path {
		b.WriteByte(',')
		b.WriteString(p)
	}
	b.WriteByte(':')
	b.WriteString(t.info)
	return b.String()
}

// key identifies a packet for duplicate checks: source, destination and
// info, but not the path, which differs by the route a copy took. This is
// how APRS-IS servers compare packets.
func (t tnc2) key() string {
	return t.src + ">" + t.dst + ":" + t.info
}

// hasPathElement reports whether t's path holds any of calls, used or not.
func (t tnc2) hasPathElement(calls ...string) bool {
	for _, p := range t.path {
		p = strings.TrimSuffix(p, "*")
		for _, c := range calls {
			if strings.EqualFold(p, c) {
				return true
			}
		}
	}
	return false
}

// isAX25Call reports whether call can be an AX.25 address exactly as
// written: one to six capitals or digits and an optional SSID 0-15. Radios
// that turn an M17 APRS line into an AX.25 frame drop a line holding any
// other address.
func isAX25Call(call string) bool {
	return call != "" && fap.CheckAX25Call(call) == call
}

// sameStation reports whether two APRS callsigns are the same station.
func sameStation(a, b string) bool {
	return strings.EqualFold(a, b)
}

// aprsMessage is the part of a message packet the gate uses.
type aprsMessage struct {
	addressee string
	ack       bool   // an ack or a rej
	id        string // the message number, or the number acked or rejected
}

// parseMessage returns the message t carries, if it is one: info
// ":ADDRESSEE:text{id", ":ADDRESSEE:ackid" or ":ADDRESSEE:rejid".
func parseMessage(t tnc2) (aprsMessage, bool) {
	var m aprsMessage
	if len(t.info) < 11 || t.info[0] != ':' || t.info[10] != ':' {
		return m, false
	}
	m.addressee = strings.TrimRight(t.info[1:10], " ")
	if m.addressee == "" {
		return m, false
	}
	text := t.info[11:]
	if strings.HasPrefix(text, "ack") || strings.HasPrefix(text, "rej") {
		m.ack = true
		m.id = strings.TrimRight(text[3:], " ")
		return m, true
	}
	if i := strings.LastIndexByte(text, '{'); i >= 0 {
		// "{MM}AA" is a reply-ack: the number is MM.
		id, _, _ := strings.Cut(text[i+1:], "}")
		m.id = strings.TrimRight(id, " ")
	}
	return m, true
}

// isGroupAddressee reports whether a message addressee is a bulletin,
// announcement or group rather than a station.
func isGroupAddressee(a string) bool {
	u := strings.ToUpper(a)
	for _, p := range []string{"BLN", "NWS-", "SKY", "CWA-", "BOM-"} {
		if strings.HasPrefix(u, p) {
			return true
		}
	}
	switch u {
	case "ALL", "CQ", "QST", "APRS":
		return true
	}
	return false
}

// isPosition reports whether t carries a position, so it can be the
// courtesy position sent with a message.
func isPosition(t tnc2) bool {
	switch t.info[0] {
	case '!', '=', '/', '@', '`', '\'', 0x1c, 0x1d, '$':
	default:
		return false
	}
	p, err := fap.Parse(t.String())
	return err == nil && p.Latitude != nil && p.Type == fap.PacketTypeLocation
}

// Reasons a packet is not gated, for logs and counters.
const (
	dropMalformed   = "malformed"
	dropOwn         = "own packet"
	dropNoGate      = "path forbids gating"
	dropQuery       = "query"
	dropNotAX25     = "address not AX.25"
	dropTooLong     = "too long for M17"
	dropNotForRF    = "addressee not heard on RF"
	dropSenderLocal = "sender heard on RF"
	dropOnAir       = "already heard on RF"
	dropDuplicate   = "duplicate"
	dropUnverified  = "receive-only login"
)

// noGateIS lists path elements that keep a packet off APRS-IS.
var noGateIS = []string{"TCPIP", "TCPXX", "NOGATE", "RFONLY"}

// noGateRF lists path elements that keep an APRS-IS packet off RF. qAX
// marks a packet from an unverified login.
var noGateRF = []string{"TCPXX", "NOGATE", "RFONLY", "qAX"}

// rfToIS decides whether a line heard on RF goes to APRS-IS and returns
// it as sent there, with the q construct and gate callsign appended. A
// third-party packet is unwrapped, layer by layer, and its inner packet
// gated instead.
func rfToIS(t tnc2, gate, qConstruct string) (string, string) {
	for {
		if sameStation(t.src, gate) {
			return "", dropOwn
		}
		if t.hasPathElement(noGateIS...) {
			return "", dropNoGate
		}
		if t.info[0] == '?' {
			return "", dropQuery
		}
		if t.info[0] != '}' {
			break
		}
		inner, err := parseTNC2(t.info[1:])
		if err != nil {
			return "", dropMalformed
		}
		t = inner
	}
	out := t
	out.path = append(append([]string(nil), t.path...), qConstruct, gate)
	return out.String(), ""
}

// rfMaxPayload is the most an M17 packet holds after its type byte, NUL
// terminator and CRC: 33 frames of 25 bytes.
const rfMaxPayload = 33*25 - 4

// ax25MaxFrame is the longest AX.25 frame an OpenRTX radio accepts
// (APRS_PACLEN), without its FCS.
const ax25MaxFrame = 330

// isToRFLine returns the line transmitted for an APRS-IS packet: its IS
// path replaced by TCPIP and the gate, "SRC>DST,TCPIP,GATE*:info". That is
// the inner packet of the third-party form RF iGates send, so that RF
// gates that hear it again don't send it back to APRS-IS. When the source
// or destination can't be an AX.25 address the line is skipped, or with
// wrap, sent in third-party form under the gate's own callsign.
func isToRFLine(t tnc2, gate, tocall string, wrap bool) (string, string) {
	out := tnc2{src: t.src, dst: t.dst, path: []string{"TCPIP", gate + "*"}, info: t.info}
	if !isAX25Call(t.src) || !isAX25Call(t.dst) {
		if !wrap {
			return "", dropNotAX25
		}
		out = tnc2{src: gate, dst: tocall, info: "}" + out.String()}
	}
	line := out.String()
	// Seven bytes per address, and two for control and PID.
	frame := (2+len(out.path))*7 + 2 + len(out.info)
	if len(line) > rfMaxPayload || frame > ax25MaxFrame {
		return "", dropTooLong
	}
	return line, ""
}
