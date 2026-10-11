package main

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/m17/inet"
)

// linkGateway is a gateway linked to M17-AAA A, with M17-BBB also in its
// host file, both fake reflectors, that takes link commands from K0RET*.
func linkGateway(t *testing.T, revertAfter time.Duration) (*Gateway, *fakeReflector, *fakeReflector) {
	t.Helper()
	a, b := newFakeReflector(t, true), newFakeReflector(t, true)
	port := func(r *fakeReflector) uint { return uint(r.conn.LocalAddr().(*net.UDPAddr).Port) }
	g := &Gateway{modem: &recordingModem{}, state: Idle, lastStreamID: 0xFFFF,
		callsign: "N1ADJ G", Name: "M17-AAA", Module: "A",
		hostfile: &inet.Hostfile{Hosts: map[string]inet.Host{
			"M17-AAA": {Name: "M17-AAA", Server: "127.0.0.1", Port: port(a)},
			"M17-BBB": {Name: "M17-BBB", Server: "127.0.0.1", Port: port(b)},
		}},
		linkCfg:     linkConfig{rfLinking: true, callsigns: []string{"K0RET*"}, revertAfter: revertAfter},
		packetQueue: make(chan func() error, packetQueueLen),
		stop:        make(chan struct{}),
	}
	cs, err := m17.EncodeCallsign(g.callsign)
	if err != nil {
		t.Fatal(err)
	}
	g.encodedCallsign = *cs
	if err := g.loadAudioClips("audio", g.callsign); err != nil {
		t.Fatal(err)
	}
	go g.sendPackets()
	if err := g.linkTo(g.Name, g.Module, "startup"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(g.stop)
		if c := g.link.Swap(nil); c != nil {
			c.Close()
		}
		close(g.packetQueue)
	})
	return g, a, b
}

// rawCallsign encodes s as a radio does, without the checks EncodeCallsign
// makes, so that "INFO" can be a destination.
func rawCallsign(s string) m17.EncodedCallsign {
	const chars = " ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-/."
	var v uint64
	for i := len(s) - 1; i >= 0; i-- {
		v = v*40 + uint64(strings.IndexByte(chars, s[i]))
	}
	var e m17.EncodedCallsign
	for i := 5; i >= 0; i-- {
		e[i] = byte(v)
		v >>= 8
	}
	return e
}

// keyUp has the gateway receive a short voice transmission from src to dst.
func keyUp(t *testing.T, g *Gateway, src, dst string) {
	t.Helper()
	lsf, err := m17.NewLSF(m17.DestinationAll, src, m17.LSFTypeStream, m17.LSFDataTypeVoice, 0)
	if err != nil {
		t.Fatal(err)
	}
	lsf.Dst = rawCallsign(dst)
	lsf.CalcCRC()
	g.receivedRFLSF(lsf, 0)
	g.receivedRFStreamFrame(lsf, make([]byte, 16), 0x1234, 0, 0)
	g.receivedRFStreamFrame(lsf, make([]byte, 16), 0x1234, 0x8001, 0)
	g.receivedRFStreamEOT(lsf, 0x1234, 1, 0)
}

