package main

import (
	"fmt"
	"testing"
	"time"
)

func gated(class gatedClass, key string, at time.Time) *gatedPacket {
	return &gatedPacket{class: class, key: key, line: key, queued: at}
}

// TestGatedQueueOrder: acks go before messages, messages before the rest,
// and each class in the order queued.
func TestGatedQueueOrder(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Minute, 60, 10, now)
	q.push(gated(gatedOther, "pos", now))
	q.push(gated(gatedMessage, "msg1", now))
	q.push(gated(gatedAck, "ack", now))
	q.push(gated(gatedMessage, "msg2", now))
	var got []string
	for {
		p, _ := q.next(now)
		if p == nil {
			break
		}
		got = append(got, p.key)
	}
	if fmt.Sprint(got) != "[ack msg1 msg2 pos]" {
		t.Errorf("sent %v", got)
	}
}

// TestGatedQueueExpiry: a packet that has waited longer than maxAge is
// dropped, not sent.
func TestGatedQueueExpiry(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Minute, 60, 10, now)
	q.push(gated(gatedAck, "old", now))
	q.push(gated(gatedMessage, "new", now.Add(30*time.Second)))
	p, _ := q.next(now.Add(61 * time.Second))
	if p == nil || p.key != "new" {
		t.Fatalf("next = %+v, want new", p)
	}
	if q.len() != 0 {
		t.Errorf("%d left queued", q.len())
	}
}

// TestGatedQueueRate: after a burst, packets wait for the bucket to
// refill at the rate given.
func TestGatedQueueRate(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Hour, 6, 2, now)
	for i := range 4 {
		q.push(gated(gatedMessage, fmt.Sprint(i), now))
	}
	for range 2 {
		if p, _ := q.next(now); p == nil {
			t.Fatal("burst not allowed")
		}
	}
	p, wait := q.next(now)
	if p != nil || wait != 10*time.Second {
		t.Fatalf("next = %v, %v; want nil, 10s", p, wait)
	}
	if p, _ := q.next(now.Add(10 * time.Second)); p == nil {
		t.Error("no token after 10 s at 6 a minute")
	}
	if p, _ := q.next(now.Add(15 * time.Second)); p != nil {
		t.Error("token too soon")
	}
}

// TestGatedQueueReplace: a sender's retry replaces the queued copy, and a
// full class drops its oldest packet.
func TestGatedQueueReplace(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Hour, 60, 100, now)
	q.push(gated(gatedMessage, "W1AW>K0RET-7{01", now))
	retry := gated(gatedMessage, "W1AW>K0RET-7{01", now.Add(time.Second))
	q.push(retry)
	if q.len() != 1 {
		t.Fatalf("%d queued after a retry, want 1", q.len())
	}
	if p, _ := q.next(now); p != retry {
		t.Error("retry did not replace the queued copy")
	}
	for i := range gatedClassLimit[gatedOther] + 1 {
		q.push(gated(gatedOther, fmt.Sprint(i), now))
	}
	if p, _ := q.next(now); p == nil || p.key != "1" {
		t.Errorf("oldest kept after overflow: %+v", p)
	}
}

// TestGatedQueueCancel: an ack heard on RF drops the message it acks.
func TestGatedQueueCancel(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Hour, 60, 100, now)
	p := gated(gatedMessage, "a", now)
	p.from, p.msg = "W1AW", aprsMessage{addressee: "K0RET-7", id: "01"}
	q.push(p)
	q.cancel("W1AW", "K0RET-7", "02")
	if q.len() != 1 {
		t.Fatal("wrong message cancelled")
	}
	q.cancel("w1aw", "K0RET-7", "01")
	if q.len() != 0 {
		t.Error("acked message still queued")
	}
}

// TestGatedQueueHold: a held packet waits until its time without holding
// up the ones behind it.
func TestGatedQueueHold(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Hour, 60, 100, now)
	held := gated(gatedMessage, "held", now)
	held.notBefore = now.Add(5 * time.Second)
	q.push(held)
	q.push(gated(gatedMessage, "ready", now))
	if p, _ := q.next(now); p == nil || p.key != "ready" {
		t.Fatalf("next = %+v, want ready", p)
	}
	if p, wait := q.next(now); p != nil || wait != 5*time.Second {
		t.Fatalf("next = %+v, %v; want nil, 5s", p, wait)
	}
	if p, _ := q.next(now.Add(5 * time.Second)); p != held {
		t.Errorf("held packet not sent when due: %+v", p)
	}
}

// TestGatedQueueReserve: other traffic leaves a token for acks and
// messages, so one arriving after a burst of it goes at once.
func TestGatedQueueReserve(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Hour, 6, 3, now)
	for _, k := range []string{"a", "b", "c"} {
		q.push(gated(gatedOther, k, now))
	}
	for _, want := range []string{"a", "b"} {
		if p, _ := q.next(now); p == nil || p.key != want {
			t.Fatalf("next = %+v, want %s", p, want)
		}
	}
	if p, wait := q.next(now); p != nil || wait != 10*time.Second {
		t.Fatalf("third other = %+v, %v; want it to wait 10s", p, wait)
	}
	q.push(gated(gatedMessage, "msg", now))
	if p, _ := q.next(now); p == nil || p.key != "msg" {
		t.Errorf("message waited behind other traffic: %+v", p)
	}
	// With a burst of one there is nothing to reserve.
	q = newGatedQueue(time.Hour, 6, 1, now)
	q.push(gated(gatedOther, "a", now))
	if p, _ := q.next(now); p == nil {
		t.Error("other traffic never sent with RateBurst 1")
	}
}

// TestGatedQueueOtherAge: other traffic expires sooner than messages.
func TestGatedQueueOtherAge(t *testing.T) {
	now := time.Now()
	q := newGatedQueue(time.Minute, 60, 10, now)
	q.push(gated(gatedOther, "pos", now))
	q.push(gated(gatedMessage, "msg", now))
	later := now.Add(31 * time.Second)
	if p, _ := q.next(later); p == nil || p.key != "msg" {
		t.Fatalf("next = %+v, want msg", p)
	}
	if q.len() != 0 {
		t.Error("other traffic outlived otherMaxAge")
	}
}
