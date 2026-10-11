package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jancona/m17"
	"github.com/jancona/m17/inet"
	"gopkg.in/ini.v1"
)

// The reflector link can be changed over RF, as on g4klx's M17Gateway: a
// local station transmits to a reflector and module, such as "M17-M17 C",
// to link there, or to "UNLINK", and the gateway says what it did. The
// reflector in the configuration is home: once nothing has been received
// on RF for RevertAfter, the gateway links back to it, and a restart goes
// home too.

// linkPoll is how often the revert time is checked and the status written.
var linkPoll = 5 * time.Second

// linkConfig is what [Reflector] says about changing the link over RF.
type linkConfig struct {
	rfLinking   bool
	callsigns   []string // may change the link; empty for anyone
	revertAfter time.Duration
	statusPath  string
}

func loadLinkConfig(s *ini.Section) linkConfig {
	c := linkConfig{
		rfLinking:   s.Key("RFLinking").MustBool(false),
		revertAfter: s.Key("RevertAfter").MustDuration(15 * time.Minute),
		statusPath:  strings.TrimSpace(s.Key("LinkStatus").String()),
	}
	sep := func(r rune) bool { return r == ',' || r == ' ' }
	for _, f := range strings.FieldsFunc(s.Key("LinkCallsigns").String(), sep) {
		c.callsigns = append(c.callsigns, strings.ToUpper(f))
	}
	return c
}

// linkCommand is a link change asked for over RF.
type linkCommand struct {
	unlink bool
	name   string // the reflector
	module string
	from   string // the station that asked
}

// linkCommandFor reads a stream's destination as a link command: UNLINK,
// or a reflector in the host files with a module, such as "M17-M17 C".
// Anything else, a station or an unknown reflector, is an ordinary call.
func (g *Gateway) linkCommandFor(lsf m17.LSF) (linkCommand, bool) {
	if !g.linkCfg.rfLinking {
		return linkCommand{}, false
	}
	from := strings.TrimSpace(lsf.Src.Callsign())
	f := strings.Fields(lsf.Dst.Callsign())
	if len(f) == 1 && f[0] == "UNLINK" {
		return linkCommand{unlink: true, from: from}, true
	}
	if len(f) == 2 && len(f[1]) == 1 && f[1][0] >= 'A' && f[1][0] <= 'Z' {
		if _, ok := g.lookupReflector(f[0]); ok {
			return linkCommand{name: f[0], module: f[1], from: from}, true
		}
	}
	return linkCommand{}, false
}

// lookupReflector finds a reflector in the override host file, then the
// host file.
func (g *Gateway) lookupReflector(name string) (inet.Host, bool) {
	var h inet.Host
	var ok bool
	if g.overrideHostfile != nil {
		h, ok = g.overrideHostfile.Hosts[name]
	}
	if !ok && g.hostfile != nil {
		h, ok = g.hostfile.Hosts[name]
	}
	return h, ok
}

// linkAllowed reports whether call may change the link.
func (g *Gateway) linkAllowed(call string) bool {
	if len(g.linkCfg.callsigns) == 0 {
		return true
	}
	for _, p := range g.linkCfg.callsigns {
		if matchCall(p, call) {
			return true
		}
	}
	return false
}

// reflector returns the reflector client, or nil when unlinked.
func (g *Gateway) reflector() *inet.Client {
	return g.link.Load()
}

// linkName names c's reflector and module, such as "M17-KCW B", or "".
func linkName(c *inet.Client) string {
	if c == nil {
		return ""
	}
	return strings.TrimSpace(c.Name + " " + string(c.Module))
}

// home names the reflector and module in the configuration, or "".
func (g *Gateway) home() string {
	return strings.TrimSpace(g.Name + " " + g.Module)
}

// linkTo links to module of reflector name, dropping the current link.
func (g *Gateway) linkTo(name, module, by string) error {
	g.linkMu.Lock()
	defer g.linkMu.Unlock()
	h, ok := g.lookupReflector(name)
	if !ok {
		return fmt.Errorf("reflector %s not found", name)
	}
	c, err := inet.NewClient(name, h.Server, h.Port, module, g.callsign, g.reflectorEvent, g.queuePacket, g.TransmitVoiceStream)
	if err != nil {
		return fmt.Errorf("error creating client: %v", err)
	}
	c.ProbeReflector = true
	if old := g.link.Swap(nil); old != nil {
		old.Close()
	}
	g.linkedBy, g.linkedAt = by, time.Now()
	log.Printf("[DEBUG] Connecting to %s, %s:%d, module %s", name, h.Server, h.Port, module)
	if err := c.Connect(); err != nil {
		return fmt.Errorf("error connecting to %s %s:%d %s: %v", name, h.Server, h.Port, module, err)
	}
	g.link.Store(c)
	log.Printf("[INFO] Linked to %s %s (by %s)", name, module, by)
	return nil
}

