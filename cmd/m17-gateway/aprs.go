package main

import (
	"errors"
	"fmt"
	"log"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	fap "github.com/hessu/go-aprs-fap"
	"github.com/jancona/m17"
	"gopkg.in/ini.v1"
)

// The APRS gate passes APRS packets carried in M17 packet mode (type
// 0x02, a TNC2 line and a NUL) between RF and APRS-IS, as an iGate does.
// See docs/aprs-igate.md for the design and its reasons.

const (
	aprsAppName    = "m17-gateway"
	aprsAppVersion = "0.1"
	// aprsToCall is the destination of packets the gate makes itself: its
	// beacon, and wrapped third-party packets. Experimental, as m17-bridge's.
	aprsToCall = "APZ001"

	aprsReadTimeout  = 2 * time.Minute // servers send a keepalive every 20 s
	aprsReconnectMin = 10 * time.Second
	aprsReconnectMax = 5 * time.Minute
	aprsTick         = time.Minute
	aprsStatusEvery  = 5 * time.Second

	// rfDupeWindow suppresses a packet heard on RF again, as APRS-IS does.
	rfDupeWindow = 30 * time.Second
	// onAirWindow keeps an APRS-IS packet off RF when the same packet was
	// heard on RF, or sent there by the gate, this recently.
	onAirWindow = 60 * time.Second
	// localSenderWindow keeps a message off RF when its sender was heard
	// on RF this recently, as Dire Wolf does: the addressee can hear it.
	localSenderWindow = time.Minute
	// courtesyInterval is how often a sender's courtesy position may go.
	courtesyInterval = 30 * time.Minute
	// relayAckWindow is how long after relaying a message its ack is
	// relayed back.
	relayAckWindow = 5 * time.Minute
)

// aprsConfig is the [APRS] section of the configuration.
type aprsConfig struct {
	enabled        bool
	server         string
	callsign       string
	passcode       string
	rfToIS         bool
	isToRF         bool
	filter         string
	heardWindow    time.Duration
	maxQueueAge    time.Duration
	minTxGap       time.Duration
	rateLimit      int
	rateBurst      int
	courtesy       bool
	wrap           bool
	relay          bool
	relayHold      time.Duration
	beaconInterval time.Duration
	hasPosition    bool
	latitude       float64
	longitude      float64
	symbol         string
	comment        string
	can            int     // the CAN radios use here, for the beacon
	haat           float64 // antenna height above average terrain, m
	gain           float64 // antenna gain, dBi
	ambiguity      int     // beacon position ambiguity, 0 to 4
	logPath        string
	statusPath     string
	watches        []watchRule
}

