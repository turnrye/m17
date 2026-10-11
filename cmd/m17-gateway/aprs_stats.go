package main

import (
	"maps"
	"sync"
	"time"
)

// aprsStats counts what the gate does, for the status file: running totals,
// and what happened in the last statWindow to the packets bound for RF, by
// watch rule, so a dashboard can say which rule brings more than the
// channel can carry. It has its own lock, so that the queue can report a
// drop while the gate's is held.
type aprsStats struct {
	mu     sync.Mutex
	totals map[string]int
	recent []statEvent
}

// statWindow is how far back the recent counts go.
const statWindow = 15 * time.Minute

// statEvent is something that happened to a packet bound for RF.
type statEvent struct {
	at    time.Time
	kind  string // matched, skipped, sent, dropped or replaced
	rule  string // the watch rule that brought it, if any
	class gatedClass
}

// ruleStat is what a watch rule brought in the last statWindow.
type ruleStat struct {
	Matched int `json:"matched"`
	Sent    int `json:"sent"`
	Dropped int `json:"dropped"`
	Skipped int `json:"skipped"`
}

// recentStat is what happened to packets for RF in the last statWindow.
type recentStat struct {
	Minutes          int `json:"minutes"`
	Sent             int `json:"sent"`
	Dropped          int `json:"dropped"`          // traffic for nobody in particular
	DroppedAddressed int `json:"droppedAddressed"` // acks and messages
	Replaced         int `json:"replaced"`         // by a newer report
}

func newAPRSStats() *aprsStats {
	return &aprsStats{totals: map[string]int{}}
}

// add adds one to a running total.
func (s *aprsStats) add(name string) {
	s.mu.Lock()
	s.totals[name]++
	s.mu.Unlock()
}

// get returns a running total.
func (s *aprsStats) get(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totals[name]
}

// event notes something that happened to a packet bound for RF at at.
func (s *aprsStats) event(at time.Time, kind, rule string, class gatedClass) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(at)
	s.recent = append(s.recent, statEvent{at, kind, rule, class})
}

// prune forgets events older than statWindow. Must be called with s.mu held.
func (s *aprsStats) prune(now time.Time) {
	i := 0
	for i < len(s.recent) && now.Sub(s.recent[i].at) > statWindow {
		i++
	}
	s.recent = s.recent[i:]
}

// snapshot returns the totals, the recent counts by rule, and the recent
// counts overall, at now.
func (s *aprsStats) snapshot(now time.Time) (map[string]int, map[string]ruleStat, recentStat) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(now)
	rules := map[string]ruleStat{}
	recent := recentStat{Minutes: int(statWindow / time.Minute)}
	for _, e := range s.recent {
		r := rules[e.rule]
		switch e.kind {
		case "matched":
			r.Matched++
		case "skipped":
			r.Skipped++
		case "sent":
			r.Sent++
			recent.Sent++
		case "dropped":
			r.Dropped++
			if e.class == gatedOther {
				recent.Dropped++
			} else {
				recent.DroppedAddressed++
			}
		case "replaced":
			recent.Replaced++
		}
		if e.rule != "" {
			rules[e.rule] = r
		}
	}
	return maps.Clone(s.totals), rules, recent
}
