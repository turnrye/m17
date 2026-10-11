# APRS-IS gating in m17-gateway

**Status:** prototype on `turnrye/m17` branch `feat/aprs-igate`, 2026-10-11. Unit and integration tests pass; a dry run against live APRS-IS (receive-only login, dummy modem) works. Not yet tried on a hotspot.

## What it does

A hotspot gates APRS carried in M17 packet mode between its RF and APRS-IS, the way an RF iGate does for AX.25:

- **RF → APRS-IS.** A `0x02` packet heard on RF is sent to APRS-IS with `,qAR,<gate>` appended. Packets are still forwarded to the reflector as before.
- **APRS-IS → RF.** Messages for stations heard on this hotspot's RF, plus whatever the operator's `rf` watch rules select, are transmitted as `0x02` packets. They go out broadcast, under the gateway's M17 callsign, through the existing packet queue.

- **Relay (optional, simplex only).** Messages and acks between two stations both heard on RF are relayed when they evidently can't hear each other.

Everything is off unless `[APRS] Enabled=true`. Transmitting from APRS-IS also needs `IStoRF=true`. With `Passcode=-1` the gate logs in receive-only and logs what it would have sent.

## Over-the-air format

An APRS packet is identifier `0x02`, then a TNC2 monitor line, then a NUL, then the packet CRC. Example: `K0RET-7>APORT1::W1AW     :hello{01}`. This is the style of the SMS (`0x05`) and TLE (`0x07`) payloads. `0x01` stays reserved for conventional AX.25 packet radio and is not treated as APRS.

OpenRTX (branch `feat/aprs-on-memories`) sends and receives this. On receive, it turns the line into an AX.25 frame. A line holding an address AX.25 can't carry is dropped: one longer than six characters, or with a non-numeric SSID.

## Decisions

### 1. Line format from APRS-IS to RF

**Chosen:** send `SRC>DEST,TCPIP,GATE*:info`. The APRS-IS path is replaced by `TCPIP` and the gate, `*`-marked.

- This is exactly the inner packet of the third-party form RF iGates send (`GATE>TOCALL:}SRC>DEST,TCPIP,GATE*:info`). If radios learn to unwrap `}`, they will see the same thing.
- `TCPIP` in the path stops loops. Another M17 gate that hears our transmission won't send it back to APRS-IS, and won't count `SRC` as a local station.
- OpenRTX parses it today. Path elements may carry `*`. An address costs 7 bytes of its 330-byte frame limit.

**Sources the radio can't take** (`WINLINK`, D-Star's `KD7ZYN-S`, about a third of APRS-IS lines in a Seattle sample) are skipped and logged by default. `ThirdParty=true` sends them wrapped instead, under the gate's callsign, for radios that unwrap.

**Alternatives:**
- *Pass the line through with the IS path trimmed* (`SRC>DEST:info`). This loses the loop protection above.
- *Always wrap in third-party form.* This is what Dire Wolf does, but OpenRTX doesn't unwrap yet, so every message would be lost. It also costs about 20 bytes more.

### 2. Who gates

**Chosen:** each hotspot gates its own RF, and nothing else.

- "Heard on RF in the last hour" is knowledge only the hotspot has. It is what decides which messages are worth airtime.
- **RF→IS:** several hotspots hearing one radio is ordinary APRS. APRS-IS drops the copies within its 30 s duplicate window.
- **Packets from the reflector** are transmitted on RF as today. They are never gated to APRS-IS and never make their source "heard".
- **Packets relayed by another gateway** carry Extended Callsign Data in the LSF META. The gate treats them the same way: on the air, but the station isn't local. This covers a third gate overhearing another hotspot's relay or duplex repeat.
- **IS→RF packets** are never sent to the reflector.

So with several hotspots on one reflector module, each one transmits APRS-IS messages only for its own stations, and nothing loops.

