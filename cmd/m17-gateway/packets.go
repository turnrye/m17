package main

import (
	"log"
	"time"

	"github.com/jancona/m17"
)

// Packets the gateway transmits (from the reflector, and its own /ECHO and
// /INFO replies) go through one queue and one sender, so that:
//
//   - a packet never starts while a radio is transmitting: it waits until
//     no RF frame has been received for rxHoldoff (a radio that is
//     transmitting cannot hear it, and stopping RX to transmit would cut
//     that radio off);
//   - a packet starts at least packetGap after the gateway's own last
//     transmission, a packet or a voice frame (from the reflector, or its
//     local replies); and
//   - only one goroutine calls the modem to transmit a packet.
//
// The channel is timed from the last frame received or sent, not from the
// gateway state, because a stream whose EOT is missed leaves the state
// stuck until the next transmission; a timer cannot stick.

// packetQueueLen bounds packets waiting for the channel; more are dropped.
const packetQueueLen = 64

// channelPoll is how often a waiting packet rechecks the channel.
const channelPoll = 50 * time.Millisecond

// rfTurnaround is how long reflector voice waits after a local stream ends
// (EOT, last frame, or decoder timeout): time for the radio to switch back
// to receive. Waiting the full rxHoldoff would cut off the start of a quick
// reply, such as a parrot playback. RF that stops without ending a stream
// still waits rxHoldoff.
const rfTurnaround = 100 * time.Millisecond

// rxHeard notes a frame decoded from the receiver.
func (g *Gateway) rxHeard() {
	g.stateMutex.Lock()
	g.lastRX = time.Now()
	g.rxEnded = false
	g.stateMutex.Unlock()
}

// rxStreamEnded notes the end of a received stream. The end can be
// reported twice, by the last frame and then by the decoder after that
// frame has been repeated; the first report sets the time.
func (g *Gateway) rxStreamEnded() {
	g.stateMutex.Lock()
	if !g.rxEnded {
		g.lastRX = time.Now()
		g.rxEnded = true
	}
	g.stateMutex.Unlock()
}

// localRFActive reports whether a local radio is transmitting, or has only
// just stopped: an RF frame was received within rxHoldoff, or within
// rfTurnaround if that frame ended the stream.
func (g *Gateway) localRFActive() bool {
	g.stateMutex.Lock()
	defer g.stateMutex.Unlock()
	wait := g.rxHoldoff
	if g.rxEnded {
		wait = min(wait, rfTurnaround)
	}
	return time.Since(g.lastRX) < wait
}

// voiceSent notes a voice frame the gateway transmitted (from the
// reflector, or its own replies).
func (g *Gateway) voiceSent() {
	g.stateMutex.Lock()
	g.lastTX = time.Now()
	g.stateMutex.Unlock()
}

// queuePacket queues a packet from the reflector for transmission.
func (g *Gateway) queuePacket(p m17.Packet) error {
	return g.enqueue(func() error { return g.transmitNetPacket(p) }, p)
}

// queueLocalPacket queues a packet the gateway built itself, sent as is.
func (g *Gateway) queueLocalPacket(p m17.Packet) error {
	return g.enqueue(func() error { return g.modem.TransmitPacket(p) }, p)
}

func (g *Gateway) enqueue(send func() error, p m17.Packet) error {
	g.tryEnqueue(send, p)
	return nil
}

// tryEnqueue queues send, reporting whether there was room.
func (g *Gateway) tryEnqueue(send func() error, p m17.Packet) bool {
	select {
	case g.packetQueue <- send:
		return true
	default:
		log.Printf("[ERROR] Packet queue full; dropping packet %s", p)
		return false
	}
}

// sendPackets transmits queued packets one at a time, each when the channel
// is clear, until the queue is closed.
func (g *Gateway) sendPackets() {
	for send := range g.packetQueue {
		g.waitForChannel()
		if err := send(); err != nil {
			log.Printf("[ERROR] Error transmitting packet: %v", err)
		}
		g.packetSent()
	}
}

