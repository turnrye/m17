package main

import (
	"strings"
	"testing"
)

func mustTNC2(t *testing.T, line string) tnc2 {
	t.Helper()
	p, err := parseTNC2(line)
	if err != nil {
		t.Fatalf("parseTNC2(%q): %v", line, err)
	}
	return p
}

func TestParseTNC2(t *testing.T) {
	p := mustTNC2(t, "K0RET-7>APORT1,WIDE1-1*,qAR,K0RET-10::W1AW     :hi{01}")
	if p.src != "K0RET-7" || p.dst != "APORT1" || len(p.path) != 3 || p.path[0] != "WIDE1-1*" || p.info != ":W1AW     :hi{01}" {
		t.Errorf("parsed %+v", p)
	}
	if got := p.String(); got != "K0RET-7>APORT1,WIDE1-1*,qAR,K0RET-10::W1AW     :hi{01}" {
		t.Errorf("String() = %q", got)
	}
	if p.key() != "K0RET-7>APORT1::W1AW     :hi{01}" {
		t.Errorf("key() = %q", p.key())
	}
	for _, bad := range []string{
		"no colon here",
		"K0RET-7>APORT1:",
		"K0RET-7:no gt",
		">APORT1:no source",
		"K0RET-7>:no destination",
		"K0RET-7>APORT1,,WIDE1-1:empty element",
		"K0RET-7>APORT1:line\r\nbreak",
	} {
		if _, err := parseTNC2(bad); err == nil {
			t.Errorf("parseTNC2(%q) succeeded", bad)
		}
	}
}

func TestRFToIS(t *testing.T) {
	for _, c := range []struct {
		line, want, reason string
	}{
		{"K0RET-7>APORT1:!3508.37N/09001.05W[", "K0RET-7>APORT1,qAR,K0RET-10:!3508.37N/09001.05W[", ""},
		{"K0RET-7>APORT1,WIDE1-1*:>status", "K0RET-7>APORT1,WIDE1-1*,qAR,K0RET-10:>status", ""},
		{"K0RET-7>APORT1,NOGATE:>status", "", dropNoGate},
		{"K0RET-7>APORT1,RFONLY:>status", "", dropNoGate},
		{"K0RET-7>APORT1,TCPIP*:>status", "", dropNoGate},
		{"K0RET-7>APORT1,TCPXX*:>status", "", dropNoGate},
		{"K0RET-7>APORT1:?APRS?", "", dropQuery},
		{"K0RET-10>APORT1:>me", "", dropOwn},
		// Third party: TCPIP inside means it came from APRS-IS.
		{"N0GW>APDW17:}W1AW>APK004,TCPIP,N0GW*::K0RET-7  :hi{AB}", "", dropNoGate},
		{"N0GW>APDW17:}W1AW>APK004,WIDE1-1*:>status", "W1AW>APK004,WIDE1-1*,qAR,K0RET-10:>status", ""},
		{"N0GW>APDW17:}N1GW>APDW17:}W1AW>APK004:>nested", "W1AW>APK004,qAR,K0RET-10:>nested", ""},
		{"N0GW>APDW17:}W1AW>APK004,TCPIP*:}X>Y:>z", "", dropNoGate},
		{"N0GW>APDW17:}garbage", "", dropMalformed},
	} {
		got, reason := rfToIS(mustTNC2(t, c.line), "K0RET-10", "qAR")
		if got != c.want || reason != c.reason {
			t.Errorf("rfToIS(%q) = %q, %q; want %q, %q", c.line, got, reason, c.want, c.reason)
		}
	}
}

func TestIsToRFLine(t *testing.T) {
	for _, c := range []struct {
		line   string
		wrap   bool
		want   string
		reason string
	}{
		{"W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hi{AB}", false,
			"W1AW>APK004,TCPIP,K0RET-10*::K0RET-7  :hi{AB}", ""},
		// The radio can't take these as AX.25 addresses.
		{"WINLINK>APWL2K,TCPIP*,qAC,T2TEST::K0RET-7  :mail", false, "", dropNotAX25},
		{"KD7ZYN-S>APDPRS,TCPIP*,qAC,T2TEST::K0RET-7  :hi", false, "", dropNotAX25},
		{"KD7ZYN-S>APDPRS,TCPIP*,qAC,T2TEST::K0RET-7  :hi", true,
			"K0RET-10>APZ001:}KD7ZYN-S>APDPRS,TCPIP,K0RET-10*::K0RET-7  :hi", ""},
		{"W1AW>APK004,TCPIP*,qAC,T2TEST:>" + strings.Repeat("x", 300), false, "", dropTooLong},
	} {
		got, reason := isToRFLine(mustTNC2(t, c.line), "K0RET-10", "APZ001", c.wrap)
		if got != c.want || reason != c.reason {
			t.Errorf("isToRFLine(%q, %v) = %q, %q; want %q, %q", c.line, c.wrap, got, reason, c.want, c.reason)
		}
	}
}

func TestIsAX25Call(t *testing.T) {
	for call, want := range map[string]bool{
		"K0RET": true, "K0RET-7": true, "K0RET-15": true, "N0CALL-10": true,
		"K0RET-16": false, "WINLINK": false, "KD7ZYN-S": false, "k0ret": false, "K0RET-": false, "": false,
	} {
		if got := isAX25Call(call); got != want {
			t.Errorf("isAX25Call(%q) = %v", call, got)
		}
	}
}

func TestParseMessage(t *testing.T) {
	for _, c := range []struct {
		info string
		want aprsMessage
		ok   bool
	}{
		{":K0RET-7  :hello{01", aprsMessage{addressee: "K0RET-7", id: "01"}, true},
		{":K0RET-7  :hello{01}AB", aprsMessage{addressee: "K0RET-7", id: "01"}, true},
		{":K0RET-7  :hello", aprsMessage{addressee: "K0RET-7"}, true},
		{":W1AW     :ack01", aprsMessage{addressee: "W1AW", ack: true, id: "01"}, true},
		{":W1AW     :rej01", aprsMessage{addressee: "W1AW", ack: true, id: "01"}, true},
		{"!3508.37N/09001.05W[", aprsMessage{}, false},
		{":short", aprsMessage{}, false},
	} {
		got, ok := parseMessage(tnc2{src: "X", dst: "Y", info: c.info})
		if got != c.want || ok != c.ok {
			t.Errorf("parseMessage(%q) = %+v, %v", c.info, got, ok)
		}
	}
}

func TestIsPosition(t *testing.T) {
	for line, want := range map[string]bool{
		"W1AW>APK004:!4142.35N/07243.81W-":    true,
		"W1AW>APK004:=4142.35N/07243.81W-PHG": true,
		"W1AW>APK004::K0RET-7  :hi":           false,
		"W1AW>APK004:>status":                 false,
	} {
		if got := isPosition(mustTNC2(t, line)); got != want {
			t.Errorf("isPosition(%q) = %v", line, got)
		}
	}
}
