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
	if fns, want := m.voiceFrames(), []uint16{0x7fa, 0x7fb, 0x7fc, 0x7ff, 1, 2, 3, 3 | 0x8000}; !slices.Equal(fns, want) {
		t.Errorf("frame numbers on air %x, want %x", fns, want)
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
// is logged as ended once it times out.
func TestNetVoiceEndsStoppedStream(t *testing.T) {
	g, _, dash := netVoiceGateway(t)
	send := voiceSender(t, g, 0x4567)
	for fn := range uint16(3) {
		send(fn, false)
	}
	if e := voiceEnd(t, dash); count(e, "frames") != 3 || count(e, "lost") != 0 {
		t.Errorf("Voice End %v, want 3 frames, 0 lost", e)
	}
}