func loadAPRSConfig(s *ini.Section) (aprsConfig, error) {
	c := aprsConfig{
		enabled:        s.Key("Enabled").MustBool(false),
		server:         s.Key("Server").MustString("rotate.aprs2.net:14580"),
		callsign:       strings.ToUpper(strings.TrimSpace(s.Key("Callsign").String())),
		passcode:       strings.TrimSpace(s.Key("Passcode").MustString("-1")),
		rfToIS:         s.Key("RFtoIS").MustBool(true),
		isToRF:         s.Key("IStoRF").MustBool(false),
		filter:         strings.TrimSpace(s.Key("Filter").String()),
		heardWindow:    s.Key("HeardWindow").MustDuration(60 * time.Minute),
		maxQueueAge:    s.Key("MaxQueueAge").MustDuration(60 * time.Second),
		minTxGap:       s.Key("MinTxGap").MustDuration(3 * time.Second),
		rateLimit:      s.Key("RateLimit").MustInt(6),
		rateBurst:      s.Key("RateBurst").MustInt(3),
		courtesy:       s.Key("CourtesyPosition").MustBool(true),
		wrap:           s.Key("ThirdParty").MustBool(false),
		relay:          s.Key("Relay").MustBool(false),
		relayHold:      s.Key("RelayHold").MustDuration(5 * time.Second),
		beaconInterval: s.Key("BeaconInterval").MustDuration(30 * time.Minute),
		symbol:         s.Key("Symbol").MustString("M&"),
		comment:        s.Key("Comment").String(),
		can:            s.Key("CAN").MustInt(0),
		haat:           s.Key("HAAT").MustFloat64(0),
		gain:           s.Key("Gain").MustFloat64(0),
		ambiguity:      s.Key("Ambiguity").MustInt(0),
		logPath:        strings.TrimSpace(s.Key("Log").String()),
		statusPath:     strings.TrimSpace(s.Key("Status").String()),
	}
	var watchErrs []error
	c.watches, watchErrs = loadWatches(s)
	for _, err := range watchErrs {
		log.Printf("[ERROR] APRS: ignoring %v", err)
	}
	if !c.enabled {
		return c, nil
	}
	var errs []error
	if !isAX25Call(c.callsign) {
		errs = append(errs, fmt.Errorf("APRS Callsign %q must be an AX.25 callsign, such as N0CALL-10", c.callsign))
	}
	if _, err := strconv.Atoi(c.passcode); err != nil {
		errs = append(errs, fmt.Errorf("APRS Passcode %q must be a number, or -1 to receive only", c.passcode))
	}
	if !strings.Contains(c.server, ":") {
		c.server += ":14580"
	}
	if c.rateLimit < 1 || c.rateBurst < 1 {
		errs = append(errs, fmt.Errorf("APRS RateLimit and RateBurst must be at least 1"))
	}
	if c.can < 0 || c.can > 15 || c.ambiguity < 0 || c.ambiguity > 4 || c.haat < 0 || c.gain < 0 {
		errs = append(errs, fmt.Errorf("APRS CAN must be 0 to 15, Ambiguity 0 to 4, and HAAT and Gain not negative"))
	}
	if len(c.symbol) != 2 {
		errs = append(errs, fmt.Errorf("APRS Symbol must be a table and a code, such as M&"))
	}
	if s.HasKey("Latitude") || s.HasKey("Longitude") {
		lat, err1 := s.Key("Latitude").Float64()
		lon, err2 := s.Key("Longitude").Float64()
		if err1 != nil || err2 != nil || lat < -90 || lat > 90 || lon < -180 || lon > 180 {
			errs = append(errs, fmt.Errorf("APRS Latitude and Longitude must be decimal degrees"))
		}
		c.latitude, c.longitude, c.hasPosition = lat, lon, true
	}
	return c, errors.Join(errs...)
}

// heardStation is a station heard on RF.
type heardStation struct {
	m17Call string // the M17 source it was heard from
	lsfType [2]byte
	last    time.Time
}

// igate holds the APRS gate's state.
type igate struct {
	cfg      aprsConfig
	verified bool // the passcode matches, so APRS-IS takes what is sent
	m17Call  string
	m17Enc   *m17.EncodedCallsign
	duplex   bool // no relay: a duplex gateway repeats every packet already
	radio    radioInfo
	link     func() string // the linked reflector, for the beacon
	queue    *gatedQueue
	log      *aprsLog // nil unless Log is set
	now      func() time.Time
	reconf   chan struct{}

	mu           sync.Mutex
	heard        map[string]*heardStation // by APRS callsign
	onAir        map[string]time.Time     // packet keys heard on RF or sent there
	toIS         map[string]time.Time     // packet keys gated to APRS-IS
	positions    map[string]string        // latest APRS-IS position line by source
	positionTime map[string]time.Time
	courtesyDue  map[string]time.Time // senders whose next position goes to RF
	courtesySent map[string]time.Time
	courtesyLSF  map[string][2]byte
	relayed      map[string]time.Time // messages relayed, as "SRC>DST{id"
	lastPosition map[string]posAt     // latest APRS-IS position by source
	filter       string               // the server filter last asked for
	counts       map[string]int       // traffic counters, for the status file
	server       string               // the APRS-IS server, from its keepalives
	serverLine   string               // its last keepalive
	serverSeen   time.Time
	connectedAt  time.Time
	lastBeacon   time.Time
	beaconLine   string
	conn         *fap.Conn
	out          chan string
	done         chan struct{}
	closeOnce    sync.Once
}

