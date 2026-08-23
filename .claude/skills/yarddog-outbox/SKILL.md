---
name: yarddog-outbox
description: How yarddog gets a Telegram message out when the uplink it monitors is the thing that is down — every message is written to tbot_queue before a send is attempted, failures stay queued and flush at the end of the recovery loop and again at the next run's start, and a late message carries its original time as a queued-at annotation. Covers the three layers the outbox spans and the message contract itself. Load before touching services.OutboxService, OutboxRepository, OutboxMessage, the Notifier or Sender ports, gateway/telegram, its Client, sendMessage, DSN parse or bot-token redaction, the tbot_queue table, TELEGRAMBOT_DSN or LABEL, or the wording, tagging or language of any operator-facing Telegram message.
---

# yarddog Telegram outbox

**The Pi's only uplink is through the router it reboots**, so at the moment a message matters
most there is often no internet. That single fact is the whole design.

## Queue first, send second

Every message is **written to `tbot_queue` first**, then a send is attempted. A failed send
leaves the row queued; queued rows are flushed when connectivity returns (at the end of the
recovery loop) and again at the start of the next run. Never send without enqueuing first —
a message that exists only in a failed HTTP call is a message the operator never receives.

A late message carries its **original** time, not its delivery time: `… [queued 04:02]`. Do
not drop the `[queued …]` annotation when reformatting copy; it is the only thing separating
"the internet is down" from "the internet was down four hours ago".

## Three layers, one path

The outbox deliberately spans the layering rather than collapsing into one package:

| Layer | Piece |
|---|---|
| `infrastructure` | the `tbot_queue` table and its CRUD — the `OutboxRepository` port |
| `services` | `OutboxService` (a `Notifier`): reliable-delivery orchestration and the `[queued …]` annotation |
| `gateway/telegram` | the raw Bot API POST — a `Client` implementing `Sender`: DSN parse, `sendMessage`, bot-token redaction |

`domain.OutboxMessage` is the value that travels between them.

**Telegram is spoken over raw `net/http` — no bot library.** That is a stdlib-only constraint,
not an oversight; do not add a Telegram SDK to reach one endpoint.

The bot token must never reach a log or an error string: `gateway/telegram` redacts it, and any
new error path there has to keep doing so. Credentials arrive as `TELEGRAMBOT_DSN`
(`tbot://<chat_id>:@<bot_token>/`), parsed at startup — a structurally invalid DSN is a
startup failure even in monitor-only mode.

## The message contract

Telegram messages are the **human** contract, the counterpart to the collector's exit codes:

- Tag-first, English, templated: `#REBOOT {LABEL} …`, where `LABEL` is the per-host label.
- **Every failure the operator should know about maps to a message *and* to an
  `outcome` / `error` column on the `runs` row.** One without the other is a half-recorded
  event: a message with no row cannot be queried, a row with no message is never noticed.

Not everything is announced. The confirmation and corroboration guards are deliberately silent,
and `degraded` / cooldown-skip notices are edge-triggered — see the `yarddog-connectivity`
skill before adding a new message.
