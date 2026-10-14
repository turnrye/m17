package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/m17/dashboard"
	"github.com/jancona/m17/inet"
)

// syncBuffer is a bytes.Buffer safe to write from the stream timer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// events returns the dashboard events of the given subtype.
func (b *syncBuffer) events(t *testing.T, subtype string) []map[string]any {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []map[string]any
	for line := range strings.Lines(b.buf.String()) {
		var e map[string]any
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("dashboard line %q: %v", line, err)
		}
		if e["subtype"] == subtype {
			out = append(out, e)
		}
	}
	return out
}

// netVoiceGateway is a test gateway linked to a reflector, with a dashboard
// log.
func netVoiceGateway(t *testing.T) (*Gateway, *recordingModem, *syncBuffer) {
	t.Helper()
	g, m := testGateway(t, 10*time.Millisecond, 10*time.Millisecond)
	g.link.Store(&inet.Client{})
	dash := &syncBuffer{}
	g.dashLog = dashboard.New(slog.New(slog.NewJSONHandler(dash, nil)))
	t.Cleanup(func() { stopNetTimer(g) })
	return g, m, dash
}

// stopNetTimer stops the stream timer of a test gateway, which has no
// reflector connection to close.
func stopNetTimer(g *Gateway) {
	g.netMu.Lock()
	defer g.netMu.Unlock()
	if g.netTimer != nil {
		g.netTimer.Stop()
	}
}

// voiceSender returns a function sending one reflector frame of the stream,
// a voice frame or silence.
func voiceSender(t *testing.T, g *Gateway, sid uint16) func(fn uint16, silent bool) {
	t.Helper()
	lsf, err := m17.NewLSF("@ALL", "W2JJT C", m17.LSFTypeStream, m17.LSFDataTypeVoice, 0)
	if err != nil {
		t.Fatal(err)
	}
	voice := bytes.Repeat([]byte{0x55}, 16)
	return func(fn uint16, silent bool) {
		payload := voice
		if silent {
			payload = codec2silence3200
		}
		l := lsf // each datagram gets its own LSF, as from the reflector
		if err := g.TransmitVoiceStream(m17.NewStreamDatagram(sid, fn, &l, payload)); err != nil {
			t.Fatal(err)
		}
	}
}

