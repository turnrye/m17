package main

import (
	"log"
	"math"
	"sync"
	"time"

	"github.com/jancona/m17"
)

// gatedClass orders APRS-IS packets waiting for RF: acks first, since a
// sender retries until one arrives, then messages, then everything else.
type gatedClass int

const (
	gatedAck gatedClass = iota
	gatedMessage
	gatedOther
	gatedClasses
)

func (c gatedClass) String() string {
	return [...]string{"ack", "message", "other"}[c]
}

// gatedClassLimit bounds each class; the oldest packet in a full class is
// dropped for a new one.
var gatedClassLimit = [gatedClasses]int{16, 16, 8}

// gatedPacket is a packet from APRS-IS, or a relay, waiting to be
// transmitted.
type gatedPacket struct {
	class     gatedClass
	key       string // a queued packet with the same key is replaced
	msg       aprsMessage
	from      string
	line      string
	packet    m17.Packet
	queued    time.Time
	notBefore time.Time // held until then; zero to send when it can
	sent      func()    // optional; called once it has been transmitted
}

// gatedQueue holds APRS-IS packets for RF by class, drops them when they
// are older than maxAge, and limits the rate they are sent at with a token
// bucket of burst tokens that refills at rate per minute.
type gatedQueue struct {
	mu       sync.Mutex
	classes  [gatedClasses][]*gatedPacket
	maxAge   time.Duration
	rate     float64 // tokens per second
	burst    float64
	tokens   float64
	lastFill time.Time
	wake     chan struct{}
	dropped  func(p *gatedPacket, why string) // optional; called with mu held
}

func newGatedQueue(maxAge time.Duration, perMinute, burst int, now time.Time) *gatedQueue {
	return &gatedQueue{
		maxAge:   maxAge,
		rate:     float64(perMinute) / 60,
		burst:    float64(burst),
		tokens:   float64(burst),
		lastFill: now,
		wake:     make(chan struct{}, 1),
	}
}

// push queues p, replacing a queued packet with the same key.
func (q *gatedQueue) push(p *gatedPacket) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.classes[p.class]
	for i, old := range c {
		if old.key == p.key {
			log.Printf("[DEBUG] APRS: replacing queued %s %s", p.class, old.line)
			q.drop(old, "replaced by a newer copy")
			c = append(c[:i], c[i+1:]...)
			break
		}
	}
	if len(c) >= gatedClassLimit[p.class] {
		log.Printf("[INFO] APRS: %s queue full, dropping %s", p.class, c[0].line)
		q.drop(c[0], "dropped: queue full")
		c = c[1:]
	}
	q.classes[p.class] = append(c, p)
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// cancel drops queued messages from "from" to "to" numbered id: to has
// acked or rejected them on RF.
func (q *gatedQueue) cancel(from, to, id string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	c := q.classes[gatedMessage][:0]
	for _, p := range q.classes[gatedMessage] {
		if sameStation(p.from, from) && sameStation(p.msg.addressee, to) && p.msg.id == id {
			log.Printf("[DEBUG] APRS: %s acked on RF, dropping queued %s", to, p.line)
			q.drop(p, "dropped: acked on RF")
			continue
		}
		c = append(c, p)
	}
	q.classes[gatedMessage] = c
}

// drop reports a packet leaving the queue unsent. Must be called with
// q.mu held.
func (q *gatedQueue) drop(p *gatedPacket, why string) {
	if q.dropped != nil {
		q.dropped(p, why)
	}
}

// len reports how many packets are queued.
func (q *gatedQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	n := 0
	for _, c := range q.classes {
		n += len(c)
	}
	return n
}

// next returns the packet to send at now and takes a token for it. With
// none to send it returns how long until one is due, for a held packet or
// a token, or zero if the queue is empty. Packets older than maxAge are
// dropped on the way.
func (q *gatedQueue) next(now time.Time) (*gatedPacket, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.tokens = min(q.burst, q.tokens+now.Sub(q.lastFill).Seconds()*q.rate)
	q.lastFill = now
	var p *gatedPacket
	var at int
	var held time.Duration
	for i := range q.classes {
		c := q.classes[i][:0]
		for _, e := range q.classes[i] {
			if now.Sub(e.queued) > q.maxAge {
				log.Printf("[INFO] APRS: %s waited over %v, dropping %s", e.class, q.maxAge, e.line)
				q.drop(e, "expired after "+q.maxAge.String())
				continue
			}
			c = append(c, e)
		}
		q.classes[i] = c
		for j, e := range c {
			if p != nil {
				break
			}
			if wait := e.notBefore.Sub(now); wait > 0 {
				if held == 0 || wait < held {
					held = wait
				}
				continue
			}
			p, at = e, j
		}
	}
	if p == nil {
		return nil, held
	}
	if q.tokens < 1 {
		return nil, time.Duration(math.Ceil((1 - q.tokens) / q.rate * float64(time.Second)))
	}
	q.tokens--
	c := q.classes[p.class]
	q.classes[p.class] = append(c[:at:at], c[at+1:]...)
	return p, 0
}