// packetSent notes the end of a packet the gateway transmitted.
func (g *Gateway) packetSent() {
	g.stateMutex.Lock()
	g.lastTX = time.Now()
	g.stateMutex.Unlock()
}

// Packets from APRS-IS (aprs.go) wait in a gatedQueue, and are passed to
// the packet sender above one at a time, so they share its channel checks,
// and so that reflector and local packets go first. A gated packet also
// waits until the channel has been quiet, both ways, for minTxGap: long
// enough for a local station to key up, or answer the last over, before
// it. A burst of gated packets therefore leaves a gap after each one.

// sendGated passes gated packets to the packet sender until stop closes.
func (g *Gateway) sendGated(q *gatedQueue, minTxGap time.Duration, stop <-chan struct{}) {
	for {
		if q.len() == 0 {
			select {
			case <-q.wake:
			case <-stop:
				return
			}
			continue
		}
		wait := g.gatedWait(time.Now(), minTxGap)
		if wait <= 0 && len(g.packetQueue) > 0 {
			wait = channelPoll
		}
		if wait <= 0 {
			var p *gatedPacket
			p, wait = q.next(time.Now())
			if p != nil {
				g.transmitGated(p, minTxGap)
				continue
			}
			if wait <= 0 {
				continue // the rest had expired
			}
		}
		select {
		case <-time.After(wait):
		case <-stop:
			return
		}
	}
}

// transmitGated sends p through the packet sender and waits until it has
// gone, so that the next one is timed from its end.
func (g *Gateway) transmitGated(p *gatedPacket, gap time.Duration) {
	done := make(chan struct{})
	send := func() error {
		defer close(done)
		// A packet queued just ahead of this one may have gone since it
		// was handed over.
		for wait := g.gatedWait(time.Now(), gap); wait > 0; wait = g.gatedWait(time.Now(), gap) {
			time.Sleep(min(wait, channelPoll))
		}
		waited := time.Since(p.queued).Round(time.Millisecond)
		log.Printf("[INFO] APRS-IS to RF (%s, queued %v): %s", p.class, waited, p.line)
		err := g.modem.TransmitPacket(p.packet)
		g.packetSent()
		if g.aprs != nil {
			result := "sent after " + waited.String()
			if err != nil {
				result = "error: " + err.Error()
			}
			g.aprs.log.write(time.Now(), aprsEvent{Dir: aprsToRF, Line: p.line, Result: result, Class: p.class.String()})
		}
		return err
	}
	if g.tryEnqueue(send, p.packet) {
		<-done
	}
}

// gatedWait reports how long a gated packet must still wait at now: until
// the channel is clear for any packet, and nothing has been received or
// sent for gap.
func (g *Gateway) gatedWait(now time.Time, gap time.Duration) time.Duration {
	wait := g.channelWait(now)
	g.stateMutex.Lock()
	defer g.stateMutex.Unlock()
	last := g.lastRX
	if g.lastTX.After(last) {
		last = g.lastTX
	}
	return max(wait, last.Add(gap).Sub(now))
}

// waitForChannel returns once the channel is clear (see channelWait).
func (g *Gateway) waitForChannel() {
	logged := false
	for {
		wait := g.channelWait(time.Now())
		if wait <= 0 {
			return
		}
		if !logged {
			log.Printf("[DEBUG] Packet waiting for the channel")
			logged = true
		}
		time.Sleep(min(wait, channelPoll))
	}
}

// channelWait reports how long a packet must still wait at now; zero or
// less means it may go.
func (g *Gateway) channelWait(now time.Time) time.Duration {
	g.stateMutex.Lock()
	defer g.stateMutex.Unlock()
	return max(g.lastRX.Add(g.rxHoldoff).Sub(now), g.lastTX.Add(g.packetGap).Sub(now))
}