func newIgate(cfg aprsConfig, m17Call string) *igate {
	a := &igate{
		cfg:          cfg,
		verified:     cfg.passcode == strconv.Itoa(int(fap.AprsPasscode(cfg.callsign))),
		m17Call:      m17Call,
		now:          time.Now,
		heard:        map[string]*heardStation{},
		onAir:        map[string]time.Time{},
		toIS:         map[string]time.Time{},
		positions:    map[string]string{},
		positionTime: map[string]time.Time{},
		courtesyDue:  map[string]time.Time{},
		courtesySent: map[string]time.Time{},
		courtesyLSF:  map[string][2]byte{},
		relayed:      map[string]time.Time{},
		lastPosition: map[string]posAt{},
		counts:       map[string]int{},
		out:          make(chan string, 64),
		done:         make(chan struct{}),
		reconf:       make(chan struct{}, 1),
	}
	a.m17Enc, _ = m17.EncodeCallsign(m17Call)
	// Always there, so that IStoRF or Relay can be turned on while running.
	a.queue = newGatedQueue(cfg.maxQueueAge, cfg.rateLimit, cfg.rateBurst, time.Now())
	a.queue.dropped = func(p *gatedPacket, why string) {
		a.counts["rfDropped"]++
		a.log.write(a.now(), aprsEvent{Dir: aprsToRF, Line: p.line, Result: why, Class: p.class.String()})
	}
	a.filter = a.buildFilter()
	return a
}

// posAt is a station's position and when it was seen.
type posAt struct {
	p  [2]float64
	at time.Time
}

// passcodeOK reports whether c's passcode is its callsign's, so that
// APRS-IS takes what the gate sends.
func passcodeOK(c aprsConfig) bool {
	return c.passcode == strconv.Itoa(int(fap.AprsPasscode(c.callsign)))
}

// reconfigure applies a new [APRS] section while the gateway runs. A new
// server, callsign or passcode means logging in again; everything else
// takes effect at once.
func (a *igate) reconfigure(c aprsConfig) {
	if c.relay && a.duplex {
		c.relay = false
	}
	if !c.enabled {
		c.rfToIS, c.isToRF, c.relay = false, false, false
	}
	a.mu.Lock()
	old := a.cfg
	a.cfg = c
	a.verified = passcodeOK(c)
	a.queue.setParams(c.maxQueueAge, c.rateLimit, c.rateBurst)
	a.updateFilterLocked()
	conn := a.conn
	a.mu.Unlock()
	relogin := old.enabled != c.enabled || old.server != c.server ||
		old.callsign != c.callsign || old.passcode != c.passcode
	log.Printf("[INFO] APRS: configuration changed%s", map[bool]string{true: "; logging in again", false: ""}[relogin])
	if relogin && conn != nil {
		conn.Close() // run() logs in again with the new settings
	}
	select {
	case a.reconf <- struct{}{}:
	default:
	}
}

// linkInfo names the reflector and module the gateway links to, if any.
func (a *igate) linkInfo() string {
	if a.link == nil {
		return ""
	}
	return a.link()
}

// minTxGap returns the gap before a packet from APRS-IS.
func (a *igate) minTxGap() time.Duration {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.minTxGap
}

// count adds one to a traffic counter.
func (a *igate) count(name string) {
	a.mu.Lock()
	a.counts[name]++
	a.mu.Unlock()
}

// start connects to APRS-IS and keeps the gate's state current.
func (a *igate) start() {
	go a.run()
	go a.write()
	go a.tick()
}

func (a *igate) close() {
	a.closeOnce.Do(func() {
		close(a.done)
		a.mu.Lock()
		if a.conn != nil {
			a.conn.Close()
		}
		a.mu.Unlock()
		a.log.close()
	})
}

