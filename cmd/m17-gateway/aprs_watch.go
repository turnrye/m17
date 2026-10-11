package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	fap "github.com/hessu/go-aprs-fap"
	"gopkg.in/ini.v1"
)

// Watch rules are the operator's own choice of APRS-IS traffic, kept as
// Watch1 to Watch16 in [APRS] so a dashboard can edit them while the
// gateway runs. Each asks the server for some traffic and says what to do
// with it: transmit it on RF, or only show it in the traffic log. Since the
// server can't say which part of a filter selected a packet, the gate
// checks each packet against the rules again on its own side.

// maxWatches is how many Watch keys are read.
const maxWatches = 16

type watchKind int

const (
	watchCall watchKind = iota // traffic from a station, or messages to it
	watchArea                  // traffic within a radius of a point
	watchNear                  // traffic within a radius of the gate
)

// watchRule is one Watch key, such as "call K0RET-4 rf until
// 2026-10-11T19:00:00Z", "area 35.15 -90.05 50 dashboard" or "near 30".
type watchRule struct {
	key   string
	kind  watchKind
	call  string // may end in '*' for every SSID
	lat   float64
	lon   float64
	km    float64
	toRF  bool      // transmit what it brings; otherwise only log it
	until time.Time // zero for no end
}

// parseWatch reads one Watch value.
func parseWatch(key, value string) (watchRule, error) {
	w := watchRule{key: key}
	f := strings.Fields(value)
	if len(f) == 0 {
		return w, fmt.Errorf("%s is empty", key)
	}
	var rest []string
	switch strings.ToLower(f[0]) {
	case "call":
		if len(f) < 2 || !isWatchCall(f[1]) {
			return w, fmt.Errorf("%s: call needs a callsign, such as K0RET-4 or K0RET*", key)
		}
		w.kind, w.call, rest = watchCall, strings.ToUpper(f[1]), f[2:]
	case "area":
		if len(f) < 4 {
			return w, fmt.Errorf("%s: area needs latitude, longitude and km", key)
		}
		var err1, err2, err3 error
		w.lat, err1 = strconv.ParseFloat(f[1], 64)
		w.lon, err2 = strconv.ParseFloat(f[2], 64)
		w.km, err3 = strconv.ParseFloat(f[3], 64)
		if err1 != nil || err2 != nil || err3 != nil || w.lat < -90 || w.lat > 90 || w.lon < -180 || w.lon > 180 {
			return w, fmt.Errorf("%s: area needs latitude, longitude and km as numbers", key)
		}
		w.kind, rest = watchArea, f[4:]
	case "near":
		if len(f) < 2 {
			return w, fmt.Errorf("%s: near needs km", key)
		}
		var err error
		if w.km, err = strconv.ParseFloat(f[1], 64); err != nil {
			return w, fmt.Errorf("%s: near needs km as a number", key)
		}
		w.kind, rest = watchNear, f[2:]
	default:
		return w, fmt.Errorf("%s: %q is not call, area or near", key, f[0])
	}
	if w.kind != watchCall && (w.km <= 0 || w.km > 500) {
		return w, fmt.Errorf("%s: the radius must be from 0 to 500 km", key)
	}
	for i := 0; i < len(rest); i++ {
		switch strings.ToLower(rest[i]) {
		case "rf":
			w.toRF = true
		case "dashboard":
			w.toRF = false
		case "until":
			i++
			if i == len(rest) {
				return w, fmt.Errorf("%s: until needs a time", key)
			}
			t, err := time.Parse(time.RFC3339, rest[i])
			if err != nil {
				return w, fmt.Errorf("%s: until needs an RFC 3339 time: %v", key, err)
			}
			w.until = t
		default:
			return w, fmt.Errorf("%s: unexpected %q", key, rest[i])
		}
	}
	return w, nil
}