// linkedTo waits for the gateway to be linked as want ("" for unlinked).
func linkedTo(t *testing.T, g *Gateway, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for linkName(g.reflector()) != want || g.getState() != Idle {
		if time.Now().After(deadline) {
			t.Fatalf("linked to %q, state %v; want %q", linkName(g.reflector()), g.getState(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLinkCommandFor(t *testing.T) {
	g, _, _ := linkGateway(t, time.Hour)
	for dst, want := range map[string]*linkCommand{
		"M17-BBB C": {name: "M17-BBB", module: "C", from: "K0RET-2"},
		"UNLINK":    {unlink: true, from: "K0RET-2"},
		"M17-XYZ C": nil, // not in the host files: an ordinary call
		"M17-BBB":   nil,
		"W1AW":      nil,
	} {
		lsf, err := m17.NewLSF(m17.DestinationAll, "K0RET-2", m17.LSFTypeStream, m17.LSFDataTypeVoice, 0)
		if err != nil {
			t.Fatal(err)
		}
		lsf.Dst = rawCallsign(dst)
		got, ok := g.linkCommandFor(lsf)
		if (want == nil) == ok || (ok && got != *want) {
			t.Errorf("linkCommandFor(%q) = %+v, %v", dst, got, ok)
		}
	}
	g.linkCfg.rfLinking = false
	lsf, _ := m17.NewLSF("M17-BBB C", "K0RET-2", m17.LSFTypeStream, m17.LSFDataTypeVoice, 0)
	if _, ok := g.linkCommandFor(lsf); ok {
		t.Error("link command taken with RFLinking off")
	}
}

func TestLinkAllowed(t *testing.T) {
	g := &Gateway{linkCfg: linkConfig{callsigns: []string{"K0RET*", "N1ADJ"}}}
	for call, want := range map[string]bool{"K0RET-2": true, "K0RET": true, "N1ADJ": true, "N1ADJ-7": false, "W1AW": false} {
		if got := g.linkAllowed(call); got != want {
			t.Errorf("linkAllowed(%q) = %v", call, got)
		}
	}
	if !(&Gateway{}).linkAllowed("W1AW") {
		t.Error("an empty LinkCallsigns refused someone")
	}
}

// TestRFLinking: a transmission to a reflector links there, without going
// to either reflector itself; UNLINK unlinks; strangers are ignored.
func TestRFLinking(t *testing.T) {
	g, a, b := linkGateway(t, time.Hour)
	keyUp(t, g, "W1AW", "M17-BBB C")
	linkedTo(t, g, "M17-AAA A")

	keyUp(t, g, "K0RET-2", "M17-BBB C")
	linkedTo(t, g, "M17-BBB C")
	for _, r := range []*fakeReflector{a, b} {
		if streams, _, _ := r.received(); len(streams) != 0 {
			t.Errorf("the command was sent to a reflector: %v", streams)
		}
	}
	if g.linkedBy != "K0RET-2" {
		t.Errorf("linked by %q", g.linkedBy)
	}

	keyUp(t, g, "K0RET-2", "UNLINK")
	linkedTo(t, g, "")
	keyUp(t, g, "K0RET-2", "INFO")
	linkedTo(t, g, "")
}

// TestRevert: with nothing received on RF for RevertAfter, the gateway
// goes back home; anything received holds it where it is.
func TestRevert(t *testing.T) {
	old := linkPoll
	linkPoll = 20 * time.Millisecond
	t.Cleanup(func() { linkPoll = old })
	g, _, _ := linkGateway(t, 300*time.Millisecond)
	g.linkCfg.statusPath = filepath.Join(t.TempDir(), "link-status.json")
	go g.revertLoop(g.stop)

	keyUp(t, g, "K0RET-2", "M17-BBB C")
	linkedTo(t, g, "M17-BBB C")
	for range 10 { // RF for longer than RevertAfter
		g.rxHeard()
		time.Sleep(60 * time.Millisecond)
	}
	if linkName(g.reflector()) != "M17-BBB C" {
		t.Fatal("went back home while RF was received")
	}
	var s linkStatus
	b, err := os.ReadFile(g.linkCfg.statusPath)
	if err == nil {
		err = json.Unmarshal(b, &s)
	}
	if err != nil || s.Home != "M17-AAA A" || s.Linked != "M17-BBB C" || s.LinkedBy != "K0RET-2" || s.RevertAt == "" {
		t.Errorf("status %+v, %v", s, err)
	}
	linkedTo(t, g, "M17-AAA A")
	if g.linkedBy != "revert" {
		t.Errorf("linked by %q", g.linkedBy)
	}
}