// voiceEnd waits for the stream's Voice End event and returns it.
func voiceEnd(t *testing.T, dash *syncBuffer) map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if ends := dash.events(t, "Voice End"); len(ends) > 0 {
			return ends[0]
		}
		if time.Now().After(deadline) {
			t.Fatal("no Voice End logged")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// count reads a numeric field of a dashboard event.
func count(e map[string]any, key string) int {
	n, _ := e[key].(float64)
	return int(n)
}

// contiguous reports whether frame numbers run on one by one, the last with
// its end bit.
func contiguous(fns []uint16) bool {
	for i := 1; i < len(fns); i++ {
		if fns[i]&0x7fff != (fns[i-1]+1)&0x7fff {
			return false
		}
	}
	return len(fns) > 0 && fns[len(fns)-1]&0x8000 != 0
}

// TestNetVoiceFillsGap: when reflector frames stop for longer than the modem
// has queued, silence goes out in their place, the late frames follow, and
// the frame numbers on air run on without a break.
func TestNetVoiceFillsGap(t *testing.T) {
	g, m, dash := netVoiceGateway(t)
	send := voiceSender(t, g, 0x1234)
	for fn := range uint16(5) {
		send(fn, false)
		time.Sleep(m17.FrameTime)
	}
	time.Sleep(300 * time.Millisecond)
	for fn := uint16(5); fn < 10; fn++ {
		send(fn, false)
	}
	send(10|0x8000, false)

	fns, silent := m.voiceFrames(), m.silentFrames()
	fills := 0
	for _, s := range silent {
		if s {
			fills++
		}
	}
	if fills == 0 {
		t.Fatalf("no silence sent during a 300 ms gap: frames %x", fns)
	}
	if got, want := len(fns), 11+fills; got != want {
		t.Errorf("%d frames on air, want 11 received + %d silence", got, fills)
	}
	if !contiguous(fns) {
		t.Errorf("frame numbers on air %x do not run on", fns)
	}
	e := voiceEnd(t, dash)
	if count(e, "frames") != 11 || count(e, "lost") != 0 || count(e, "filled") != fills {
		t.Errorf("Voice End %v, want 11 frames, 0 lost, %d filled", e, fills)
	}
	if count(e, "maxGapMs") < 300 {
		t.Errorf("Voice End maxGapMs %d, want at least 300", count(e, "maxGapMs"))
	}
}

// TestNetVoiceCountsLost: frames missing from the sequence are counted as
// lost, but neither the last frame repeated with its end bit nor a sender
// wrapping its frame number early is.
func TestNetVoiceCountsLost(t *testing.T) {
	g, m, dash := netVoiceGateway(t)
	send := voiceSender(t, g, 0x2345)
	// 0x7fd and 0x7fe are lost; then the sender wraps at 0x7ff, as one
	// bridge into M17-KCW does, and resends its last frame with the end bit.
	for _, fn := range []uint16{0x7fa, 0x7fb, 0x7fc, 0x7ff, 1, 2, 3, 3 | 0x8000} {
		send(fn, false)
	}
	e := voiceEnd(t, dash)
	if count(e, "frames") != 8 || count(e, "lost") != 2 {
		t.Errorf("Voice End %v, want 8 frames, 2 lost", e)
	}
	if fns := m.voiceFrames(); !contiguous(fns) || fns[0] != 0x7fa {
		t.Errorf("frame numbers on air %x, want 7fa onwards without a break", fns)
	}
}

// TestNetVoiceSilentBetweenVoice: silence received between voice frames is
// counted; silence before the first voice frame or after the last is not.
func TestNetVoiceSilentBetweenVoice(t *testing.T) {
	g, _, dash := netVoiceGateway(t)
	send := voiceSender(t, g, 0x3456)
	frames := []bool{true, false, true, true, true, false, false, true, true}
	for i, silent := range frames {
		fn := uint16(i)
		if i == len(frames)-1 {
			fn |= 0x8000
		}
		send(fn, silent)
	}
	if e := voiceEnd(t, dash); count(e, "silent") != 3 {
		t.Errorf("Voice End %v, want 3 silent", e)
	}
}

// TestNetVoiceEndsStoppedStream: a stream that stops without its last frame
// is filled with silence until it times out, then ended with a last frame,
// so receivers see an end of transmission.
func TestNetVoiceEndsStoppedStream(t *testing.T) {
	g, m, dash := netVoiceGateway(t)
	send := voiceSender(t, g, 0x4567)
	for fn := range uint16(3) {
		send(fn, false)
	}
	e := voiceEnd(t, dash)
	fns := m.voiceFrames()
	if !contiguous(fns) {
		t.Fatalf("frame numbers on air %x, want a run ending in a last frame", fns)
	}
	if count(e, "frames") != 3 || count(e, "filled") == 0 {
		t.Errorf("Voice End %v, want 3 frames and some filled", e)
	}
	time.Sleep(200 * time.Millisecond)
	if n := len(m.voiceFrames()); n != len(fns) {
		t.Errorf("%d more frames sent after the stream ended", n-len(fns))
	}
}

// cc1200Model is a modem that queues frames as the CC1200 HAT does: a
// transmission starts with preamble and LSF, each 40 ms frame takes about
// 21 ms to write over the serial link, and the modem sends bare carrier when
// its queue runs dry.
type cc1200Model struct {
	recordingModem
	airEnd time.Time     // when the queued frames run out
	dry    time.Duration // carrier sent with nothing queued, mid-transmission
}

func (c *cc1200Model) TransmitVoiceStream(sd m17.StreamDatagram) error {
	chunks := 1
	if c.airEnd.IsZero() {
		chunks = 3
	}
	for range chunks {
		time.Sleep(21 * time.Millisecond)
		now := time.Now()
		c.mu.Lock()
		if !c.airEnd.IsZero() && now.After(c.airEnd) {
			c.dry += now.Sub(c.airEnd)
		}
		c.airEnd = later(c.airEnd, now).Add(m17.FrameTime)
		c.mu.Unlock()
	}
	if sd.LastFrame {
		c.mu.Lock()
		c.airEnd = time.Time{}
		c.mu.Unlock()
	}
	return c.recordingModem.TransmitVoiceStream(sd)
}

// TestNetVoiceGapDoesNotStarveModem: a 250 ms pause in reflector frames,
// longer than a CC1200 has queued, does not leave the modem with nothing to
// send.
func TestNetVoiceGapDoesNotStarveModem(t *testing.T) {
	m := &cc1200Model{}
	g := &Gateway{modem: m, state: Idle, rxHoldoff: 10 * time.Millisecond}
	g.link.Store(&inet.Client{})
	t.Cleanup(func() { stopNetTimer(g) })
	send := voiceSender(t, g, 0x5678)
	next := time.Now()
	pace := func(fn uint16) {
		time.Sleep(time.Until(next))
		send(fn, false)
		next = next.Add(m17.FrameTime)
	}
	for fn := range uint16(10) {
		pace(fn)
	}
	next = next.Add(250 * time.Millisecond)
	for fn := uint16(10); fn < 20; fn++ {
		pace(fn)
	}
	pace(20 | 0x8000)
	m.mu.Lock()
	dry := m.dry
	m.mu.Unlock()
	if dry > 0 {
		t.Errorf("modem ran dry for %v", dry)
	}
	if fns := m.voiceFrames(); !slices.ContainsFunc(m.silentFrames(), func(s bool) bool { return s }) {
		t.Errorf("no silence sent: frames %x", fns)
	}
}
