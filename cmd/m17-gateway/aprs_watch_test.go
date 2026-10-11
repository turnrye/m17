package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseWatch(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"call k0ret-4", "call K0RET-4 dashboard"},
		{"call K0RET* rf", "call K0RET* rf"},
		{"area 35.15 -90.05 50 rf until 2026-10-11T19:00:00Z", "area 35.15 -90.05 50 rf until 2026-10-11T19:00:00Z"},
		{"near 30 dashboard", "near 30 dashboard"},
		{"AREA 1 2 3 RF", "area 1 2 3 rf"},
	} {
		w, err := parseWatch("Watch1", c.in)
		if err != nil || w.String() != c.want {
			t.Errorf("parseWatch(%q) = %q, %v; want %q", c.in, w.String(), err, c.want)
		}
	}
	for _, bad := range []string{
		"", "call", "call K0RET;7", "area 1 2", "area 91 0 5", "area 1 2 0", "near 600",
		"near x", "call W1AW sometimes", "call W1AW until tomorrow", "beam 1 2 3",
	} {
		if _, err := parseWatch("Watch1", bad); err == nil {
			t.Errorf("parseWatch(%q) succeeded", bad)
		}
	}
}

// TestNearNeedsPosition: near is relative to the gate, so without a
// position it asks the server for nothing.
func TestNearNeedsPosition(t *testing.T) {
	w, _ := parseWatch("Watch1", "near 25 rf")
	if f := w.serverFilter(aprsConfig{}); f != "" {
		t.Errorf("filter %q without a position", f)
	}
	c := aprsConfig{hasPosition: true, latitude: 35.1, longitude: -90.05}
	if f := w.serverFilter(c); f != "r/35.1000/-90.0500/25" {
		t.Errorf("filter %q", f)
	}
}

// TestReconfigure: new settings take effect at once; a new filter is sent.
func TestReconfigure(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	c := a.cfg
	c.isToRF = false
	c.minTxGap = 7 * time.Second
	c.watches = []watchRule{{key: "Watch1", kind: watchCall, call: "W1AW"}}
	a.reconfigure(c)
	if a.cfg.isToRF || a.minTxGap() != 7*time.Second || !strings.HasSuffix(a.filter, "b/W1AW g/W1AW") {
		t.Errorf("cfg %+v filter %q", a.cfg, a.filter)
	}
	a.duplex = true
	c.relay = true
	a.reconfigure(c)
	if a.cfg.relay {
		t.Error("relay on in duplex")
	}
}

