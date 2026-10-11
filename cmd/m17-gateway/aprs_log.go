package main

import (
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

// The APRS traffic log holds, one JSON object per line, every APRS line
// heard on RF or received from APRS-IS with what the gate did with it, and
// what it sent, so that a dashboard can show the raw traffic both ways. It
// is a file of its own, so that busy APRS-IS traffic can't push voice out
// of the dashboard log's last-heard, and is rotated at aprsLogMax, keeping
// one old file.

const aprsLogMax = 4 << 20

// Directions in the APRS traffic log.
const (
	aprsFromRF = "RF"         // heard on RF
	aprsFromIS = "APRS-IS"    // received from APRS-IS
	aprsToRF   = "RF TX"      // transmitted, or dropped from the queue
	aprsToIS   = "APRS-IS TX" // the gate's own beacon
)

// aprsEvent is one entry of the APRS traffic log.
type aprsEvent struct {
	Time   string `json:"time"`
	Dir    string `json:"dir"`
	M17    string `json:"m17,omitempty"` // the M17 source a line was heard from
	Line   string `json:"line"`
	Result string `json:"result"`
	Out    string `json:"out,omitempty"`   // the line as gated to APRS-IS
	Class  string `json:"class,omitempty"` // queue class, for RF TX
}

// aprsLog writes the APRS traffic log. A nil *aprsLog writes nothing.
type aprsLog struct {
	mu   sync.Mutex
	path string
	f    *os.File
	size int64
}

func openAPRSLog(path string) (*aprsLog, error) {
	l := &aprsLog{path: path}
	if err := l.open(); err != nil {
		return nil, err
	}
	return l, nil
}

// open opens the log for appending. Must be called with l.mu held.
func (l *aprsLog) open() error {
	f, err := os.OpenFile(l.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	l.f, l.size = f, st.Size()
	return nil
}

// write appends ev, stamped with now.
func (l *aprsLog) write(now time.Time, ev aprsEvent) {
	if l == nil {
		return
	}
	ev.Time = now.Format(time.RFC3339Nano)
	b, err := json.Marshal(ev)
	if err != nil {
		return
	}
	b = append(b, '\n')
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return
	}
	if l.size+int64(len(b)) > aprsLogMax {
		l.f.Close()
		l.f = nil
		if err := os.Rename(l.path, l.path+".1"); err != nil {
			log.Printf("[ERROR] APRS: rotating %s: %v", l.path, err)
		}
		if err := l.open(); err != nil {
			log.Printf("[ERROR] APRS: reopening %s: %v", l.path, err)
			return
		}
	}
	n, err := l.f.Write(b)
	l.size += int64(n)
	if err != nil {
		log.Printf("[ERROR] APRS: writing %s: %v", l.path, err)
	}
}

func (l *aprsLog) close() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		l.f.Close()
		l.f = nil
	}
}