// qConstruct is qAR for a gate that also transmits, qAO for receive only.
func (a *igate) qConstruct() string {
	if a.cfg.isToRF || a.cfg.relay {
		return "qAR"
	}
	return "qAO"
}

// fromRF takes an APRS packet received on RF. It must not block: it runs
// on the decoder.
func (a *igate) fromRF(lsf *m17.LSF, payload []byte) {
	line, _, _ := strings.Cut(string(payload), "\x00")
	m17Src := lsf.Src.Callsign()
	now := a.now()
	t, err := parseTNC2(line)
	if err != nil {
		log.Printf("[DEBUG] APRS: bad line from %s on RF: %v", m17Src, err)
		a.log.write(now, aprsEvent{Dir: aprsFromRF, M17: m17Src, Line: line, Result: "bad line"})
		return
	}
	a.mu.Lock()
	a.counts["rfHeard"]++
	// A packet another gate brought from APRS-IS, or a gateway relayed
	// (marked by Extended Callsign Data), is on the air, but its source is
	// not a station heard here.
	if !t.hasPathElement("TCPIP", "TCPXX") && lsf.ECD() == nil {
		call := strings.ToUpper(t.src)
		if a.heard[call] == nil {
			log.Printf("[INFO] APRS: heard %s on RF from M17 %s", call, lsf.Src.Callsign())
			a.heard[call] = &heardStation{}
			a.updateFilterLocked()
		}
		*a.heard[call] = heardStation{m17Call: lsf.Src.Callsign(), lsfType: lsf.Type, last: now}
	}
	a.onAir[t.key()] = now
	if m, ok := parseMessage(t); ok && m.ack && a.queue != nil {
		a.queue.cancel(m.addressee, t.src, m.id)
	}
	if a.cfg.relay {
		a.relayLocked(lsf, t, now)
	}
	if !a.cfg.rfToIS {
		a.mu.Unlock()
		a.log.write(now, aprsEvent{Dir: aprsFromRF, M17: m17Src, Line: line, Result: "heard (RFtoIS off)"})
		return
	}
	out, reason := rfToIS(t, a.cfg.callsign, a.qConstruct())
	if reason == "" {
		k, _ := parseTNC2(out)
		if now.Sub(a.toIS[k.key()]) < rfDupeWindow {
			reason = dropDuplicate
		} else {
			a.toIS[k.key()] = now
		}
	}
	verified := a.verified
	switch {
	case reason != "":
		a.counts["rfNotGated"]++
	case verified:
		a.counts["rfGated"]++
	}
	a.mu.Unlock()
	ev := aprsEvent{Dir: aprsFromRF, M17: m17Src, Line: line, Out: out}
	switch {
	case reason != "":
		log.Printf("[DEBUG] APRS: not gating to APRS-IS (%s): %s", reason, line)
		ev.Result = "not gated: " + reason
	case !verified:
		log.Printf("[INFO] APRS: would gate to APRS-IS (receive-only login): %s", out)
		ev.Result = "would gate (receive only)"
	default:
		log.Printf("[INFO] APRS: RF to APRS-IS: %s", out)
		a.send(out)
		ev.Result = "gated"
	}
	a.log.write(now, ev)
}