// unlink drops the link.
func (g *Gateway) unlink(by string) {
	g.linkMu.Lock()
	defer g.linkMu.Unlock()
	if old := g.link.Swap(nil); old != nil {
		old.Close()
	}
	g.linkedBy, g.linkedAt = by, time.Now()
	log.Printf("[INFO] Unlinked (by %s)", by)
}

// reflectorEvent puts a reflector link event in the dashboard log.
func (g *Gateway) reflectorEvent(event, name string, module byte) {
	g.dashLog.Log("Reflector", event, "name", name, "module", string(module))
}

// runLinkCommand carries out cmd, at the end of the transmission that
// asked for it, and says what it did. A station that may not change the
// link gets no answer.
func (g *Gateway) runLinkCommand(cmd linkCommand) {
	if !g.linkAllowed(cmd.from) {
		log.Printf("[INFO] Ignoring a link command from %s: not in LinkCallsigns", cmd.from)
		g.setState(Idle)
		return
	}
	cur := linkName(g.reflector())
	switch {
	case cmd.unlink && cur == "":
		g.playMessage("callsign", "is_already_unlinked")
	case cmd.unlink:
		g.unlink(cmd.from)
		g.playMessage("callsign", "is_unlinked")
	case cur == cmd.name+" "+cmd.module:
		g.playMessage("callsign", "is_already_linked", cur)
	default:
		if err := g.linkTo(cmd.name, cmd.module, cmd.from); err != nil {
			log.Printf("[ERROR] Linking to %s %s: %v", cmd.name, cmd.module, err)
			g.playMessage("link_refused")
			return
		}
		g.playMessage("callsign", "is_linking", cmd.name+" "+cmd.module)
	}
}

// revertAt returns when the gateway goes back home: RevertAfter after the
// later of the last frame received on RF and the last link change. ok is
// false when it is home, or won't go back.
func (g *Gateway) revertAt() (time.Time, bool) {
	if !g.linkCfg.rfLinking || g.linkCfg.revertAfter <= 0 || linkName(g.reflector()) == g.home() {
		return time.Time{}, false
	}
	g.stateMutex.Lock()
	last := g.lastRX
	g.stateMutex.Unlock()
	g.linkMu.Lock()
	if g.linkedAt.After(last) {
		last = g.linkedAt
	}
	g.linkMu.Unlock()
	return last.Add(g.linkCfg.revertAfter), true
}

// revertLoop goes back home when it is time, and writes the link status,
// until stop closes.
func (g *Gateway) revertLoop(stop <-chan struct{}) {
	t := time.NewTicker(linkPoll)
	defer t.Stop()
	for {
		select {
		case <-t.C:
		case <-stop:
			return
		}
		if at, ok := g.revertAt(); ok && !time.Now().Before(at) {
			log.Printf("[INFO] Nothing received on RF for %v: going back to %q", g.linkCfg.revertAfter, g.home())
			if g.Name == "" {
				g.unlink("revert")
			} else if err := g.linkTo(g.Name, g.Module, "revert"); err != nil {
				log.Printf("[ERROR] Going back to %s %s: %v", g.Name, g.Module, err)
			}
		}
		g.writeLinkStatus()
	}
}

// linkStatus is the link status file's content, for the dashboard.
type linkStatus struct {
	Time        string `json:"time"`
	Home        string `json:"home"`
	Linked      string `json:"linked"`
	LinkedBy    string `json:"linkedBy,omitempty"`
	LinkedAt    string `json:"linkedAt,omitempty"`
	RFLinking   bool   `json:"rfLinking"`
	RevertAfter int    `json:"revertAfter"` // seconds; 0 for never
	RevertAt    string `json:"revertAt,omitempty"`
	LastRF      string `json:"lastRF,omitempty"`
}

// writeLinkStatus writes the link status file, if one is set.
func (g *Gateway) writeLinkStatus() {
	path := g.linkCfg.statusPath
	if path == "" {
		return
	}
	s := linkStatus{
		Time:        stamp(time.Now()),
		Home:        g.home(),
		Linked:      linkName(g.reflector()),
		RFLinking:   g.linkCfg.rfLinking,
		RevertAfter: int(g.linkCfg.revertAfter / time.Second),
	}
	g.linkMu.Lock()
	s.LinkedBy, s.LinkedAt = g.linkedBy, stamp(g.linkedAt)
	g.linkMu.Unlock()
	g.stateMutex.Lock()
	s.LastRF = stamp(g.lastRX)
	g.stateMutex.Unlock()
	if at, ok := g.revertAt(); ok {
		s.RevertAt = stamp(at)
	}
	b, err := json.Marshal(s)
	if err != nil {
		return
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0644); err != nil {
		log.Printf("[ERROR] Writing %s: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, path); err != nil {
		log.Printf("[ERROR] Renaming %s: %v", tmp, err)
	}
}
