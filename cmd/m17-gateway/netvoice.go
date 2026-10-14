package main

import (
	"bytes"
	"log"
	"time"

	"github.com/jancona/m17"
)

// netStreamTimeout ends a reflector stream that stops without a last frame.
const netStreamTimeout = time.Second

// netStream is the reflector voice stream being relayed over RF, and how its
// frames arrived. Shown per stream on the dashboard, the counts tell a station
// with a bad connection, whose streams alone suffer, from a bad connection
// here, where every station's do.
type netStream struct {
	id     uint16
	lsf    m17.LSF   // as received from the reflector, for the dashboard
	lastFN uint16    // last frame number received, without the end bit
	lastRX time.Time // when that frame came

	// For the dashboard
	frames    int           // frames received
	lost      int           // frames missing from the sequence received
	silent    int           // silence frames received between voice frames
	silentRun int           // silence frames received since the last voice frame
	voice     bool          // a voice frame has been received
	maxGap    time.Duration // longest wait between frames received
}

// received counts a frame from the reflector, arriving at now.
func (s *netStream) received(sd m17.StreamDatagram, now time.Time) {
	fn := sd.FrameNumber & 0x7fff
	if s.frames > 0 {
		s.maxGap = max(s.maxGap, now.Sub(s.lastRX))
		// A step forward of more than one means frames were lost on the
		// way. Zero is the last frame repeated with its end bit, and a step
		// back is a sender that wraps the frame number early.
		if d := (fn - s.lastFN) & 0x7fff; d > 1 && d < 0x4000 {
			s.lost += int(d - 1)
		}
	}
	s.frames++
	s.lastFN = fn
	s.lastRX = now
	// Silence between voice frames is often the sender covering for audio
	// it lost itself. Silence before the first voice frame or after the
	// last is not counted.
	if bytes.Equal(sd.Payload[:], codec2silence3200) {
		s.silentRun++
		return
	}
	if s.voice {
		s.silent += s.silentRun
	}
	s.silentRun = 0
	s.voice = true
}

// netHeld reports whether reflector voice must wait: a local radio is
// transmitting, or the gateway is sending something of its own.
func (g *Gateway) netHeld() bool {
	return g.localRFActive() || g.ownTX.Load() > 0
}

// armNetTimer sets the stream's timer for when it times out. Callers hold
// netMu.
func (g *Gateway) armNetTimer(s *netStream) {
	next := s.lastRX.Add(netStreamTimeout)
	if g.netTimer == nil {
		g.netTimer = time.AfterFunc(time.Until(next), g.netTick)
	} else {
		g.netTimer.Reset(time.Until(next))
	}
}

// netTick runs when the reflector stream's timer fires, and ends a stream
// that has stopped.
func (g *Gateway) netTick() {
	g.netMu.Lock()
	s := g.net
	if s == nil {
		g.netMu.Unlock()
		return
	}
	if time.Since(s.lastRX) < netStreamTimeout {
		g.armNetTimer(s)
		g.netMu.Unlock()
		return
	}
	log.Printf("[DEBUG] Timed out Internet voice stream %04x", s.id)
	g.endNetStream(s)
	g.netMu.Unlock()
	if g.getState() == Echo {
		g.echoStreamEnd()
	}
}

// endNetStream logs the end of a reflector stream, with how it arrived.
// Callers hold netMu.
func (g *Gateway) endNetStream(s *netStream) {
	log.Printf("[DEBUG] Internet voice stream %04x from %s: %d frames, %d lost, %d silent, longest gap %v",
		s.id, s.lsf.Src.Callsign(), s.frames, s.lost, s.silent, s.maxGap.Round(time.Millisecond))
	g.dashLog.LogFrame(&s.lsf, "Internet", "Voice End",
		"frames", s.frames, "lost", s.lost, "silent", s.silent,
		"maxGapMs", s.maxGap.Milliseconds())
	g.net = nil
}
