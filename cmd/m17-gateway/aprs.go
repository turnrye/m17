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
	beaconInterval time.Duration
	hasPosition    bool
	latitude       float64
	longitude      float64
	symbol         string
	comment        string
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
		beaconInterval: s.Key("BeaconInterval").MustDuration(30 * time.Minute),
		symbol:         s.Key("Symbol").MustString("I&"),
		comment:        s.Key("Comment").String(),
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
	queue    *gatedQueue // nil unless IS to RF is on
	now      func() time.Time

	mu           sync.Mutex
	heard        map[string]*heardStation // by APRS callsign
	onAir        map[string]time.Time     // packet keys heard on RF or sent there
	toIS         map[string]time.Time     // packet keys gated to APRS-IS
	positions    map[string]string        // latest APRS-IS position line by source
	positionTime map[string]time.Time
	courtesyDue  map[string]time.Time // senders whose next position goes to RF
	courtesySent map[string]time.Time
	courtesyLSF  map[string][2]byte
	filter       string // the server filter last asked for
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
		out:          make(chan string, 64),
		done:         make(chan struct{}),
	}
	if cfg.isToRF {
		a.queue = newGatedQueue(cfg.maxQueueAge, cfg.rateLimit, cfg.rateBurst, time.Now())
	}
	a.filter = a.buildFilter()
	return a
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
	})
}

// qConstruct is qAR for a gate that also transmits, qAO for receive only.
func (a *igate) qConstruct() string {
	if a.cfg.isToRF {
		return "qAR"
	}
	return "qAO"
}

// fromRF takes an APRS packet received on RF. It must not block: it runs
// on the decoder.
func (a *igate) fromRF(lsf *m17.LSF, payload []byte) {
	line, _, _ := strings.Cut(string(payload), "\x00")
	t, err := parseTNC2(line)
	if err != nil {
		log.Printf("[DEBUG] APRS: bad line from %s on RF: %v", lsf.Src.Callsign(), err)
		return
	}
	now := a.now()
	a.mu.Lock()
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
	if !a.cfg.rfToIS {
		a.mu.Unlock()
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
	a.mu.Unlock()
	switch {
	case reason != "":
		log.Printf("[DEBUG] APRS: not gating to APRS-IS (%s): %s", reason, line)
	case !a.verified:
		log.Printf("[INFO] APRS: would gate to APRS-IS (receive-only login): %s", out)
	default:
		log.Printf("[INFO] APRS: RF to APRS-IS: %s", out)
		a.send(out)
	}
}

// fromIS takes a packet from APRS-IS.
func (a *igate) fromIS(line string) {
	t, err := parseTNC2(line)
	if err != nil {
		log.Printf("[DEBUG] APRS: bad line from APRS-IS: %v", err)
		return
	}
	if a.queue == nil {
		return
	}
	now := a.now()
	a.mu.Lock()
	defer a.mu.Unlock()
	p, reason := a.selectForRF(t, now)
	if reason != "" {
		log.Printf("[DEBUG] APRS: not gating to RF (%s): %s", reason, line)
		return
	}
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
			return nil, dropNotForRF
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
		if a.cfg.filter == "" {
			return nil, dropNotForRF
		}
		// The operator's own filter chose it. A local station's
		// packets are on the air already.
		if s := a.heard[src]; s != nil && now.Sub(s.last) < a.cfg.heardWindow {
			return nil, dropSenderLocal
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
// owed a courtesy position, and the operator's own filter. Must be called
// with a.mu held, or before the gate starts.
func (a *igate) buildFilter() string {
	f := "g/" + strings.Join(append([]string{a.cfg.callsign}, slices.Sorted(maps.Keys(a.heard))...), "/")
	if len(a.courtesyDue) > 0 {
		f += " b/" + strings.Join(slices.Sorted(maps.Keys(a.courtesyDue)), "/")
	}
	if a.cfg.filter != "" {
		f += " " + a.cfg.filter
	}
	return f
}

// updateFilterLocked sends the server a new filter if it has changed.
// Must be called with a.mu held.
func (a *igate) updateFilterLocked() {
	if !a.cfg.isToRF {
		return // nothing from APRS-IS is used
	}
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
	for src, t := range a.positionTime {
		if now.Sub(t) > a.cfg.heardWindow {
			delete(a.positionTime, src)
			delete(a.positions, src)
		}
	}
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
		filter := a.filter
		if !a.cfg.isToRF {
			filter = ""
		}
		a.mu.Unlock()
		conn, err := fap.Dial(a.cfg.server, a.cfg.callsign, a.cfg.passcode, aprsAppName, aprsAppVersion, filter)
		if err != nil {
			log.Printf("[INFO] APRS: unable to log in to %s: %v", a.cfg.server, err)
		} else {
			login := "receive only"
			if a.verified {
				login = "verified"
			}
			log.Printf("[INFO] APRS: logged in to %s as %s (%s), filter %q", a.cfg.server, a.cfg.callsign, login, filter)
			delay = aprsReconnectMin
			a.mu.Lock()
			a.conn = conn
			a.mu.Unlock()
			select {
			case <-a.done:
				conn.Close()
				return
			default:
			}
			a.beacon()
			for {
				line, err := conn.ReadPacket(aprsReadTimeout)
				if err != nil {
					log.Printf("[INFO] APRS: lost APRS-IS connection: %v", err)
					break
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
		case <-time.After(delay):
		}
		delay = min(2*delay, aprsReconnectMax)
	}
}

// tick expires state, and beacons, until the gate closes.
func (a *igate) tick() {
	expire := time.NewTicker(aprsTick)
	defer expire.Stop()
	var beacon <-chan time.Time
	if a.cfg.beaconInterval > 0 {
		t := time.NewTicker(a.cfg.beaconInterval)
		defer t.Stop()
		beacon = t.C
	}
	for {
		select {
		case <-expire.C:
			a.expire(a.now())
		case <-beacon:
			a.beacon()
		case <-a.done:
			return
		}
	}
}

// beacon sends the gate's position to APRS-IS, not RF, when one is set
// and the login is verified.
func (a *igate) beacon() {
	if !a.cfg.hasPosition || a.cfg.beaconInterval <= 0 || !a.verified {
		return
	}
	pos, err := fap.EncodePosition(a.cfg.latitude, a.cfg.longitude, nil, nil, nil, a.cfg.symbol,
		&fap.EncodePositionOpts{Comment: a.cfg.comment})
	if err != nil {
		log.Printf("[ERROR] APRS: beacon: %v", err)
		return
	}
	line := a.cfg.callsign + ">" + aprsToCall + ",TCPIP*:" + pos
	log.Printf("[DEBUG] APRS: beacon %s", line)
	a.send(line)
}
