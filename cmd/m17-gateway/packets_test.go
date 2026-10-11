package main

import (
	"sync"
	"testing"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/m17/inet"
)

// recordingModem records when each packet is transmitted and its text, and
// the frame number of each voice frame.
type recordingModem struct {
	mu    sync.Mutex
	sent  []time.Time
	texts []string
	voice []uint16
}

func (m *recordingModem) StartDecoding(func(uint16, []m17.SoftBit)) {}
func (m *recordingModem) Start() error                              { return nil }
func (m *recordingModem) Reset() error                              { return nil }
func (m *recordingModem) Close() error                              { return nil }
func (m *recordingModem) TransmitVoiceStream(sd m17.StreamDatagram) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.voice = append(m.voice, sd.FrameNumber)
	return nil
}
func (m *recordingModem) TransmitPacket(p m17.Packet) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = append(m.sent, time.Now())
	m.texts = append(m.texts, p.SMSText())
	return nil
}

func (m *recordingModem) sentTexts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.texts...)
}

func (m *recordingModem) voiceFrames() []uint16 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]uint16(nil), m.voice...)
}

func (m *recordingModem) times() []time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]time.Time(nil), m.sent...)
}

func testGateway(t *testing.T, holdoff, gap time.Duration) (*Gateway, *recordingModem) {
	t.Helper()
	m := &recordingModem{}
	g := &Gateway{modem: m, state: Idle, lastStreamID: 0xFFFF, rxHoldoff: holdoff, packetGap: gap, packetQueue: make(chan func() error, packetQueueLen)}
	go g.sendPackets()
	t.Cleanup(func() { close(g.packetQueue) })
	return g, m
}

func smsPacket(t *testing.T, text string) m17.Packet {
	t.Helper()
	p, err := m17.NewPacket("N1ADJ 8", "W1AW", m17.PacketTypeSMS, append([]byte(text), 0))
	if err != nil {
		t.Fatal(err)
	}
	return *p
}

func waitSent(t *testing.T, m *recordingModem, n int) []time.Time {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for len(m.times()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("%d packets sent, want %d", len(m.times()), n)
		}
		time.Sleep(5 * time.Millisecond)
	}
	return m.times()
}

// TestPacketsSpaced: queued packets go out one at a time, at least
// packetGap apart.
func TestPacketsSpaced(t *testing.T) {
	gap := 150 * time.Millisecond
	g, m := testGateway(t, 10*time.Millisecond, gap)
	for _, s := range []string{"one", "two", "three", "four"} {
		g.queueLocalPacket(smsPacket(t, s))
	}
	sent := waitSent(t, m, 4)
	for i := 1; i < len(sent); i++ {
		if d := sent[i].Sub(sent[i-1]); d < gap {
			t.Errorf("packets %d and %d only %v apart; gap is %v", i-1, i, d, gap)
		}
	}
}

// TestPacketWaitsForRF: a packet queued while a radio is transmitting goes
// out only after no RF has been received for rxHoldoff.
func TestPacketWaitsForRF(t *testing.T) {
	gap := 150 * time.Millisecond
	g, m := testGateway(t, gap, 10*time.Millisecond)
	stop := time.Now().Add(400 * time.Millisecond)
	g.rxHeard()
	g.queueLocalPacket(smsPacket(t, "while you talk"))
	var lastRF time.Time
	for time.Now().Before(stop) { // frames every 40 ms, as a voice stream
		g.rxHeard()
		lastRF = time.Now()
		if len(m.times()) > 0 {
			t.Fatal("packet transmitted while RF was being received")
		}
		time.Sleep(40 * time.Millisecond)
	}
	sent := waitSent(t, m, 1)
	if d := sent[0].Sub(lastRF); d < gap {
		t.Errorf("packet went %v after the last RF frame; holdoff is %v", d, gap)
	}
}

// TestPacketWaitsForOwnVoice: a packet waits packetGap after the gateway's
// own last voice frame.
func TestPacketWaitsForOwnVoice(t *testing.T) {
	gap := 150 * time.Millisecond
	g, m := testGateway(t, 10*time.Millisecond, gap)
	g.voiceSent()
	sentVoice := time.Now()
	g.queueLocalPacket(smsPacket(t, "after the voice"))
	sent := waitSent(t, m, 1)
	if d := sent[0].Sub(sentVoice); d < gap {
		t.Errorf("packet went %v after the last voice frame; gap is %v", d, gap)
	}
}