**Alternative: one central gate on a reflector module** (like `m17-bridge`'s SMS bridge). It would see every packet only as relayed, with no way to tell which RF a station is on. Its IS→RF traffic would be transmitted by every hotspot on the module. And it would double-gate whatever the hotspots already gate. It could make sense later for network-only clients, which have no RF and no gate of their own. It would then need to skip `0x02` packets that came from a hotspot's RF.

### 3. Acks

The gateway never acks on a radio's behalf. Radios ack their own messages, and the gate passes acks through like any other packet. This differs from `m17-bridge`'s SMS module, which acks for its users.

- **RF→IS duplicates:** a packet identical in source, destination and info within 30 s isn't gated twice. OpenRTX retries after 30, 60, 120 and 240 s, and acks every copy at least 30 s apart, so each retry and each ack goes.
- **A queued message whose ack is heard on RF** is dropped.
- **A sender's retry from APRS-IS** replaces the copy still queued, keyed by sender, addressee and message number.
- **The gate's own IS→RF messages are not counted as "on the air"** for later duplicate checks, following Dire Wolf. So a retry still goes if the radio missed the first copy. Only a copy actually heard on RF stops it.
- **Acks from APRS-IS** go ahead of everything else in the queue.

### 4. LSF destination and CAN

**Destination: broadcast `@ALL` (0xFFFFFFFFFFFF).** OpenRTX decodes all-ones as `ALL`, which `Callsign::isSpecial()` accepts, so the radio takes the packet. The Go library can't encode the plain text `ALL`: `EncodeCallsign` rejects it. `@ALL` is therefore the only broadcast this side can produce.

Broadcast also lets other radios on the channel show the traffic, as on any APRS channel. A directed LSF is possible for messages, since the heard table records the M17 callsign each APRS station was heard from. It is a later option, not needed now.

**CAN matters only when a radio has its CAN RX check on with a non-zero CAN.** OpenRTX accepts a packet only if its callsign matches *and* (CAN matches or the check is off). The gate therefore copies the CAN bits of the addressee's last RF packet into the LSF. CAN is TYPE bits 7–10, which straddle both TYPE bytes.

The library's `LSF.CAN()` and `NewLSF(…, can)` handle only bits 8–10. That is an existing bug, worth fixing separately, so the gate copies the bits directly.

### 5. The gateway's own beacon

**Chosen:** beacon to APRS-IS only, never RF. It goes out every 30 minutes (`BeaconInterval`), from the login call with only `TCPIP*` as the path, and only when `Latitude`/`Longitude` are set and the login is verified.

It is a position report that says how to reach the hotspot, in the APRS frequency format (aprs.org/info/freqspec.txt):

```
K0RET-10>APZ001,TCPIP*:!3508.37NM09001.05W&PHG0330/441.112MHz M17 CAN0 M17-KCW B M17 hotspot
```

The comment is built in this order:
1. **PHG**, if `HAAT` is set: power from `[Radio] Power` (10 dBm is honestly 0 W), height from `HAAT` in metres, gain from `Gain` in dBi.
2. **The frequency**, from `TXFrequency`, in the first ten bytes as the format requires. It is given in whole kHz, so 441.1125 shows as 441.112.
3. **The split** in 10 kHz units, when the receive frequency differs, as on a duplex repeater.
4. **"M17"**, placed where D-STAR beacons put their mode.
5. **`CAN`**, the linked reflector, and `Comment`. No standard covers these.

**Symbol:** `M&`, an M overlay on the gateway symbol. APRS defines no M17 symbol, and g4klx's gateways use an equally undefined `D&`.

**Privacy:** `Ambiguity` 1–4 blurs the position, for a hotspot at someone's home.

**Why:** it puts the hotspot on aprs.fi, gives the `qAR,<gate>` path a location, and gives `near` rules a centre, all at no airtime cost.

**Alternatives:**
- *g4klx's M17Gateway sends `MMDVM Voice (M17) 439.47500MHz -9.0000MHz`.* Its frequency isn't where radios that QSY from beacons look, and its split isn't in the standard form.
- *openSPOT sends no frequency at all.*
- *A frequency object* (`;441.112M*…`), as the frequency spec suggests. Those are meant for local RF, and their name holds only the frequency.
- *Dire Wolf's `IBEACON`* goes to RF by default, with `SENDTO=IG` for APRS-IS.

The gate's own packets use TOCALL `APZ001` (experimental, as `m17-bridge` does). `m17-gateway` should register one with aprsorg/aprs-deviceid.

### 6. Not transmitting what is already on the air

Ryan's rule: an APRS-IS packet is not transmitted if the same packet (source, destination and info; path ignored) was heard on RF in the last 60 s. That covers several cases:

- a duplex hotspot's own repeat;
- another gate or hotspot that sent it first;
- the station itself.

Dire Wolf goes less far. It never compares APRS-IS packets with what it hears. It only suppresses packets its own digipeater or IGate sent in the last 60 s, and messages whose sender was heard directly in the last minute (its `i/` filter). The gate does both of those as well:

- **Local sender:** a message whose sender was heard on RF within 1 minute stays off RF, since the addressee can hear the sender.
- **Watched traffic** (the operator's `rf` rules) stays off RF when its source was heard on RF within `HeardWindow`.

**Local stations on a simplex hotspot** that can't hear each other would otherwise never get each other's messages. Gating doesn't help: APRS-IS doesn't return a packet to the client that sent it, and the local-sender rule would stop it anyway. On FM a digipeater covers this, so the gate has a narrow relay for it (`Relay`, off by default; decision 7).

### 7. Relay between local stations

**Chosen:** on a simplex hotspot, relay only APRS messages and acks, and only between two stations both heard directly on its RF.

- **Messages:** a relay waits `RelayHold` (5 s) and is dropped if the addressee's ack is heard first, since the addressee then heard the sender directly. This is viscous digipeating, decided by the ack instead of by hearing another digipeater.
- **Acks:** an ack is relayed only for a message the gate relayed in the last 5 minutes. If one station needed the relay, the other most likely does too.
- **Marking:** the relay is the sender's LSF with the gateway in its Extended Callsign Data, as a duplex gateway repeats, on the addressee's CAN. The line gets `GATE*` added to its path. Other gateways therefore neither count it as a local station nor relay it again, and gates that hear it still gate it, as a digipeated packet.
- **Excluded:** positions are not relayed. A duplex gateway relays nothing, since it repeats every packet already.

**Alternatives:**
- *A WIDEn-N digipeater:* radios would have to ask for it with a path, which OpenRTX doesn't send on M17. It would also double the airtime of every beacon on a voice channel.
- *A KISS TNC for Dire Wolf or aprx to drive:* Dire Wolf third-party-wraps everything it sends from APRS-IS, which OpenRTX can't unwrap. The M17 parts (voice-first channel sharing, LSF, ECD, CAN) would still have to live in the gateway.

### 8. Defaults, from measured airtime

One `0x02` packet is preamble + LSF + ⌈(line + 4) / 25⌉ packet frames + EOT, at 40 ms each. Measured with `Packet.Encode()`:

| Packet | Line | Frames | On air |
|---|---|---|---|
| ack `K0RET-7>APORT1::W1AW     :ack01` | 31 B | 5 | 200 ms |
| position or short message | 35–42 B | 5 | 200 ms |
| IS→RF message, 37-character text | 80 B | 7 | 280 ms |
| IS→RF message, 67-character text (APRS maximum) | 110 B | 8 | 320 ms |

The CC1200 driver keys about 12 ms before the packet and holds TX 80 ms after it (`packetEndMargin`). So each gated packet takes the channel for about 0.3–0.4 s.

| Key | Default | Why |
|---|---|---|
| `MinTxGap` | 3 s | Before each IS→RF packet, the channel must have been quiet, both received and sent, this long. That covers hearing the channel clear, keying up, and the hotspot locking on the LSF (about 1–1.5 s), and the usual 1–2 s before an answer to the last over. The rate limit already spaces packets 10 s apart on average, so the gap costs nothing in throughput. |
| `RateLimit` / `RateBurst` | 6 per minute, burst 3 | Dire Wolf's default is 6 per minute. At 0.4 s each that is at most 4 % of airtime. Packets over the limit wait in the queue rather than being dropped, as Dire Wolf drops them. |
| `MaxQueueAge` | 60 s | Dire Wolf drops a packet after 60 s without a clear channel. Senders retry, so an older copy is superseded. |
| `HeardWindow` | 60 min | aprs-is.net says "heard within 1 hour (or less)"; Dire Wolf uses 180 min. OpenRTX beacon intervals go up to 60 min, so a shorter window would forget a station that beacons hourly. |
| Per-class queue | 16 acks, 16 messages, 8 other | When a class is full, its oldest packet is dropped. |

## Rules

**RF → APRS-IS** (aprs-is.net IGateDetails). A packet is not gated if any of these hold:

- its source is the gate;
- its path holds `TCPIP`, `TCPXX`, `NOGATE` or `RFONLY`;
- it is a generic query (`?`);
- it is a third-party packet (`}`) whose inner path holds one of those words (layers are unwrapped, and the inner packet is gated);
- it duplicates one gated in the last 30 s.

The q construct is `qAR` when `IStoRF` is on and `qAO` (receive-only gate) when it is off, as in Dire Wolf.

**APRS-IS → RF.** A message, ack or rej goes if all of these hold:

- its addressee was heard on RF within `HeardWindow` (not via another gate or a relay);
- its sender was not heard on RF in the last minute;
- its path holds none of `TCPXX`, `NOGATE`, `RFONLY`, `qAX`;
- it was not heard on RF in the last 60 s;
- its addresses fit AX.25, or `ThirdParty` is on.

Two more kinds go:

- **Courtesy position:** after a message, the sender's position goes once per 30 minutes. It is the last position seen if there is one. Otherwise the filter asks for the sender's next position (`b/`), and that one goes.
- **Watched traffic:** whatever the operator's `rf` watch rules select, unless its source is local (see Operator rules).

The server filter is `g/<gate>/<heard stations…>`, plus `b/<senders owed a position>`, plus the watch rules in force, plus the raw `Filter`. It is updated with `#filter` as stations come and go, and as rules start and end.

## Operator rules

Beyond the iGate's own selection, the operator chooses more APRS-IS traffic with up to 16 rules, `Watch1` to `Watch16` in `[APRS]`. Each one asks the server for some traffic and says what to do with it:

| Rule | Server filter | Matches |
|---|---|---|
| `call CALL` (`CALL*` for every SSID) | `b/CALL g/CALL` | packets from the station, and messages to it |
| `area LAT LON KM` | `r/LAT/LON/KM` | packets placed within KM of the point, and others from stations last placed there |
| `near KM` | `r/` around `Latitude`/`Longitude` | the same, around the gate; nothing without a position |

**Actions:**
- `rf` transmits what the rule selects, with the same quiet gap, rate limit and queue as other APRS-IS traffic, but never a local station's own packets.
- `dashboard` (the default) only logs it, so a wide area can be watched without spending airtime.
- An optional `until <RFC 3339 time>` ends the rule, and the filter drops it. That suits "follow this station for two hours".

**Matching:** the server can't say which part of a filter selected a packet, so the gate checks each one against the rules itself. A rule that transmits wins over one that only logs. For a packet with no position, the sender's last known position counts.

**The raw `Filter`:** for anything the rules can't express. What only it selects is logged, never transmitted.

## Live configuration and status

The gate re-reads `[APRS]` when the configuration file changes, so a dashboard only needs to save the file.

- **When it applies:** a change is applied once the file has been unchanged for one 5-second poll, so a half-written file isn't read. A section that doesn't load is reported and left unapplied.
- **What takes effect at once:** toggles, rules, filter, rates and beacon settings. A new server, callsign or passcode logs in again. The reflector link and the heard list are untouched.
- **Turning APRS on** needs a restart, since the gate isn't created at all while it is off.

With `Status` set, the gate rewrites a JSON status file every 5 seconds. It holds:
- whether it is logged in, and to which server (taken from the server's own keepalive lines);
- verified or receive only;
- the filter in force;
- the heard stations, with their M17 callsign and CAN;
- the rules and whether each is active;
- the queue by class;
- counters, and the last beacon.

## Channel sharing

IS→RF packets wait in their own queue, ordered acks > messages > other. A scheduler hands them, one at a time, to the existing packet queue and sender (`packets.go`). That way they pass the same `channelWait` checks, and reflector and local packets go first. A packet is handed over only when all of these hold:

- the reflector queue is empty;
- no RF has been received for `RXHoldoff`;
- nothing has been received *or* sent for `MinTxGap`;
- a rate-limit token is available.

The last check is repeated just before transmitting, in case a reflector packet went in the meantime. The next packet is timed from the end of the last one, so a burst always leaves a `MinTxGap` gap a local station can key up in.

**When more arrives than the channel can carry**, traffic for nobody in particular is what gives way. That is the `other` class: what watch rules bring, and courtesy positions.

- **Last in line:** it goes only when no ack or message is waiting.
- **One token in reserve:** it may only spend the rate budget while one token is left for an ack or message. A burst of it therefore never makes a message wait for the bucket to refill.
- **Shorter expiry:** it expires after 30 s, against 60 s (`MaxQueueAge`) for acks and messages.
- **Newest wins:** a newer position, status, weather report, telemetry, object or item from the same station replaces the one still waiting, so a backlog clears itself.
- **Full lanes:** a full class drops its oldest packet.

**Seeing it:**
- **Traffic log:** every drop appears there.
- **Status file:**
  - each watch rule's last 15 minutes: matched, sent, dropped, and skipped (a local station's, or one AX.25 can't carry);
  - totals for the same window: sent, dropped, dropped acks and messages, and replaced.
- **Dashboard warning:** the APRS page warns when anything was dropped, and names the rule responsible, so the operator can narrow it or switch it to dashboard-only. The gate doesn't change the rule itself.

**On a duplex repeater** (SX1255, or MMDVM with `Duplex = true`), the same rules hold: a packet never starts while RF is being received. A user who keys up during a gated packet is heard, since the repeater receives while it transmits. Their repeat then races the packet for the modem, as reflector packets already do. A shorter `MinTxGap` can suit a duplex repeater.

## Traffic log

With `Log` set, the gate writes one JSON object per line to its own file:

- every APRS line heard on RF or received from APRS-IS, with what it did with it;
- every packet it transmitted, or dropped from the queue unsent;
- its beacon.

The file is rotated at 4 MB, keeping one old file. It is kept apart from `dashboard.log` because the dashboard's last-heard reads only that file's last 100 lines, so busy APRS-IS traffic would push voice out of them. rpi-dashboard shows it on an APRS page.

| Field | Meaning |
|---|---|
| `time` | RFC 3339 timestamp |
| `dir` | `RF` (heard), `APRS-IS` (received), `RF TX` (transmitted or dropped from the queue), `APRS-IS TX` (the gate's beacon) |
| `m17` | for `RF`, the M17 source it was heard from |
| `line` | the TNC2 line as heard, received, or transmitted |
| `result` | `gated`, `would gate (receive only)`, `not gated: <reason>`, `queued <class>`, `not sent: <reason>`, `sent after <wait>`, `expired after <age>`, `dropped: queue full`, `dropped: acked on RF`, `replaced by a newer copy`, `beacon` |
| `out` | for `RF`, the line as gated to APRS-IS |
| `class` | for `RF TX`: `ack`, `message` or `other` |

```json
{"time":"2026-10-11T08:27:16.32-05:00","dir":"RF","m17":"K0RET","line":"K0RET-7>APORT1:>here","result":"gated","out":"K0RET-7>APORT1,qAR,K0RET-10:>here"}
```

## Not done

- **Rule 4 of aprs-is.net** ("the receiving station has not been heard via the Internet") is not done; Dire Wolf doesn't do it either.
- **Answering `?IGATE?` queries.**
- **Dashboard entries for gated packets.** The dashboard log is unchanged: RF `0x02` packets already appear there as RF packets of type 2.

## What the radio side would need

1. **Third-party unwrap (`}`)**, to receive from sources AX.25 can't hold (`ThirdParty=true`), or longer callsigns and non-numeric SSIDs in its APRS model.
2. **Margin on the first retry.** It goes 30 s after the previous send ends, just past the 30 s duplicate windows of APRS-IS and this gate. A few seconds of jitter, or 35 s, would make sure scheduling slack never pushes a retry inside them.

Nothing else is required. The radio already accepts broadcast `0x02` lines with a `TCPIP,GATE*` path, acks over M17, and composes APRS messages in M17 mode (New APRS).
