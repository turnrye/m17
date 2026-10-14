package main

import (
	"bytes"
	"log"
	"time"

	"github.com/jancona/m17"
)

// netStreamTimeout ends a reflector stream that stops without a last frame.
const netStreamTimeout = time.Second

// fillMargin is how long before the modem would run out of frames the gateway
// sends a silence frame instead: time to write one frame to the modem, about
// 21 ms over the CC1200's serial link, plus scheduling.
const fillMargin = 40 * time.Millisecond

// netStream is the reflector voice stream being relayed over RF, and how its
// frames arrived. Shown per stream on the dashboard, the counts tell a station
// with a bad connection, whose streams alone suffer, from a bad connection
// here, where every station's do.
//
// Reflector frames come late, or not at all. The modem sends what it has
// queued, and once that runs out it sends unmodulated carrier, which knocks
// receivers out of frame sync: they lose several hundred milliseconds of audio
// finding it again, however short the gap. So when no frame has come by the
// time the modem would run out, the gateway sends a silence frame, and a late
// frame goes out after it. Each fill leaves a frame more queued in the modem,
// which absorbs the stream's jitter from then on.
type netStream struct {
	id      uint16
	lsf     m17.LSF   // as received from the reflector, for the dashboard
	txLSF   m17.LSF   // as sent over RF, for silence frames
	lastFN  uint16    // last frame number received, without the end bit
	lastRX  time.Time // when that frame came
	sending bool      // frames are going to the modem, not held back
	nextFN  uint16    // next frame number on air
	airEnd  time.Time // when the modem runs out of frames, as counted here

	// For the dashboard
	frames    int           // frames received
	lost      int           // frames missing from the sequence received
	filled    int           // silence frames sent because no frame had come
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

// onAir gives a frame its number on air. A transmission keeps the reflector's
// frame numbers until a frame is lost or filled; from then on they still run
// on one by one, as they must on air.
func (s *netStream) onAir(sd m17.StreamDatagram) m17.StreamDatagram {
	if !s.sending {
		s.nextFN = sd.FrameNumber & 0x7fff
	}
	sd.FrameNumber = s.nextFN
	if sd.LastFrame {
		sd.FrameNumber |= 0x8000
	}
	s.nextFN = (s.nextFN + 1) & 0x7fff
	return sd
}

// sent notes a frame the modem took at now. A new transmission has the
// preamble and LSF queued ahead of its first frame: one of them is counted
// here, and the other left as margin.
func (s *netStream) sent(now time.Time) {
	if !s.sending {
		s.airEnd = now.Add(m17.FrameTime)
	}
	s.airEnd = later(s.airEnd, now).Add(m17.FrameTime)
	s.sending = true
}

func later(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

// netHeld reports whether reflector voice must wait: a local radio is
// transmitting, or the gateway is sending something of its own.
func (g *Gateway) netHeld() bool {
	return g.localRFActive() || g.ownTX.Load() > 0
}

// sendNetFrame transmits a reflector frame, or a silence frame in its place.
// Callers hold netMu.
func (g *Gateway) sendNetFrame(s *netStream, sd m17.StreamDatagram) error {
	err := g.modem.TransmitVoiceStream(s.onAir(sd))
	if err != nil {
		return err
	}
	s.sent(time.Now())
	g.voiceSent()
	return nil
}

// sendSilenceFrame sends a silence frame in the reflector stream, the last
// one if last is set. Callers hold netMu.
func (g *Gateway) sendSilenceFrame(s *netStream, last bool) {
	sd := m17.NewStreamDatagram(s.id, 0, &s.txLSF, codec2silence3200)
	sd.LastFrame = last
	if err := g.sendNetFrame(s, sd); err != nil {
		log.Printf("[ERROR] Error transmitting silence in Internet voice stream %04x: %v", s.id, err)
	}
}

// armNetTimer sets the stream's timer for whichever comes first: the modem
// running out of frames, or the stream timing out. Callers hold netMu.
func (g *Gateway) armNetTimer(s *netStream) {
	next := s.lastRX.Add(netStreamTimeout)
	if s.sending && s.airEnd.Add(-fillMargin).Before(next) {
		next = s.airEnd.Add(-fillMargin)
	}
	if g.netTimer == nil {
		g.netTimer = time.AfterFunc(time.Until(next), g.netTick)
	} else {
		g.netTimer.Reset(time.Until(next))
	}
}

// netTick runs when the reflector stream's timer fires. It sends silence if
// the modem is about to run out of frames, and ends a stream that has stopped.
func (g *Gateway) netTick() {
	g.netMu.Lock()
	s := g.net
	if s == nil {
		g.netMu.Unlock()
		return
	}
	held := g.netHeld()
	if held {
		s.sending = false
	}
	if time.Since(s.lastRX) >= netStreamTimeout {
		log.Printf("[DEBUG] Timed out Internet voice stream %04x", s.id)
		if s.sending {
			// Receivers get an end of transmission rather than carrier
			// dropping out from under them.
			g.sendSilenceFrame(s, true)
		}
		g.endNetStream(s)
		g.netMu.Unlock()
		if g.getState() == Echo {
			g.echoStreamEnd()
		}
		return
	}
	if s.sending && !time.Now().Before(s.airEnd.Add(-fillMargin)) {
		g.sendSilenceFrame(s, false)
		s.filled++
	}
	g.armNetTimer(s)
	g.netMu.Unlock()
}

// endNetStream logs the end of a reflector stream, with how it arrived.
// Callers hold netMu.
func (g *Gateway) endNetStream(s *netStream) {
	log.Printf("[DEBUG] Internet voice stream %04x from %s: %d frames, %d lost, %d filled, %d silent, longest gap %v",
		s.id, s.lsf.Src.Callsign(), s.frames, s.lost, s.filled, s.silent, s.maxGap.Round(time.Millisecond))
	g.dashLog.LogFrame(&s.lsf, "Internet", "Voice End",
		"frames", s.frames, "lost", s.lost, "filled", s.filled, "silent", s.silent,
		"maxGapMs", s.maxGap.Milliseconds())
	g.net = nil
}
