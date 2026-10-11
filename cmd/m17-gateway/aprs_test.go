package main

import (
	"strconv"
	"strings"
	"testing"
	"time"

	fap "github.com/hessu/go-aprs-fap"
	"github.com/jancona/m17"
)

const testGate = "K0RET-10"

// testIgate returns a gate with a verified login and IS to RF on, whose
// clock is *now, without connecting it.
func testIgate(t *testing.T, now *time.Time) *igate {
	t.Helper()
	cfg := aprsConfig{
		enabled:     true,
		callsign:    testGate,
		passcode:    strconv.Itoa(int(fap.AprsPasscode(testGate))),
		rfToIS:      true,
		isToRF:      true,
		heardWindow: time.Hour,
		maxQueueAge: time.Hour,
		rateLimit:   60,
		rateBurst:   100,
		courtesy:    true,
	}
	a := newIgate(cfg, "K0RET   H")
	a.now = func() time.Time { return *now }
	return a
}

// hearRF has the gate hear line on RF from M17 station src on CAN can.
func hearRF(t *testing.T, a *igate, src string, can byte, line string) {
	t.Helper()
	lsf, err := m17.NewLSF(m17.DestinationAll, src, m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	if err != nil {
		t.Fatal(err)
	}
	lsf.Type[0] |= can >> 1
	lsf.Type[1] |= can << 7
	a.fromRF(&lsf, append([]byte(line), 0))
}

// sentIS returns the lines queued for APRS-IS.
func sentIS(a *igate) []string {
	var out []string
	for {
		select {
		case l := <-a.out:
			out = append(out, l)
		default:
			return out
		}
	}
}

// queuedRF returns the lines queued for RF, emptying the queue.
func queuedRF(a *igate, now time.Time) []*gatedPacket {
	var out []*gatedPacket
	for {
		p, _ := a.queue.next(now)
		if p == nil {
			return out
		}
		out = append(out, p)
	}
}

func TestRFGatedToIS(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:!3508.37N/09001.05W[OpenRTX")
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1,NOGATE:>not me")
	got := sentIS(a)
	want := "K0RET-7>APORT1,qAR,K0RET-10:!3508.37N/09001.05W[OpenRTX"
	if len(got) != 1 || got[0] != want {
		t.Errorf("sent %q", got)
	}
	if a.filter != "g/K0RET-10/K0RET-7" {
		t.Errorf("filter %q", a.filter)
	}
}

// TestRFDuplicate: a repeat within 30 s is not gated again; a retry after
// it is.
func TestRFDuplicate(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	line := "K0RET-7>APORT1::W1AW     :hello{01}"
	hearRF(t, a, "K0RET", 0, line)
	sentIS(a)
	now = now.Add(20 * time.Second)
	hearRF(t, a, "K0RET", 0, line)
	if got := sentIS(a); len(got) != 0 {
		t.Errorf("repeat gated: %q", got)
	}
	now = now.Add(11 * time.Second)
	hearRF(t, a, "K0RET", 0, line)
	if got := sentIS(a); len(got) != 1 {
		t.Errorf("retry not gated: %q", got)
	}
}

// TestReceiveOnly: with passcode -1 nothing is sent to APRS-IS.
func TestReceiveOnly(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	a.verified = false
	a.cfg.isToRF = false
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>status")
	if got := sentIS(a); len(got) != 0 {
		t.Errorf("sent %q", got)
	}
}

// TestMessageToHeardStation: a message to a station heard on RF is queued
// as a broadcast from the gateway, on the CAN the station was heard on.
func TestMessageToHeardStation(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 5, "K0RET-7>APORT1:>here")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::N0BODY   :hello{AC}")
	q := queuedRF(a, now)
	if len(q) != 1 {
		t.Fatalf("queued %d, want 1", len(q))
	}
	p := q[0]
	if p.class != gatedMessage || p.line != "W1AW>APK004,TCPIP,K0RET-10*::K0RET-7  :hello{AB}" {
		t.Errorf("queued %s %q", p.class, p.line)
	}
	lsf := p.packet.LSF
	if !m17.IsBroadcast(lsf.Dst) || lsf.Src.Callsign() != "K0RET   H" || !lsf.CheckCRC() {
		t.Errorf("LSF %s", lsf)
	}
	if can := (lsf.Type[0]&0x07)<<1 | lsf.Type[1]>>7; can != 5 {
		t.Errorf("CAN %d, want 5", can)
	}
	if got := strings.TrimRight(string(p.packet.Payload), "\x00"); got != p.line || p.packet.Type != m17.PacketTypeAPRS {
		t.Errorf("payload %q type %d", got, p.packet.Type)
	}
}

// TestHeardWindow: once a station hasn't been heard for HeardWindow,
// messages to it stay off RF.
func TestHeardWindow(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	now = now.Add(61 * time.Minute)
	a.expire(now)
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	if a.queue.len() != 0 {
		t.Error("message to a station gone for over an hour queued")
	}
	if strings.Contains(a.filter, "K0RET-7") {
		t.Errorf("filter %q still asks for K0RET-7", a.filter)
	}
}

// TestAlreadyOnAir: a packet from APRS-IS that was heard on RF in the
// last minute is not sent again.
func TestAlreadyOnAir(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	// Another hotspot gated W5XYZ-7's message; we heard it too.
	hearRF(t, a, "W5XYZ", 0, "W5XYZ-7>APORT1::K0RET-7  :hi{01}")
	now = now.Add(2 * time.Minute)
	hearRF(t, a, "W5XYZ", 0, "W5XYZ-7>APORT1::K0RET-7  :hi{02}")
	now = now.Add(5 * time.Second)
	a.fromIS("W5XYZ-7>APORT1,qAR,N0GW::K0RET-7  :hi{02}")
	if a.queue.len() != 0 {
		t.Error("packet heard on RF 5 s ago queued")
	}
	now = now.Add(time.Minute)
	a.fromIS("W5XYZ-7>APORT1,qAR,N0GW::K0RET-7  :hi{02}")
	if a.queue.len() != 1 {
		t.Error("retry after the on-air window not queued")
	}
}

// TestGatedByOthers: a packet another gate brought from APRS-IS is on the
// air, but doesn't make its source a station heard here, nor go back.
func TestGatedByOthers(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	hearRF(t, a, "N0GW", 0, "W1AW>APK004,TCPIP,N0GW*::K0RET-7  :hi{AB}")
	if a.heard["W1AW"] != nil {
		t.Error("W1AW heard on RF")
	}
	if got := sentIS(a); len(got) != 1 {
		t.Errorf("sent %q", got)
	}
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hi{AB}")
	if a.queue.len() != 0 {
		t.Error("packet on the air already queued")
	}
	a.fromIS("K0RET-7>APORT1,qAR,N0GW::W1AW     :ackAB")
	if a.queue.len() != 0 {
		t.Error("ack to W1AW queued")
	}
}

// TestRelayed: a packet another gateway relayed from its reflector, with
// Extended Callsign Data in the LSF, doesn't make its source local.
func TestRelayed(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	lsf, err := m17.NewLSF(m17.DestinationAll, "W5XYZ", m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	if err != nil {
		t.Fatal(err)
	}
	gw, _ := m17.EncodeCallsign("N0GW    C")
	lsf.SetECD(gw, nil)
	a.fromRF(&lsf, []byte("W5XYZ-7>APORT1:>far away\x00"))
	if a.heard["W5XYZ-7"] != nil {
		t.Error("relayed station heard as local")
	}
	if got := sentIS(a); len(got) != 1 {
		t.Errorf("relayed packet not gated: %q", got)
	}
}

// TestLocalSender: a message from a station heard on RF in the last minute
// stays off RF: the addressee can hear it.
func TestLocalSender(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	hearRF(t, a, "W5XYZ", 0, "W5XYZ-7>APORT1:>also here")
	a.fromIS("W5XYZ-7>APORT1,qAR,N0GW::K0RET-7  :hi{01}")
	if a.queue.len() != 0 {
		t.Error("message from a local sender queued")
	}
	now = now.Add(61 * time.Second)
	a.fromIS("W5XYZ-7>APORT1,qAR,N0GW::K0RET-7  :hi{01}")
	if a.queue.len() != 1 {
		t.Error("message from a sender not heard for a minute not queued")
	}
}

// TestAckCancels: an ack heard on RF drops the queued message it acks, and
// a sender's retry replaces its queued copy.
func TestAckCancels(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	now = now.Add(30 * time.Second)
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	if a.queue.len() != 1 {
		t.Fatalf("%d queued after a retry, want 1", a.queue.len())
	}
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1::W1AW     :ackAB")
	if a.queue.len() != 0 {
		t.Error("acked message still queued")
	}
	got := sentIS(a)
	if got[len(got)-1] != "K0RET-7>APORT1,qAR,K0RET-10::W1AW     :ackAB" {
		t.Errorf("ack not gated: %q", got)
	}
}

// TestAckClass: an ack from APRS-IS goes ahead of messages.
func TestAckClass(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :ack01")
	q := queuedRF(a, now)
	if len(q) != 2 || q[0].class != gatedAck || q[1].class != gatedMessage {
		t.Errorf("queued %v", q)
	}
}

// TestNoGateFromIS: path words and unverified logins keep packets off RF;
// so does an address the radio can't take.
func TestNoGateFromIS(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	for _, l := range []string{
		"W1AW>APK004,TCPXX*,qAX,T2TEST::K0RET-7  :hi{1}",
		"W1AW>APK004,NOGATE,qAR,N0GW::K0RET-7  :hi{2}",
		"W1AW>APK004,RFONLY,qAR,N0GW::K0RET-7  :hi{3}",
		"WINLINK>APWL2K,TCPIP*,qAC,T2TEST::K0RET-7  :mail",
		"K0RET-10>APZ001,TCPIP*,qAC,T2TEST::K0RET-7  :me",
		"W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-10 :to the gate{4}",
	} {
		a.fromIS(l)
	}
	if n := a.queue.len(); n != 0 {
		t.Errorf("%d queued, want 0: %v", n, queuedRF(a, now))
	}
}

// TestCourtesyPosition: after a message, the sender's position goes to RF,
// the one already seen or the next to arrive, and not again for a while.
func TestCourtesyPosition(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:!4142.35N/07243.81W-")
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	q := queuedRF(a, now)
	if len(q) != 2 || q[0].class != gatedMessage || q[1].line != "W1AW>APK004,TCPIP,K0RET-10*:!4142.35N/07243.81W-" {
		t.Fatalf("queued %v", q)
	}

	// No position seen: the filter asks for the next one.
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hi{01}")
	if !strings.Contains(a.filter, "b/N1ADJ") {
		t.Fatalf("filter %q", a.filter)
	}
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST:!4352.73N/06959.22W-")
	q = queuedRF(a, now)
	if len(q) != 2 || q[1].line != "N1ADJ>APK004,TCPIP,K0RET-10*:!4352.73N/06959.22W-" {
		t.Fatalf("queued %v", q)
	}
	if strings.Contains(a.filter, "b/") {
		t.Errorf("filter %q still asks for a position", a.filter)
	}
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :again{02}")
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST:!4352.73N/06959.22W-")
	if q = queuedRF(a, now); len(q) != 1 {
		t.Errorf("courtesy position repeated: %v", q)
	}
}

// watching sets a's rules from Watch values.
func watching(t *testing.T, a *igate, values ...string) {
	t.Helper()
	a.cfg.watches = nil
	for i, v := range values {
		w, err := parseWatch("Watch"+strconv.Itoa(i+1), v)
		if err != nil {
			t.Fatal(err)
		}
		a.cfg.watches = append(a.cfg.watches, w)
	}
	a.updateFilterLocked()
}

// TestRawFilterWatched: what only the raw Filter brings is logged, not
// transmitted.
func TestRawFilterWatched(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	a.cfg.filter = "m/10"
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>status")
	if a.queue.len() != 0 || a.counts["isWatched"] != 1 {
		t.Errorf("queued %d, watched %d", a.queue.len(), a.counts["isWatched"])
	}
}

// TestWatchArea: an rf area rule transmits positions inside its radius,
// and later packets from those stations, but not local stations' or ones
// outside it.
func TestWatchArea(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	watching(t, a, "area 41.7 -72.7 20 rf")
	if !strings.Contains(a.filter, "r/41.7000/-72.7000/20") {
		t.Errorf("filter %q", a.filter)
	}
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:!4142.35N/07243.81W-")  // inside
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>status")               // its last position is inside
	a.fromIS("N1ADJ>APK004,TCPIP*,qAC,T2TEST:!4352.73N/06959.22W-") // outside
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:!4142.00N/07243.00W[")
	a.fromIS("K0RET-7>APORT1,qAR,N0GW:!4142.00N/07243.00W[x")
	q := queuedRF(a, now)
	if len(q) != 2 || q[0].from != "W1AW" || q[1].from != "W1AW" {
		t.Errorf("queued %v", q)
	}
}

// TestWatchCall: a call rule selects traffic from a station and messages
// to it; dashboard rules only log it; rules end on time.
func TestWatchCall(t *testing.T) {
	now := time.Now()
	a := testIgate(t, &now)
	until := now.Add(time.Hour).UTC().Format(time.RFC3339)
	watching(t, a, "call W1AW rf until "+until, "call N1ADJ* dashboard")
	if !strings.Contains(a.filter, "b/W1AW g/W1AW") || !strings.Contains(a.filter, "b/N1ADJ* g/N1ADJ*") {
		t.Errorf("filter %q", a.filter)
	}
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>status")
	a.fromIS("KK4IOH-1>APIN22,TCPIP*,qAC,T2TEST::W1AW     :hi{1")
	a.fromIS("N1ADJ-7>APK004,TCPIP*,qAC,T2TEST:>status")
	if q := queuedRF(a, now); len(q) != 2 || q[1].class != gatedOther {
		t.Errorf("queued %v", q)
	}
	if a.counts["isWatched"] != 1 {
		t.Errorf("watched %d, want 1", a.counts["isWatched"])
	}
	now = now.Add(2 * time.Hour)
	a.expire(now)
	if strings.Contains(a.filter, "W1AW ") {
		t.Errorf("ended rule still in filter %q", a.filter)
	}
	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST:>later")
	if a.queue.len() != 0 {
		t.Error("ended rule still transmits")
	}
}

// TestGatewayAPRS: through the gateway, an APRS packet received on RF goes
// to APRS-IS, and a message back to its sender is transmitted.
func TestGatewayAPRS(t *testing.T) {
	g, m := noReflectorGateway(t, false)
	now := time.Now()
	a := testIgate(t, &now)
	a.now = time.Now
	a.m17Call = g.callsign
	g.aprs = a
	stop := make(chan struct{})
	go g.sendGated(a.queue, func() time.Duration { return 50 * time.Millisecond }, stop)
	t.Cleanup(func() { close(stop) })

	p, err := m17.NewPacket(m17.DestinationAll, "K0RET", m17.PacketTypeAPRS, []byte("K0RET-7>APORT1:!3508.37N/09001.05W[\x00"))
	if err != nil {
		t.Fatal(err)
	}
	if err := g.receivedRFPacket(*p.LSF, p.PayloadBytes(), 0); err != nil {
		t.Fatal(err)
	}
	if got := sentIS(a); len(got) != 1 || got[0] != "K0RET-7>APORT1,qAR,K0RET-10:!3508.37N/09001.05W[" {
		t.Errorf("sent to APRS-IS %q", got)
	}

	a.fromIS("W1AW>APK004,TCPIP*,qAC,T2TEST::K0RET-7  :hello{AB}")
	waitSent(t, &m.recordingModem, 1)
	tx := m.sent()[0]
	want := "W1AW>APK004,TCPIP,K0RET-10*::K0RET-7  :hello{AB}\x00"
	if tx.Type != m17.PacketTypeAPRS || string(tx.Payload) != want || !m17.IsBroadcast(tx.LSF.Dst) || tx.LSF.Src.Callsign() != "N1ADJ G" {
		t.Errorf("transmitted %s", tx)
	}
}

// relayIgate returns a gate that relays, having heard K0RET-7 (M17 K0RET)
// and W5XYZ-7 (M17 W5XYZ, CAN 3) on RF.
func relayIgate(t *testing.T, now *time.Time) *igate {
	t.Helper()
	a := testIgate(t, now)
	a.cfg.relay, a.cfg.relayHold = true, 5*time.Second
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1:>here")
	hearRF(t, a, "W5XYZ", 3, "W5XYZ-7>APORT1:>also here")
	return a
}

// TestRelay: a message between two stations heard here is relayed after
// the hold, marked with the gateway; the ack back is relayed too.
func TestRelay(t *testing.T) {
	now := time.Now()
	a := relayIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1::W5XYZ-7  :hi{01}")
	if p, wait := a.queue.next(now); p != nil || wait != 5*time.Second {
		t.Fatalf("relay not held: %v, %v", p, wait)
	}
	now = now.Add(5 * time.Second)
	q := queuedRF(a, now)
	if len(q) != 1 {
		t.Fatalf("queued %v", q)
	}
	p := q[0]
	lsf := p.packet.LSF
	if p.class != gatedMessage || p.line != "K0RET-7>APORT1,K0RET-10*::W5XYZ-7  :hi{01}" {
		t.Errorf("relayed %s %q", p.class, p.line)
	}
	if lsf.Src.Callsign() != "K0RET" || lsf.ECD() == nil || lsf.ECD().Callsign1.Callsign() != "K0RET   H" || !lsf.CheckCRC() {
		t.Errorf("LSF %s", lsf)
	}
	if can := (lsf.Type[0]&0x07)<<1 | lsf.Type[1]>>7; can != 3 {
		t.Errorf("CAN %d, want the addressee's 3", can)
	}
	p.sent()

	hearRF(t, a, "W5XYZ", 3, "W5XYZ-7>APORT1::K0RET-7  :ack01")
	q = queuedRF(a, now)
	if len(q) != 1 || q[0].class != gatedAck || q[0].line != "W5XYZ-7>APORT1,K0RET-10*::K0RET-7  :ack01" {
		t.Errorf("ack relay %v", q)
	}
}

// TestRelayHeardDirectly: when the addressee acks within the hold, it
// heard the sender directly: nothing is relayed either way.
func TestRelayHeardDirectly(t *testing.T) {
	now := time.Now()
	a := relayIgate(t, &now)
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1::W5XYZ-7  :hi{01}")
	now = now.Add(2 * time.Second)
	hearRF(t, a, "W5XYZ", 3, "W5XYZ-7>APORT1::K0RET-7  :ack01")
	now = now.Add(10 * time.Second)
	if q := queuedRF(a, now); len(q) != 0 {
		t.Errorf("relayed %v", q)
	}
}

// TestRelayNot: positions, unheard or group addressees, and packets from
// APRS-IS or another gateway are not relayed; nor anything when off.
func TestRelayNot(t *testing.T) {
	now := time.Now()
	a := relayIgate(t, &now)
	for _, l := range []string{
		"K0RET-7>APORT1:!3508.37N/09001.05W[",
		"K0RET-7>APORT1::N0BODY   :hi{01}",
		"K0RET-7>APORT1::BLN1     :bulletin",
		"W1AW>APK004,TCPIP,N0GW*::W5XYZ-7  :hi{02}",
		"K0RET-7>APORT1,K0RET-10*::W5XYZ-7  :hi{03}",
		"W5XYZ-7>APORT1::K0RET-7  :ack09",
	} {
		hearRF(t, a, "K0RET", 0, l)
	}
	lsf, _ := m17.NewLSF(m17.DestinationAll, "K0RET", m17.LSFTypePacket, m17.LSFDataTypeData, 0)
	gw, _ := m17.EncodeCallsign("N0GW    C")
	lsf.SetECD(gw, nil)
	a.fromRF(&lsf, []byte("K0RET-7>APORT1::W5XYZ-7  :relayed{04}\x00"))
	a.cfg.relay = false
	hearRF(t, a, "K0RET", 0, "K0RET-7>APORT1::W5XYZ-7  :off{05}")
	if q := queuedRF(a, now.Add(time.Minute)); len(q) != 0 {
		t.Errorf("relayed %v", q)
	}
}
