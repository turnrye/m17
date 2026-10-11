package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// readAPRSLog returns the events in the APRS traffic log at path.
func readAPRSLog(t *testing.T, path string) []aprsEvent {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var evs []aprsEvent
	s := bufio.NewScanner(f)
	for s.Scan() {
		var ev aprsEvent
		if err := json.Unmarshal(s.Bytes(), &ev); err != nil {
			t.Fatalf("bad log line %q: %v", s.Text(), err)
		}
		evs = append(evs, ev)
	}
	return evs
}

// TestAPRSLogEvents: the log records each line heard or received with
// what the gate did with it, and packets dropped from the queue.
func TestAPRSLogEvents(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	path := filepath.Join(t.TempDir(), "aprs.log")
	var err error
	if a.log, err = openAPRSLog(path); err != nil {
		t.Fatal(err)
	}
	a.cfg.maxQueueAge = time.Second
	a.queue.maxAge = time.Second
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1,NOGATE:>not me")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::N0BODY   :hello{AC}")
	a.queue.next(now.Add(2 * time.Second))
	a.log.close()

	want := []aprsEvent{
		{Dir: aprsFromRF, M17: "K0RET", Line: "K0RET-7>APORT1:>here", Result: "gated", Out: "K0RET-7>APORT1,qAR,K0RET-10:>here"},
		{Dir: aprsFromRF, M17: "K0RET", Line: "K0RET-7>APORT1,NOGATE:>not me", Result: "not gated: " + dropNoGate},
		{Dir: aprsFromIS, Line: "W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}", Result: "queued message"},
		{Dir: aprsFromIS, Line: "W1AW>APK004,TCPIP*,qAC,T2TEST::N0BODY   :hello{AC}", Result: "not sent: " + dropNotForRF},
		{Dir: aprsToRF, Line: "W1AW>APK004,TCPIP,K0RET-10*::K0RET-7  :hello{AB}", Result: "expired after 1s", Class: "message"},
	}
	got := readAPRSLog(t, path)
	if len(got) != len(want) {
		t.Fatalf("%d events, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].Time == "" {
			t.Errorf("event %d has no time", i)
		}
		got[i].Time = ""
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// TestAPRSLogRotates: past aprsLogMax the log moves to .1 and starts again.
func TestAPRSLogRotates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "aprs.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", aprsLogMax-10)+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	l, err := openAPRSLog(path)
	if err != nil {
		t.Fatal(err)
	}
	l.write(time.Now(), aprsEvent{Dir: aprsFromRF, Line: "A>B:c", Result: "gated"})
	l.close()
	if got := readAPRSLog(t, path); len(got) != 1 || got[0].Line != "A>B:c" {
		t.Errorf("new log holds %+v", got)
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != aprsLogMax-9 {
		t.Errorf("old log: %v, %v", st, err)
	}
}
