# Changing the reflector link over RF

**Status:** on `turnrye/m17` branch `feat/aprs-igate`, 2026-10-11.

## What it does

With `[Reflector] RFLinking = true`, a local station changes the gateway's reflector link by transmitting to it, as on g4klx's M17Gateway (Pi-Star, WPSD). No radio changes are needed: set the destination and key up for a moment.

| Destination | The gateway |
|---|---|
| A reflector and module in the host files, such as `M17-M17 C` | unlinks, links there, and says "… is linking to M17-M17 C" |
| `UNLINK` | unlinks, and says so |
| `INFO` (as well as `/INFO`, `#INFO`) | says what it is linked to, as before |
| `ECHO` (as well as `/ECHO`, `#ECHO`) | plays the transmission back, as before |

- **Not forwarded.** A transmission that is a command goes to no reflector.
- **The reply** uses the announcement clips the gateway already ships: `is_linking`, `is_unlinked`, `is_already_linked`, `is_already_unlinked`, `link_refused`.
- **A name not in the host files** is an ordinary call. It isn't refused, and the link stays.
  - **M17Gateway differs:** it unlinks there, so a call to a station whose callsign looks like a reflector's would drop the link.

## Who, and for how long

- **Who may:** `LinkCallsigns` lists the stations that may do it, with `*` for any SSID or suffix (`N0CALL*`). Others are ignored and logged. Empty lets anyone in range, as on M17Gateway.
  - Callsigns aren't authenticated, so this stops accidents, not a determined impostor.
- **Home:** the reflector in the configuration (`Name`/`Module`).
  - Once nothing has been received on RF for `RevertAfter` (15 minutes by default), the gateway links back there. Any transmission from a local radio restarts the wait; reflector traffic it relays does not.
  - A restart also goes home, so the configuration stays the source of truth.
  - M17Gateway does the same with `Startup`, `Revert` and `HangTime`.
- **Status:** `LinkStatus` names a JSON file, rewritten every 5 seconds, for the dashboard. It holds the home, the link, who changed it and when, the last frame received on RF, and when the gateway goes back.

## How

The reflector client used to be created once at startup and read throughout the gateway. It is now held in an atomic pointer (`Gateway.reflector()`).
- **Changes go one at a time,** under a lock. Each one closes the old client, which sends DISC, then dials the new one.
- **Two fixes in `inet.Client`:**
  - `running` became atomic, since `Close()` now runs while the client's goroutine does.
  - A closed client no longer reconnects from its ping-timeout loop, so a dropped link can't come back on its own.