// testGated sends gated packets from a new queue, without a rate limit.
func testGated(t *testing.T, g *Gateway, gap time.Duration) *gatedQueue {
	t.Helper()
	q := newGatedQueue(time.Minute, 6000, 100, time.Now())
	stop := make(chan struct{})
	go g.sendGated(q, func() time.Duration { return gap }, stop)
	t.Cleanup(func() { close(stop) })
	return q
}

func gatedSMS(t *testing.T, text string) *gatedPacket {
	t.Helper()
	return &gatedPacket{class: gatedMessage, key: text, line: text, packet: smsPacket(t, text), queued: time.Now()}
}

// TestGatedSpaced: gated packets go one at a time, each at least minTxGap
// after the one before.
func TestGatedSpaced(t *testing.T) {
	gap := 200 * time.Millisecond
	g, m := testGateway(t, 10*time.Millisecond, 10*time.Millisecond)
	q := testGated(t, g, gap)
	for _, s := range []string{"one", "two", "three"} {
		q.push(gatedSMS(t, s))
	}
	sent := waitSent(t, m, 3)
	for i := 1; i < len(sent); i++ {
		if d := sent[i].Sub(sent[i-1]); d < gap {
			t.Errorf("gated packets %d and %d only %v apart; gap is %v", i-1, i, d, gap)
		}
	}
}

// TestGatedWaitsAfterRF: a gated packet waits minTxGap after local RF, not
// just rxHoldoff, so that the next over can start first.
func TestGatedWaitsAfterRF(t *testing.T) {
	gap := 300 * time.Millisecond
	g, m := testGateway(t, 50*time.Millisecond, 10*time.Millisecond)
	q := testGated(t, g, gap)
	g.rxStreamEnded()
	ended := time.Now()
	q.push(gatedSMS(t, "after the over"))
	sent := waitSent(t, m, 1)
	if d := sent[0].Sub(ended); d < gap {
		t.Errorf("gated packet went %v after RF; gap is %v", d, gap)
	}
}

// slowModem takes a while to transmit a packet, as a real one does.
type slowModem struct {
	recordingModem
}

func (m *slowModem) TransmitPacket(p m17.Packet) error {
	time.Sleep(150 * time.Millisecond)
	return m.recordingModem.TransmitPacket(p)
}

// TestGatedHoldsVoice: reflector voice arriving while an APRS packet is
// sent waits for it, unless HoldVoice is off.
func TestGatedHoldsVoice(t *testing.T) {
	for _, hold := range []bool{true, false} {
		m := &slowModem{}
		g := &Gateway{modem: m, state: Idle, lastStreamID: 0xFFFF, rxHoldoff: time.Millisecond,
			packetGap: time.Millisecond, packetQueue: make(chan func() error, packetQueueLen)}
		g.link.Store(&inet.Client{})
		g.aprs = &igate{cfg: aprsConfig{holdVoice: hold}, stats: newAPRSStats(), now: time.Now}
		go g.sendPackets()
		lsf, err := m17.NewLSF("@ALL", "W7GKB D", m17.LSFTypeStream, m17.LSFDataTypeVoice, 0)
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan struct{})
		go func() {
			g.transmitGated(gatedSMS(t, "packet"), time.Millisecond)
			close(done)
		}()
		time.Sleep(50 * time.Millisecond) // the packet is going out
		g.TransmitVoiceStream(m17.NewStreamDatagram(0x6425, 0, &lsf, make([]byte, 16)))
		<-done
		close(g.packetQueue)
		if got := len(m.voiceFrames()); (got == 0) != hold {
			t.Errorf("HoldVoice %v: %d voice frames sent during the packet", hold, got)
		}
	}
}

// TestReflectorFirst: packets from the reflector, or the gateway's own,
// go before gated ones, and a gated one still waits minTxGap after them.
func TestReflectorFirst(t *testing.T) {
	gap := 100 * time.Millisecond
	g, m := testGateway(t, 10*time.Millisecond, 10*time.Millisecond)
	slow := smsPacket(t, "slow")
	g.enqueue(func() error {
		time.Sleep(150 * time.Millisecond)
		return m.TransmitPacket(slow)
	}, slow)
	time.Sleep(10 * time.Millisecond) // the sender has taken it
	g.queueLocalPacket(smsPacket(t, "local"))
	q := testGated(t, g, gap)
	q.push(gatedSMS(t, "gated"))
	sent := waitSent(t, m, 3)
	if got := m.sentTexts(); got[0] != "slow" || got[1] != "local" || got[2] != "gated" {
		t.Errorf("sent %v", got)
	}
	if d := sent[2].Sub(sent[1]); d < gap {
		t.Errorf("gated packet went %v after the local one; gap is %v", d, gap)
	}
}