// relayLocked queues a message heard on RF for another station heard
// here, so that two stations of a simplex hotspot that can't hear each
// other can still exchange messages. A message is held for relayHold, and
// dropped if its addressee acks it first, having heard it directly; an ack
// is relayed only for a message that was. Must be called with a.mu held.
func (a *igate) relayLocked(lsf *m17.LSF, t tnc2, now time.Time) {
	m, ok := parseMessage(t)
	if !ok || isGroupAddressee(m.addressee) || lsf.ECD() != nil || len(t.path) >= 8 ||
		t.hasPathElement("TCPIP", "TCPXX", a.cfg.callsign) {
		return
	}
	src, dst := strings.ToUpper(t.src), strings.ToUpper(m.addressee)
	st := a.heard[dst]
	if sameStation(src, dst) || sameStation(dst, a.cfg.callsign) || st == nil || now.Sub(st.last) > a.cfg.heardWindow {
		return
	}
	p := &gatedPacket{class: gatedMessage, key: "relay " + t.key(), msg: m, from: src, queued: now,
		notBefore: now.Add(a.cfg.relayHold)}
	if m.ack {
		if now.Sub(a.relayed[dst+">"+src+"{"+m.id]) > relayAckWindow {
			return // they hear each other
		}
		p.class, p.notBefore = gatedAck, time.Time{}
	} else if m.id != "" {
		k := src + ">" + dst + "{" + m.id
		p.key = "relay " + k
		p.sent = func() {
			a.mu.Lock()
			a.relayed[k] = a.now()
			a.mu.Unlock()
		}
	}
	out := t
	out.path = append(append([]string(nil), t.path...), a.cfg.callsign+"*")
	p.line = out.String()
	// Repeated as a duplex gateway repeats: the sender's LSF, marked with
	// the gateway's callsign, on the addressee's CAN.
	l := *lsf
	l.Type[0] = l.Type[0]&^0x07 | st.lsfType[0]&0x07
	l.Type[1] = l.Type[1]&^0x80 | st.lsfType[1]&0x80
	l.SetECD(a.m17Enc, nil)
	p.packet = m17.Packet{LSF: &l, Type: m17.PacketTypeAPRS, Payload: append([]byte(p.line), 0)}
	p.packet.CalcCRC()
	a.queue.push(p)
	a.counts["relayQueued"]++
	a.log.write(now, aprsEvent{Dir: aprsFromRF, M17: lsf.Src.Callsign(), Line: t.String(),
		Result: "relay queued (" + p.class.String() + ")"})
}

