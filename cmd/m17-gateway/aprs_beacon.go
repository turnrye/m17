package main

import (
	"fmt"
	"math"
	"strconv"
	"strings"

	fap "github.com/hessu/go-aprs-fap"
)

// The gate's beacon tells APRS-IS where the hotspot is and how to reach it
// over M17, in the APRS frequency format (aprs.org/info/freqspec.txt): a
// PHG extension when the antenna height is set, then the frequency in the
// first ten bytes of the comment, the split for a duplex repeater, and the
// mode, as D-STAR beacons do. "M17 CANn" and the linked reflector follow;
// no standard covers those.

// radioInfo is what the beacon says about the hotspot's radio.
type radioInfo struct {
	rxHz     uint32
	txHz     uint32
	powerDBm float32
}

// phg returns the PHG data extension for a transmitter of powerDBm with an
// antenna haatM metres above average terrain and gainDBi of gain, all
// directions: power p² W, height 10·2^h ft, gain g dB.
func phg(powerDBm float32, haatM, gainDBi float64) string {
	digit := func(f float64) int { return int(max(0, min(9, math.Round(f)))) }
	watts := math.Pow(10, float64(powerDBm)/10) / 1000
	feet := haatM * 3.28084
	h := 0
	if feet > 10 {
		h = digit(math.Log2(feet / 10))
	}
	return fmt.Sprintf("PHG%d%d%d0", digit(math.Sqrt(watts)), h, digit(gainDBi))
}

// beaconComment builds the beacon's comment.
func beaconComment(c aprsConfig, r radioInfo, link string) string {
	var b strings.Builder
	if c.haat > 0 {
		b.WriteString(phg(r.powerDBm, c.haat, c.gain))
		b.WriteByte('/')
	}
	if r.txHz > 0 {
		// Whole kHz, as the format allows: 441.1125 MHz is 441.112.
		fmt.Fprintf(&b, "%03d.%03dMHz", r.txHz/1000000, r.txHz/1000%1000)
		// Radios transmit on the hotspot's receive frequency.
		if r.rxHz != r.txHz {
			fmt.Fprintf(&b, " %+04d", (int64(r.rxHz)-int64(r.txHz))/10000)
		}
		b.WriteByte(' ')
	}
	b.WriteString("M17 CAN" + strconv.Itoa(c.can))
	if link != "" {
		b.WriteString(" " + link)
	}
	if c.comment != "" {
		b.WriteString(" " + c.comment)
	}
	return b.String()
}

// beaconLine returns the beacon the gate sends with c, or "" without a
// position.
func beaconLine(c aprsConfig, r radioInfo, link string) (string, error) {
	if !c.hasPosition {
		return "", nil
	}
	pos, err := fap.EncodePosition(c.latitude, c.longitude, nil, nil, nil, c.symbol,
		&fap.EncodePositionOpts{Ambiguity: c.ambiguity, Comment: beaconComment(c, r, link)})
	if err != nil {
		return "", err
	}
	return c.callsign + ">" + aprsToCall + ",TCPIP*:" + pos, nil
}