// isWatchCall reports whether s can be a watched callsign: an APRS-IS
// callsign, optionally ending in '*'.
func isWatchCall(s string) bool {
	s = strings.TrimSuffix(s, "*")
	if s == "" || len(s) > 9 {
		return false
	}
	for _, c := range s {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

// String formats w as a Watch value.
func (w watchRule) String() string {
	var s string
	switch w.kind {
	case watchCall:
		s = "call " + w.call
	case watchArea:
		s = fmt.Sprintf("area %s %s %s", fmtFloat(w.lat), fmtFloat(w.lon), fmtFloat(w.km))
	case watchNear:
		s = "near " + fmtFloat(w.km)
	}
	if w.toRF {
		s += " rf"
	} else {
		s += " dashboard"
	}
	if !w.until.IsZero() {
		s += " until " + w.until.UTC().Format(time.RFC3339)
	}
	return s
}

func fmtFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// active reports whether w is in force at now.
func (w watchRule) active(now time.Time) bool {
	return w.until.IsZero() || now.Before(w.until)
}

// center returns the point a radius rule is measured from: its own, or the
// gate's for near. ok is false when near has no gate position to use.
func (w watchRule) center(c aprsConfig) (lat, lon float64, ok bool) {
	switch w.kind {
	case watchArea:
		return w.lat, w.lon, true
	case watchNear:
		return c.latitude, c.longitude, c.hasPosition
	}
	return 0, 0, false
}

// serverFilter returns the APRS-IS filter parts that select w's traffic.
func (w watchRule) serverFilter(c aprsConfig) string {
	if w.kind == watchCall {
		return "b/" + w.call + " g/" + w.call
	}
	lat, lon, ok := w.center(c)
	if !ok {
		return ""
	}
	return fmt.Sprintf("r/%.4f/%.4f/%s", lat, lon, fmtFloat(w.km))
}

// matches reports whether t is w's traffic: from the watched station or a
// message to it, or at a position within the radius. pos is the packet's
// position, or its sender's last known one.
func (w watchRule) matches(t tnc2, c aprsConfig, pos *[2]float64) bool {
	if w.kind == watchCall {
		if matchCall(w.call, t.src) {
			return true
		}
		m, ok := parseMessage(t)
		return ok && matchCall(w.call, m.addressee)
	}
	lat, lon, ok := w.center(c)
	return ok && pos != nil && fap.Distance(lat, lon, pos[0], pos[1]) <= w.km
}

// matchCall reports whether call matches pattern, which may end in '*'.
func matchCall(pattern, call string) bool {
	if p, ok := strings.CutSuffix(pattern, "*"); ok {
		return strings.HasPrefix(strings.ToUpper(call), p)
	}
	return strings.EqualFold(pattern, call)
}

// loadWatches reads the Watch keys of s, in order. A bad rule is reported
// and left out, so that a bad edit doesn't take the gate down.
func loadWatches(s *ini.Section) ([]watchRule, []error) {
	var rules []watchRule
	var errs []error
	for i := 1; i <= maxWatches; i++ {
		key := "Watch" + strconv.Itoa(i)
		if !s.HasKey(key) || strings.TrimSpace(s.Key(key).String()) == "" {
			continue
		}
		w, err := parseWatch(key, s.Key(key).String())
		if err != nil {
			errs = append(errs, err)
			continue
		}
		rules = append(rules, w)
	}
	return rules, errs
}

// watchAction is what the operator's rules say to do with a packet.
type watchAction int

const (
	watchNone      watchAction = iota // no rule wants it
	watchDashboard                    // log it, don't transmit it
	watchRF                           // transmit it
)

// watchActionFor decides what the rules in force at now say about t, and
// names the rule that decided. Any rule that transmits wins over one that
// only logs. Traffic that only the raw Filter can have selected is logged.
// Must be called with a.mu held.
func (a *igate) watchActionFor(t tnc2, now time.Time) (watchAction, string) {
	var pos *[2]float64
	if p, ok := packetPosition(t); ok {
		pos = &p
	} else if p, ok := a.lastPosition[strings.ToUpper(t.src)]; ok {
		pos = &p.p
	}
	action, rule := watchNone, ""
	for _, w := range a.cfg.watches {
		if !w.active(now) || !w.matches(t, a.cfg, pos) {
			continue
		}
		if w.toRF {
			return watchRF, w.String()
		}
		if action == watchNone {
			action, rule = watchDashboard, w.String()
		}
	}
	if action == watchNone && a.cfg.filter != "" {
		action, rule = watchDashboard, "Filter"
	}
	return action, rule
}

// packetPosition returns the position t carries, if any.
func packetPosition(t tnc2) ([2]float64, bool) {
	switch t.info[0] {
	case '!', '=', '/', '@', '`', '\'', 0x1c, 0x1d, '$', ';', ')':
	default:
		return [2]float64{}, false
	}
	p, err := fap.Parse(t.String())
	if err != nil || p.Latitude == nil || p.Longitude == nil {
		return [2]float64{}, false
	}
	return [2]float64{*p.Latitude, *p.Longitude}, true
}
