# yarddog

A self-hosted watchdog for a home GPON router (Nokia G-240W-A): a short-lived process that
checks the internet uplink and reboots the router over its web UI when the link is down. No
cloud, no open ports. (The stock firmware has a reboot button but no scheduler.)

Two binaries: `yarddog`, the collector — check, reboot if needed, record, notify, exit; and
`yarddogd`, an optional read-only query daemon that serves the recorded history as a JSON
API and status dashboard over the LAN.

## What it does

- checks connectivity against independent IP and domain targets
- soft mode (default) reboots only when the uplink is actually down; hard mode
  (`--hard-reboot`) reboots unconditionally
- monitor-only mode (`YARDDOG_REBOOT_ENABLED=false`): watch and record, never reboot
- records every run, check and recovery phase in SQLite, with optional per-run
  host-telemetry and ping metrics
- Telegram alerts, outbox-queued so they still arrive after an outage
- `yarddogd` serves all of it read-only over the LAN

## Build & run

```bash
make build OS=arm64          # -> build/{yarddog,yarddogd}-arm64; drop OS= for the host
cp yarddog.env.example .env  # LABEL, TELEGRAMBOT_DSN, ROUTER_USER, ROUTER_PASS are required
./build/yarddog              # runs one cycle and exits
```

`yarddog` is short-lived — schedule it however you like. `yarddogd` is the long-running
daemon (`YARDDOG_DAEMON_TOKEN=… yarddogd`). Config is an env file with `YARDDOG_`-prefixed
keys; `yarddog.env.example` documents every setting. For the reference Raspberry Pi
deployment, run `deploy/pi5-install.sh`.

## Notes

- The router UI is plain HTTP — the password crosses the LAN in cleartext, so keep `.env` at
  mode 600 and the router UI off the internet.
- Tested on Nokia G-240W-A; a different device is a driver in `gateway/router/`, selected by
  `YARDDOG_ROUTER_KIND`.
