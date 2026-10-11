package main

import (
	"encoding/json"
	"log"
	"maps"
	"os"
	"slices"
	"time"

	"gopkg.in/ini.v1"
)

// The status file tells a dashboard how the APRS gate is doing: whether
// it is logged in to APRS-IS and where, the filter in force, the stations
// heard, the operator's rules, the queue and counters. It is rewritten
// every few seconds, whole, by renaming a new copy over it.

// aprsStatus is the status file's content.
type aprsStatus struct {
	Time          string         `json:"time"`
	Enabled       bool           `json:"enabled"`
	Callsign      string         `json:"callsign"`
	Server        string         `json:"server"`
	Connected     bool           `json:"connected"`
	ConnectedTo   string         `json:"connectedTo,omitempty"`
	ConnectedAt   string         `json:"connectedAt,omitempty"`
	LastKeepalive string         `json:"lastKeepalive,omitempty"`
	ServerLine    string         `json:"serverLine,omitempty"`
	Verified      bool           `json:"verified"`
	RFtoIS        bool           `json:"rfToIS"`
	IStoRF        bool           `json:"isToRF"`
	Relay         bool           `json:"relay"`
	Filter        string         `json:"filter"`
	HasPosition   bool           `json:"hasPosition"`
	Latitude      float64        `json:"latitude,omitempty"`
	Longitude     float64        `json:"longitude,omitempty"`
	Heard         []heardStatus  `json:"heard"`
	Watches       []watchStatus  `json:"watches"`
	Queue         map[string]int `json:"queue"`
	Counts        map[string]int `json:"counts"`
	Recent        recentStat     `json:"recent"`
	Beacon        string         `json:"beacon,omitempty"`
	LastBeacon    string         `json:"lastBeacon,omitempty"`
	BeaconPreview string         `json:"beaconPreview,omitempty"`
}

// heardStatus is a station heard on RF.
type heardStatus struct {
	Call      string `json:"call"`
	M17       string `json:"m17"`
	CAN       int    `json:"can"`
	LastHeard string `json:"lastHeard"`
}

// watchStatus is one of the operator's rules.
type watchStatus struct {
	Key    string   `json:"key"`
	Rule   string   `json:"rule"`
	Kind   string   `json:"kind"`
	Call   string   `json:"call,omitempty"`
	Lat    float64  `json:"lat,omitempty"`
	Lon    float64  `json:"lon,omitempty"`
	Km     float64  `json:"km,omitempty"`
	ToRF   bool     `json:"toRF"`
	Until  string   `json:"until,omitempty"`
	Active bool     `json:"active"`
	Recent ruleStat `json:"recent"`
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339)
}

// status gathers the gate's status at now.
func (a *igate) status(now time.Time) aprsStatus {
	totals, rules, recent := a.stats.snapshot(now)
	a.mu.Lock()
	defer a.mu.Unlock()
	c := a.cfg
	s := aprsStatus{
		Time:          stamp(now),
		Enabled:       c.enabled,
		Callsign:      c.callsign,
		Server:        c.server,
		Connected:     a.conn != nil,
		ConnectedTo:   a.server,
		ServerLine:    a.serverLine,
		LastKeepalive: stamp(a.serverSeen),
		Verified:      a.verified,
		RFtoIS:        c.rfToIS,
		IStoRF:        c.isToRF,
		Relay:         c.relay,
		Filter:        a.filter,
		HasPosition:   c.hasPosition,
		Latitude:      c.latitude,
		Longitude:     c.longitude,
		Heard:         []heardStatus{},
		Watches:       []watchStatus{},
		Queue:         a.queue.counts(),
		Counts:        totals,
		Recent:        recent,
		Beacon:        a.beaconLine,
		LastBeacon:    stamp(a.lastBeacon),
	}
	if s.Connected {
		s.ConnectedAt = stamp(a.connectedAt)
	}
	if line, err := beaconLine(c, a.radio, a.linkInfo()); err == nil {
		s.BeaconPreview = line
	}
	for _, call := range slices.Sorted(maps.Keys(a.heard)) {
		h := a.heard[call]
		can := int(h.lsfType[0]&0x07)<<1 | int(h.lsfType[1]>>7)
		s.Heard = append(s.Heard, heardStatus{call, h.m17Call, can, stamp(h.last)})
	}
	for _, w := range c.watches {
		ws := watchStatus{Key: w.key, Rule: w.String(), Call: w.call, ToRF: w.toRF,
			Until: stamp(w.until), Active: w.active(now), Recent: rules[w.String()]}
		switch w.kind {
		case watchCall:
			ws.Kind = "call"
		case watchArea:
			ws.Kind, ws.Lat, ws.Lon, ws.Km = "area", w.lat, w.lon, w.km
		case watchNear:
			ws.Kind, ws.Km = "near", w.km
		}
		s.Watches = append(s.Watches, ws)
	}
	return s
}

// writeStatus writes the status file, if one is set.
func (a *igate) writeStatus() {
	a.mu.Lock()
	path := a.cfg.statusPath
	a.mu.Unlock()
	if path == "" {
		return
	}
	b, err := json.Marshal(a.status(a.now()))
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0644); err != nil {
		log.Printf("[ERROR] APRS: writing %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[ERROR] APRS: renaming %s: %v", tmp, err)
	}
}

// configPoll is how often the configuration file is checked for changes.
var configPoll = 5 * time.Second

// watchConfig applies changes to [APRS] in the configuration file at path
// while the gateway runs, so that a dashboard can change them by saving
// the file. A change is read once the file has been the same for one
// poll, so that a half-written file isn't. A section that doesn't load is
// reported and left unapplied.
func (a *igate) watchConfig(path string) {
	if path == "" {
		return
	}
	type version struct {
		mod  time.Time
		size int64
	}
	stat := func() (version, bool) {
		st, err := os.Stat(path)
		if err != nil {
			return version{}, false
		}
		return version{st.ModTime(), st.Size()}, true
	}
	applied, _ := stat()
	var seen version
	t := time.NewTicker(configPoll)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-a.done:
			return
		}
		v, ok := stat()
		if !ok || v == applied {
			continue
		}
		if v != seen {
			seen = v // changed; wait for it to settle
			continue
		}
		applied = v
		f, err := ini.Load(path)
		if err != nil {
			log.Printf("[ERROR] APRS: not applying %s: %v", path, err)
			continue
		}
		c, err := loadAPRSConfig(f.Section("APRS"))
		if err != nil {
			log.Printf("[ERROR] APRS: not applying %s: %v", path, err)
			continue
		}
		a.reconfigure(c)
	}
}