// fromIS takes a packet from APRS-IS.
func (a *igate) fromIS(line string) {
	now := a.now()
	t, err := parseTNC2(line)
	if err != nil {
		log.Printf("[DEBUG] APRS: bad line from APRS-IS: %v", err)
		a.log.write(now, aprsEvent{Dir: aprsFromIS, Line: line, Result: "bad line"})
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.counts["isReceived"]++
	if pos, ok := packetPosition(t); ok {
		a.lastPosition[strings.ToUpper(t.src)] = posAt{pos, now}
	}
	p, reason := a.selectForRF(t, now)
	switch {
	case reason == dropWatched:
		a.counts["isWatched"]++
		a.log.write(now, aprsEvent{Dir: aprsFromIS, Line: line, Result: "watched"})
		return
	case reason != "":
		a.counts["isNotSent"]++
		log.Printf("[DEBUG] APRS: not gating to RF (%s): %s", reason, line)
		a.log.write(now, aprsEvent{Dir: aprsFromIS, Line: line, Result: "not sent: " + reason})
		return
	case !a.cfg.isToRF:
		a.counts["isWatched"]++
		a.log.write(now, aprsEvent{Dir: aprsFromIS, Line: line, Result: "watched (IStoRF off)"})
		return
	}
	a.counts["isQueued"]++
	a.log.write(now, aprsEvent{Dir: aprsFromIS, Line: line, Result: "queued " + p.class.String()})
	a.queue.push(p)
	switch {
	case p.class == gatedMessage && a.cfg.courtesy:
		a.courtesyFor(p.from, p.packet.LSF.Type, now)
	case p.class == gatedOther:
		// Messages and acks are left out, so that a sender's retry goes
		// even if the addressee missed the first copy.
		a.onAir[t.key()] = now
	}
}

// selectForRF applies the APRS-IS to RF rules to t and returns it ready to
// queue. Must be called with a.mu held.
func (a *igate) selectForRF(t tnc2, now time.Time) (*gatedPacket, string) {
	src := strings.ToUpper(t.src)
	if sameStation(src, a.cfg.callsign) {
		return nil, dropOwn
	}
	if t.hasPathElement(noGateRF...) {
		return nil, dropNoGate
	}
	if now.Sub(a.onAir[t.key()]) < onAirWindow {
		return nil, dropOnAir
	}
	p := &gatedPacket{class: gatedOther, key: t.key(), from: src, queued: now}
	var lsfType [2]byte
	m, isMsg := parseMessage(t)
	isPos := !isMsg && isPosition(t)
	switch {
	case isMsg && !isGroupAddressee(m.addressee):
		if sameStation(m.addressee, a.cfg.callsign) {
			return nil, dropOwn
		}
		st := a.heard[strings.ToUpper(m.addressee)]
		if st == nil || now.Sub(st.last) > a.cfg.heardWindow {
			// Not for a station here: only the operator's rules want it,
			// and then as any other traffic they bring.
			if r := a.watchedLocked(t, now); r != "" {
				return nil, r
			}
			break
		}
		if s := a.heard[src]; s != nil && now.Sub(s.last) < localSenderWindow {
			return nil, dropSenderLocal
		}
		p.class, p.msg, lsfType = gatedMessage, m, st.lsfType
		if m.ack {
			p.class = gatedAck
		}
		// A sender's retry replaces the copy still queued.
		p.key = src + ">" + m.addressee + "{" + m.id
		if m.id == "" {
			p.key = t.key()
		}
	case isPos && !a.courtesyDue[src].IsZero():
		lsfType = a.courtesyLSF[src]
		delete(a.courtesyDue, src)
		delete(a.courtesyLSF, src)
		a.courtesySent[src] = now
		a.updateFilterLocked()
	default:
		if isPos {
			a.positions[src], a.positionTime[src] = t.String(), now
		}
		if r := a.watchedLocked(t, now); r != "" {
			return nil, r
		}
	}
	line, reason := isToRFLine(t, a.cfg.callsign, aprsToCall, a.cfg.wrap)
	if reason != "" {
		return nil, reason
	}
	pkt, err := a.rfPacket(line, lsfType)
	if err != nil {
		log.Printf("[ERROR] APRS: building packet: %v", err)
		return nil, dropMalformed
	}
	p.line, p.packet = line, *pkt
	return p, ""
}

// watchedLocked applies the operator's rules to traffic the iGate rules
// don't transmit: "" to transmit it, or the reason not to. Must be called
// with a.mu held.
func (a *igate) watchedLocked(t tnc2, now time.Time) string {
	switch a.watchActionFor(t, now) {
	case watchRF:
		// A local station's packets are on the air already.
		if s := a.heard[strings.ToUpper(t.src)]; s != nil && now.Sub(s.last) < a.cfg.heardWindow {
			return dropSenderLocal
		}
		return ""
	case watchDashboard:
		return dropWatched
	}
	return dropNotForRF
}

// rfPacket builds the M17 packet for line, broadcast from the gateway, on
// the CAN taken from lsfType.
func (a *igate) rfPacket(line string, lsfType [2]byte) (*m17.Packet, error) {
	p, err := m17.NewPacket(m17.DestinationAll, a.m17Call, m17.PacketTypeAPRS, append([]byte(line), 0))
	if err != nil {
		return nil, err
	}
	// CAN is TYPE bits 7-10, astride the two bytes.
	p.LSF.Type[0] = p.LSF.Type[0]&^0x07 | lsfType[0]&0x07
	p.LSF.Type[1] = p.LSF.Type[1]&^0x80 | lsfType[1]&0x80
	p.LSF.CalcCRC()
	return p, nil
}

// courtesyFor sends src's position to RF after a message from it: the one
// last seen, or else the next to arrive, which the filter then asks for.
// Must be called with a.mu held.
func (a *igate) courtesyFor(src string, lsfType [2]byte, now time.Time) {
	if now.Sub(a.courtesySent[src]) < courtesyInterval {
		return
	}
	if line, ok := a.positions[src]; ok && now.Sub(a.positionTime[src]) < a.cfg.heardWindow {
		t, _ := parseTNC2(line)
		rf, reason := isToRFLine(t, a.cfg.callsign, aprsToCall, a.cfg.wrap)
		if pkt, err := a.rfPacket(rf, lsfType); reason == "" && err == nil {
			a.queue.push(&gatedPacket{class: gatedOther, key: t.key(), from: src, line: rf, packet: *pkt, queued: now})
			a.courtesySent[src] = now
			return
		}
	}
	a.courtesyDue[src] = now
	a.courtesyLSF[src] = lsfType
	a.updateFilterLocked()
}

// buildFilter returns the server filter: messages to the stations heard on
// RF (and to the gate, so that it is never empty), positions of senders
// owed a courtesy position, the operator's rules in force, and the raw
// Filter. Must be called with a.mu held, or before the gate starts.
func (a *igate) buildFilter() string {
	f := "g/" + strings.Join(append([]string{a.cfg.callsign}, slices.Sorted(maps.Keys(a.heard))...), "/")
	if len(a.courtesyDue) > 0 {
		f += " b/" + strings.Join(slices.Sorted(maps.Keys(a.courtesyDue)), "/")
	}
	now := a.now()
	for _, w := range a.cfg.watches {
		if sf := w.serverFilter(a.cfg); sf != "" && w.active(now) {
			f += " " + sf
		}
	}
	if a.cfg.filter != "" {
		f += " " + a.cfg.filter
	}
	return f
}

// updateFilterLocked sends the server a new filter if it has changed.
// Must be called with a.mu held.
func (a *igate) updateFilterLocked() {
	f := a.buildFilter()
	if f == a.filter {
		return
	}
	a.filter = f
	if a.conn != nil {
		log.Printf("[DEBUG] APRS: filter %s", f)
		a.send("#filter " + f)
	}
}

// expire forgets what is older than it needs to be kept.
func (a *igate) expire(now time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for call, st := range a.heard {
		if now.Sub(st.last) > a.cfg.heardWindow {
			log.Printf("[INFO] APRS: %s not heard on RF for %v", call, a.cfg.heardWindow)
			delete(a.heard, call)
		}
	}
	for src, t := range a.courtesyDue {
		if now.Sub(t) > a.cfg.heardWindow {
			delete(a.courtesyDue, src)
			delete(a.courtesyLSF, src)
		}
	}
	for _, m := range []map[string]time.Time{a.onAir, a.toIS} {
		maps.DeleteFunc(m, func(_ string, t time.Time) bool { return now.Sub(t) > onAirWindow })
	}
	maps.DeleteFunc(a.courtesySent, func(_ string, t time.Time) bool { return now.Sub(t) > courtesyInterval })
	maps.DeleteFunc(a.relayed, func(_ string, t time.Time) bool { return now.Sub(t) > relayAckWindow })
	for src, t := range a.positionTime {
		if now.Sub(t) > a.cfg.heardWindow {
			delete(a.positionTime, src)
			delete(a.positions, src)
		}
	}
	maps.DeleteFunc(a.lastPosition, func(_ string, p posAt) bool { return now.Sub(p.at) > a.cfg.heardWindow })
	a.updateFilterLocked()
}

// send queues a line for APRS-IS without waiting.
func (a *igate) send(line string) {
	select {
	case a.out <- line:
	default:
		log.Printf("[ERROR] APRS: APRS-IS send queue full; dropping %s", line)
	}
}

// write sends queued lines to APRS-IS.
func (a *igate) write() {
	for {
		select {
		case line := <-a.out:
			a.mu.Lock()
			conn := a.conn
			a.mu.Unlock()
			if conn == nil {
				log.Printf("[INFO] APRS: not connected to APRS-IS; dropping %s", line)
				continue
			}
			if err := conn.SendLine(line); err != nil {
				log.Printf("[INFO] APRS: sending to APRS-IS: %v", err)
			}
		case <-a.done:
			return
		}
	}
}

// run keeps a connection to APRS-IS and reads it until the gate closes.
func (a *igate) run() {
	delay := aprsReconnectMin
	for {
		a.mu.Lock()
		cfg, filter, verified := a.cfg, a.filter, a.verified
		a.mu.Unlock()
		if !cfg.enabled {
			select {
			case <-a.done:
				return
			case <-a.reconf:
			}
			continue
		}
		conn, err := fap.Dial(cfg.server, cfg.callsign, cfg.passcode, aprsAppName, aprsAppVersion, filter)
		if err != nil {
			log.Printf("[INFO] APRS: unable to log in to %s: %v", cfg.server, err)
		} else {
			login := "receive only"
			if verified {
				login = "verified"
			}
			log.Printf("[INFO] APRS: logged in to %s as %s (%s), filter %q", cfg.server, cfg.callsign, login, filter)
			delay = aprsReconnectMin
			a.mu.Lock()
			a.conn = conn
			a.connectedAt = a.now()
			a.server, a.serverLine = "", ""
			a.mu.Unlock()
			select {
			case <-a.done:
				conn.Close()
				return
			default:
			}
			a.beacon()
			for {
				line, err := conn.ReadLine(aprsReadTimeout)
				if err != nil {
					log.Printf("[INFO] APRS: lost APRS-IS connection: %v", err)
					break
				}
				if strings.HasPrefix(line, "#") {
					a.serverNote(line)
					continue
				}
				a.fromIS(line)
			}
			a.mu.Lock()
			a.conn = nil
			a.mu.Unlock()
			conn.Close()
		}
		select {
		case <-a.done:
			return
		case <-a.reconf:
			delay = aprsReconnectMin
			continue
		case <-time.After(delay):
		}
		delay = min(2*delay, aprsReconnectMax)
	}
}

// serverNote keeps a comment line from the server, for the status file:
// its keepalive names it, such as "# aprsc 2.1.14-g5e22b37 11 Oct 2026
// 14:38:08 GMT T2RDU 66.226.147.251:14580".
func (a *igate) serverNote(line string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.serverLine, a.serverSeen = line, a.now()
	f := strings.Fields(line)
	if len(f) >= 3 && strings.Contains(f[len(f)-1], ":") {
		a.server = f[len(f)-2]
	}
	if _, after, ok := strings.Cut(line, " server "); ok && strings.HasPrefix(line, "# logresp") {
		a.server = strings.TrimSpace(after)
	}
}

// tick expires state, beacons when due, and writes the status file, until
// the gate closes.
func (a *igate) tick() {
	t := time.NewTicker(aprsStatusEvery)
	defer t.Stop()
	lastExpire := a.now()
	for {
		select {
		case <-t.C:
			now := a.now()
			if now.Sub(lastExpire) >= aprsTick {
				a.expire(now)
				lastExpire = now
			}
			a.mu.Lock()
			due := a.cfg.beaconInterval > 0 && now.Sub(a.lastBeacon) >= a.cfg.beaconInterval
			a.mu.Unlock()
			if due {
				a.beacon()
			}
			a.writeStatus()
		case <-a.done:
			return
		}
	}
}

// beacon sends the gate's position to APRS-IS, not RF, when one is set
// and the login is verified.
func (a *igate) beacon() {
	a.mu.Lock()
	c, verified, connected := a.cfg, a.verified, a.conn != nil
	if !c.hasPosition || c.beaconInterval <= 0 || !verified || !connected {
		a.mu.Unlock()
		return
	}
	a.lastBeacon = a.now()
	a.mu.Unlock()
	line, err := beaconLine(c, a.radio, a.linkInfo())
	if err != nil {
		log.Printf("[ERROR] APRS: beacon: %v", err)
		return
	}
	log.Printf("[DEBUG] APRS: beacon %s", line)
	a.send(line)
	a.mu.Lock()
	a.beaconLine = line
	a.counts["beacons"]++
	a.mu.Unlock()
	a.log.write(a.now(), aprsEvent{Dir: aprsToIS, Line: line, Result: "beacon"})
}
