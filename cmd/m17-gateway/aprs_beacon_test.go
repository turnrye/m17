package main

import (
	"strings"
	"testing"
)

func TestPHG(t *testing.T) {
	for _, c := range []struct {
		dbm        float32
		haat, gain float64
		want       string
	}{
		{10, 30, 3, "PHG0330"},  // a 10 mW hotspot: 0 W
		{37, 100, 6, "PHG2560"}, // 5 W: 2² W, 328 ft: 10·2⁵ ft
		{50, 2000, 12, "PHG9990"},
		{10, 1, 0, "PHG0000"},
	} {
		if got := phg(c.dbm, c.haat, c.gain); got != c.want {
			t.Errorf("phg(%v, %v, %v) = %s, want %s", c.dbm, c.haat, c.gain, got, c.want)
		}
	}
}

func TestBeaconComment(t *testing.T) {
	c := aprsConfig{haat: 30, gain: 3, comment: "M17 hotspot"}
	simplex := radioInfo{441112500, 441112500, 10}
	if got := beaconComment(c, simplex, "M17-KCW B"); got != "PHG0330/441.112MHz M17 CAN0 M17-KCW B M17 hotspot" {
		t.Errorf("simplex: %q", got)
	}
	c = aprsConfig{can: 3}
	if got := beaconComment(c, radioInfo{446500000, 441500000, 30}, ""); got != "441.500MHz +500 M17 CAN3" {
		t.Errorf("duplex: %q", got)
	}
	if got := beaconComment(c, radioInfo{439400000, 448400000, 30}, ""); got != "448.400MHz -900 M17 CAN3" {
		t.Errorf("duplex down: %q", got)
	}
}

func TestBeaconLine(t *testing.T) {
	c := aprsConfig{callsign: "K0RET-10", symbol: "M&", haat: 30, gain: 3}
	r := radioInfo{441112500, 441112500, 10}
	if line, err := beaconLine(c, r, ""); line != "" || err != nil {
		t.Errorf("without a position: %q, %v", line, err)
	}
	c.hasPosition, c.latitude, c.longitude = true, 35.1395, -90.0175
	line, err := beaconLine(c, r, "M17-KCW B")
	want := "K0RET-10>APZ001,TCPIP*:!3508.37NM09001.05W&PHG0330/441.112MHz M17 CAN0 M17-KCW B"
	if err != nil || line != want {
		t.Errorf("beacon %q, %v\nwant   %q", line, err, want)
	}
	c.ambiguity = 2
	line, _ = beaconLine(c, r, "")
	if !strings.Contains(line, "!3508.  NM09001.  W&") {
		t.Errorf("ambiguous beacon %q", line)
	}
}