// TestWatchConfig: an edit to [APRS] in the file is applied once it
// settles; one that doesn't load is not.
func TestWatchConfig(t *testing.T) {
	old := configPoll
	configPoll = 20 * time.Millisecond
	t.Cleanup(func() { configPoll = old })
	now := time.Now()
	a := testIgate(t, &now)
	path := filepath.Join(t.TempDir(), "gateway.ini")
	write := func(s string) {
		if err := os.WriteFile(path, []byte(s), 0644); err != nil {
			t.Fatal(err)
		}
	}
	base := "[APRS]\nEnabled = true\nCallsign = " + testGate + "\nPasscode = -1\n"
	write(base + "IStoRF = true\n")
	go a.watchConfig(path)
	t.Cleanup(a.close)
	time.Sleep(50 * time.Millisecond)

	write(base + "IStoRF = false\nWatch1 = call W1AW rf\n")
	waitFor(t, func() bool { return a.status(now).Watches != nil && len(a.status(now).Watches) == 1 })
	if a.status(now).IStoRF {
		t.Error("IStoRF still on")
	}
	write(base + "Callsign = not a call\n")
	time.Sleep(100 * time.Millisecond)
	if s := a.status(now); s.Callsign != testGate || len(s.Watches) != 1 {
		t.Errorf("bad edit applied: %+v", s)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestStatusFile: the status file says how the gate is doing.
func TestStatusFile(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	a.cfg.statusPath = filepath.Join(t.TempDir(), "aprs-status.json")
	watching(t, a, "call W1AW rf")
	hearRF(t, a, "K0RET", 5, "K0RET-7>APORT1:>here")
	a.serverNote("# aprsc 2.1.14-g5e22b37 11 Oct 2026 14:38:08 GMT T2RDU 66.226.147.251:14580")
	a.writeStatus()
	b, err := os.ReadFile(a.cfg.statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var s aprsStatus
	if err := json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if s.ConnectedTo != "T2RDU" || !s.Verified || s.Callsign != testGate || s.Counts["rfHeard"] != 1 {
		t.Errorf("status %+v", s)
	}
	if len(s.Heard) != 1 || s.Heard[0].Call != "K0RET-7" || s.Heard[0].M17 != "K0RET" || s.Heard[0].CAN != 5 {
		t.Errorf("heard %+v", s.Heard)
	}
	if len(s.Watches) != 1 || s.Watches[0].Rule != "call W1AW rf" || !s.Watches[0].Active {
		t.Errorf("watches %+v", s.Watches)
	}
	if !strings.Contains(s.Filter, "g/K0RET-10/K0RET-7 b/W1AW g/W1AW") {
		t.Errorf("filter %q", s.Filter)
	}
}

// TestNewestWins: a newer report from a station replaces the one waiting.
func TestNewestWins(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	watching(t, a, "call W1AW rf")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:!4142.35N/07243.81W-")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:!4142.40N/07243.90W-")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>status one")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>status two")
	q := queuedRF(a, now)
	if len(q) != 2 || !strings.HasSuffix(q[0].line, "07243.90W-") || !strings.HasSuffix(q[1].line, "status two") {
		t.Errorf("queued %v", q)
	}
	if _, _, recent := a.stats.snapshot(now); recent.Replaced != 2 {
		t.Errorf("replaced %d, want 2", recent.Replaced)
	}
}

func TestLatestKey(t *testing.T) {
	for line, want := range map[string]string{
		"W1AW>APK004:!4142.35N/07243.81W-":                  "position W1AW",
		"W1AW>APK004:`c51l\x1c>/]\"4)}":                     "position W1AW",
		"W1AW>APK004:>status":                               "status W1AW",
		"W1AW>APK004:_10090556c220s004g005t077":             "weather W1AW",
		"W1AW>APK004:T#005,199,000,255,073,123,01101001":    "telemetry W1AW",
		"W1AW>APK004:;LEADER   *092345z4903.50N/07201.75W>": "object LEADER",
		"W1AW>APK004:)AID #2!4903.50N/07201.75WA":           "item AID #2",
		"W1AW>APK004::K0RET-7  :hi{1":                       "W1AW>APK004::K0RET-7  :hi{1",
	} {
		if got := latestKey(mustTNC2(t, line)); got != want {
			t.Errorf("latestKey(%q) = %q, want %q", line, got, want)
		}
	}
}

// TestRuleStats: the status file says what each rule brought lately, and
// what became of it.
func TestRuleStats(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	watching(t, a, "call W1AW rf", "area 41.7 -72.7 20 dashboard")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:!4142.35N/07243.81W-")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>status")
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST:!4142.00N/07243.00W-") // only the area rule
	hearRF(t, a, "W1AW", 0, "W1AW>APORT1:>here")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>via another gate") // local now
	p, _ := a.queue.next(now)
	a.sentRF(p)
	a.queue.next(now.Add(time.Minute)) // the other one expires
	s := a.status(now.Add(time.Minute))
	call, area := s.Watches[0].Recent, s.Watches[1].Recent
	if call != (ruleStat{Matched: 3, Sent: 1, Dropped: 1, Skipped: 1}) {
		t.Errorf("call rule %+v", call)
	}
	if area != (ruleStat{Matched: 1}) {
		t.Errorf("area rule %+v", area)
	}
	if s.Recent.Sent != 1 || s.Recent.Dropped != 1 || s.Recent.DroppedAddressed != 0 {
		t.Errorf("recent %+v", s.Recent)
	}
	now = now.Add(statWindow + 2*time.Minute)
	if s := a.status(now); s.Watches[0].Recent != (ruleStat{}) {
		t.Errorf("old events still counted: %+v", s.Watches[0].Recent)
	}
}
