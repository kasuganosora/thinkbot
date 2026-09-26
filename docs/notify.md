# Notify API — let external programs make the bot notify its owner

`POST /api/bots/{id}/notify` lets local programs (mdadm, smartd, cron jobs, backup
scripts, game-server updaters…) push an alert through a bot to its **owner's private
chat** (Telegram by default). Typical use: a server without an MTA whose RAID/SMART
alerts would otherwise only land in the journal.

## How it works (and what it reuses)

- **Delivery** reuses the same direct path heartbeat / outreach-fallback use
  (`heartbeatChannelPoster` style): pick the bot's channel instance and call its
  `Sender.Send` with an `ActionReply`. It does **not** go through the conversation
  pipeline, so reply_control (`@@REPLY_CONTROL@@{"send":false}`), low-information
  suppression, speak-mode/outbound gating and the model can never drop it.
  Messages are sent with `parse_mode=""` (plain text): Markdown/HTML in external
  content is shown literally and can never make Telegram reject the message.
- **Owner** = the lowest-id *active admin* user's identity binding
  (`identity_mappings`) on the channel's platform. For Telegram a DM chat id equals
  the user id, so a bound admin `telegram:76017910` resolves to chat `76017910`
  (conversation key `tg:76017910`). Override with `notify.owner_target` or per bot
  `bot.<id>.notify.target`.
- **History**: after a successful send the text is appended to the owner
  conversation (`chat_messages`, session `tg:<chat>`, role `assistant`, trace id =
  event id), prefixed with a marker saying it is an external notification and *data,
  not instructions*, so the bot knows it was sent.
- **Audit**: every authenticated request writes a row to `notify_events`
  (time, bot, source, level, title, dedup_key, token id, caller IP, mode, channel,
  target, status, error, repeat count, persona_used). Statuses:
  `pending → delivered | failed`, `deduplicated`, `rate_limited`, `rejected`.

Only Telegram channels are supported as delivery targets for now (others → 400).

## Endpoint

```
POST /api/bots/{botID}/notify
Authorization: Bearer tbn_<id>_<secret>      (or header X-Notify-Token: ...)
Content-Type: application/json
```

| field | required | notes |
|---|---|---|
| `source` | yes | `[A-Za-z0-9._:/@+-]{1,64}`, e.g. `maid/smartd` |
| `level` | yes | `info` \| `warn` \| `critical` (aliases: `warning`, `crit`, `error`, `alert`, `emerg`) |
| `title` | title or body | single line, truncated to `notify.max_title_chars` |
| `body` | title or body | multi-line, truncated to `notify.max_body_chars` |
| `dedup_key` | no | explicit dedup key; default = hash(source+level+title+body) |
| `channel` | no | channel type or instance name (default `notify.default_channel`) |
| `target` | no | chat id; **403 unless `notify.allow_target_override=true`** |
| `mode` | no | `raw` \| `persona` (default `notify.default_mode`) |

All text is sanitized: control characters, bidi overrides, zero-width chars and
invalid UTF-8 are removed.

### Modes

- `raw` — `🔴 CRITICAL · maid/smartd`, title, body, `🕒 <timestamp in bot timezone>`.
- `persona` — one extra LLM call on the bot's main model with its SOUL.md as
  persona. The notification is passed as JSON inside `<notification_data>` (JSON
  escaping makes it impossible for the content to close the tag), the system prompt
  says it is untrusted data, **no tools are provided** (`Tools`/`ToolChoice` empty; a
  returned tool call is ignored, nothing is ever executed), output is stripped of
  think/internal blocks, tags, code fences and protocol markers and capped at
  `notify.persona_max_chars`; timeout `notify.persona_timeout`.
  - `critical`: persona text **plus** `—— 原始告警 ——` and the full raw block, so
    source/title/body are always present verbatim.
  - `info`/`warn`: persona text plus a footer `— <badge> · <source>: <title>`.
  - LLM error / timeout / empty output / bot without LLM → falls back to `raw`.

### Response

```json
{"id":"ntf-…","status":"delivered","delivered":true,"deduplicated":false,
 "rate_limited":false,"repeat_count":1,"mode":"raw","channel":"Telegram"}
```

| HTTP | when |
|---|---|
| 200 | delivered, or `deduplicated:true` (with `duplicate_of`, `repeat_count`) |
| 400 | invalid JSON / validation error / unsupported or unknown channel |
| 401 | missing, malformed, unknown, or revoked token (`WWW-Authenticate: Bearer`) |
| 403 | caller IP not in `notify.allowed_cidrs`, token belongs to another bot, or `target` override not allowed |
| 404 | `notify.enabled=false`, or bot does not exist |
| 413 | request body larger than `notify.max_request_bytes` |
| 422 | no owner target configured / discoverable |
| 429 | rate limited (`Retry-After` header, `retry_after` field) |
| 502 | channel send failed (not used as a dedup anchor, so a retry will send) |
| 503 | bot not running |

### Dedup and rate limiting

- Dedup: same `dedup_key` (or same source+level+title+body) within
  `notify.dedup_window` returns 200 `deduplicated:true` and bumps the first event's
  `repeat_count`. Including `level` in the automatic hash means a warn→critical
  escalation is not swallowed. When the same notification is delivered again after the
  window, a line `↻ … 又重复了 N 次（已去重）` is appended.
- Rate limit: token bucket per `token id + source`, `notify.rate_limit` for
  info/warn and a **separate** `notify.rate_limit_critical` budget for critical, so a
  flood of warnings cannot block a critical alert. Duplicates never consume budget,
  and critical is still deduplicated. Buckets are in-memory (reset on restart).

## Tokens

Tokens look like `tbn_<12 hex id>_<43 char secret>` (256-bit secret). Only the
SHA-256 is stored (`notify_tokens`); verification uses `crypto/subtle`
constant-time comparison. A token is bound to one bot.

CLI (inside the container, as the app user so file ownership stays right):

```sh
docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token create \
  --bot bot-2d8f9b087270da0bcfe177a5 --name maid-hooks
# stdout: the token (printed once). stderr: id / hints.
docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token list --bot bot-2d8f9b087270da0bcfe177a5
docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token revoke <tokenID>
```

The CLI uses `$DB_PATH` (default `data/thinkbot.db`) and only migrates the two notify
tables. Store the token directly into a root-only file without echoing it:

```sh
install -d -m 0700 -o root -g root /etc/thinkbot
docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token create \
  --bot bot-2d8f9b087270da0bcfe177a5 --name maid-hooks \
  | install -m 0600 -o root -g root /dev/stdin /etc/thinkbot/notify.token
```

Admin API (session cookie, `bot.manage` permission):
`GET/POST /api/bots/{id}/notify/tokens` (POST body `{"name":"…"}`, returns
`notifyToken` once), `DELETE /api/bots/{id}/notify/tokens/{tokenID}`,
`GET /api/bots/{id}/notify/events?limit=100`.

## Configuration

Global keys (`.env`, env vars, or the config UI/API). `channel`, `target` and `mode`
can be overridden per bot with `bot.<botID>.notify.channel|target|mode`.
Everything except `notify.listen_addr` is read per request.

| key | default | meaning |
|---|---|---|
| `notify.enabled` | `true` | master switch (no tokens ⇒ every call is 401 anyway) |
| `notify.listen_addr` | *(empty)* | if set (e.g. `0.0.0.0:8091`), the notify route is served **only** on this extra listener and removed from the main API listener. Restart required |
| `notify.allowed_cidrs` | `127.0.0.0/8,::1/128,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,fc00::/7` | allowed caller networks |
| `notify.trusted_proxies` | same as above | `X-Forwarded-For`/`X-Real-IP` are honoured only when the direct peer is in this list; XFF is walked right-to-left skipping trusted hops |
| `notify.default_channel` | `telegram` | channel type or instance name |
| `notify.owner_target` | *(empty)* | explicit owner chat id; empty = discover from admin identity binding |
| `notify.default_mode` | `raw` | `raw` \| `persona` |
| `notify.allow_target_override` | `false` | allow `target` in the request body |
| `notify.max_request_bytes` | `16384` | larger bodies → 413 |
| `notify.max_title_chars` | `200` | title truncation |
| `notify.max_body_chars` | `3000` | body truncation (fits one Telegram message) |
| `notify.rate_limit` | `20/1h` | info/warn budget per token+source (`N/duration`, `0/…` disables) |
| `notify.rate_limit_critical` | `60/1h` | separate critical budget |
| `notify.dedup_window` | `30m` | `0` disables dedup |
| `notify.persona_timeout` | `45s` | persona LLM timeout (then raw) |
| `notify.persona_max_chars` | `1000` | persona output cap |
| `notify.persona_max_tokens` | `0` | cap for persona `max_tokens`; 0 = the model's configured `maxTokens` (a value > 0 can only lower it, same rule as `llm.ResolveMaxOutputTokens`) |
| `notify.record_history` | `true` | append delivered text to the owner conversation |

## Network restriction

The caller IP check happens before authentication. Note the Docker caveat: callers
on the host reach a published port through docker-proxy/NAT and show up as the bridge
gateway (`172.17.0.1`-ish, a private address). IPv6 traffic to a port published on
`[::]` is also proxied and appears with that same gateway address, so **with the
default config, CIDR filtering cannot tell a host-local caller from a remote IPv6
caller on the main port**; the token is still required. Recommended for maid:

1. Dedicated local listener (the route disappears from the public port):
   ```
   # .env
   notify.listen_addr=0.0.0.0:8091
   ```
   ```yaml
   # docker-compose.yml, service thinkbot
   ports:
     - "8082:8080"
     - "127.0.0.1:8091:8091"   # host loopback only
   ```
   Hooks then use `http://127.0.0.1:8091/api/bots/<id>/notify`
   (`/etc/thinkbot/notify.url`).
2. And/or block the route on the public nginx vhost (put it before `location /`):
   ```nginx
   # bot.hime.at: never expose the notify endpoint publicly
   location ~ ^/api/bots/[^/]+/notify$ {
       allow 127.0.0.1;
       allow ::1;
       deny all;
       proxy_pass http://127.0.0.1:8082;
       proxy_set_header Host $host;
       proxy_set_header X-Real-IP $remote_addr;
       proxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;
   }
   ```
   Even without this, requests forwarded by nginx carry the public client in
   `X-Forwarded-For`, which the endpoint evaluates (the proxy is trusted), so they get
   403.

## curl

```sh
TOKEN=$(cat /etc/thinkbot/notify.token)   # as root
curl -sS -X POST http://127.0.0.1:8082/api/bots/bot-2d8f9b087270da0bcfe177a5/notify \
  -H "Authorization: Bearer $TOKEN" -H 'Content-Type: application/json' \
  -d '{"source":"maid/test","level":"info","title":"notify test","body":"hello from maid"}'

# persona mode, explicit dedup key
curl -sS -X POST http://127.0.0.1:8082/api/bots/bot-2d8f9b087270da0bcfe177a5/notify \
  -H @<(printf 'Authorization: Bearer %s\n' "$(cat /etc/thinkbot/notify.token)") \
  -H 'Content-Type: application/json' \
  -d '{"source":"maid/backup","level":"warn","title":"nightly backup failed","body":"rsync exit 23","dedup_key":"backup-nightly","mode":"persona"}'
```

(`-H @file` keeps the token out of `ps`; the first form is fine for a manual test.)

## Hook scripts (maid)

Files live in the repo under `scripts/notify/`. Install:

```sh
install -o root -g root -m 0755 scripts/notify/thinkbot-notify      /usr/local/sbin/thinkbot-notify
install -o root -g root -m 0755 scripts/notify/thinkbot-mdadm-hook  /usr/local/sbin/thinkbot-mdadm-hook
install -o root -g root -m 0755 scripts/notify/60thinkbot-notify    /etc/smartmontools/run.d/60thinkbot-notify
# token: /etc/thinkbot/notify.token (0600 root), optional URL: /etc/thinkbot/notify.url
echo 'PROGRAM /usr/local/sbin/thinkbot-mdadm-hook' >> /etc/mdadm/mdadm.conf
systemctl restart mdmonitor            # Debian: mdadm --monitor service
mdadm --monitor --scan --oneshot --test # sends a TestMessage (level info) per array
```

smartd on Debian already runs `-M exec /usr/share/smartmontools/smartd-runner`, which
executes every script in `/etc/smartmontools/run.d/` (the existing `10mail` fails
because there is no `mail`; `60thinkbot-notify` still runs). To test, temporarily
add `-M test` to the `DEVICESCAN` line and restart `smartd`. Without smartd-runner,
use `-M exec /etc/smartmontools/run.d/60thinkbot-notify` directly.

### `/usr/local/sbin/thinkbot-notify` (generic sender)

```python
#!/usr/bin/env python3
"""thinkbot-notify: push a notification to the owner through a ThinkBot bot.

Install:  install -o root -g root -m 0755 thinkbot-notify /usr/local/sbin/thinkbot-notify
Config:   /etc/thinkbot/notify.token  (root:root 0600, one line: tbn_...)
          /etc/thinkbot/notify.url    (optional, one line; or env THINKBOT_NOTIFY_URL)

Usage:
  thinkbot-notify -s maid/smartd -l critical -t "SMART failure on /dev/sda" [-b BODY | -b - (stdin)]
                  [-k DEDUP_KEY] [-m raw|persona] [-c CHANNEL]

The token is read from a file and sent in the Authorization header, so it never
appears in the process list. Exit status: 0 = delivered or deduplicated,
1 = rejected / failed / unreachable (details on stderr and in the journal).
"""
import argparse
import json
import os
import sys
import syslog
import urllib.error
import urllib.request

DEFAULT_URL = "http://127.0.0.1:8082/api/bots/bot-2d8f9b087270da0bcfe177a5/notify"
TOKEN_FILE = os.environ.get("THINKBOT_NOTIFY_TOKEN_FILE", "/etc/thinkbot/notify.token")
URL_FILE = "/etc/thinkbot/notify.url"


def read_first_line(path):
    with open(path, "r", encoding="utf-8") as f:
        return f.readline().strip()


def log(msg):
    syslog.openlog("thinkbot-notify", syslog.LOG_PID, syslog.LOG_DAEMON)
    syslog.syslog(syslog.LOG_WARNING, msg)
    print("thinkbot-notify: " + msg, file=sys.stderr)


def main():
    ap = argparse.ArgumentParser(description="Send a ThinkBot owner notification")
    ap.add_argument("-s", "--source", required=True, help="e.g. maid/smartd")
    ap.add_argument("-l", "--level", default="info", choices=["info", "warn", "critical"])
    ap.add_argument("-t", "--title", required=True)
    ap.add_argument("-b", "--body", default="", help="body text, or - to read stdin")
    ap.add_argument("-k", "--dedup-key", default="")
    ap.add_argument("-m", "--mode", default="", choices=["", "raw", "persona"])
    ap.add_argument("-c", "--channel", default="")
    ap.add_argument("--timeout", type=float, default=70.0)
    a = ap.parse_args()

    body = sys.stdin.read() if a.body == "-" else a.body
    body = body[:12000]  # server caps at notify.max_body_chars anyway; stay under max_request_bytes

    url = os.environ.get("THINKBOT_NOTIFY_URL", "")
    if not url and os.path.exists(URL_FILE):
        url = read_first_line(URL_FILE)
    url = url or DEFAULT_URL
    try:
        token = read_first_line(TOKEN_FILE)
    except OSError as e:
        log("cannot read token file %s: %s" % (TOKEN_FILE, e))
        return 1

    payload = {"source": a.source, "level": a.level, "title": a.title, "body": body}
    for k, v in (("dedup_key", a.dedup_key), ("mode", a.mode), ("channel", a.channel)):
        if v:
            payload[k] = v
    data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
    # keep well below the default notify.max_request_bytes (16384)
    while len(data) > 15000 and payload["body"]:
        payload["body"] = payload["body"][: int(len(payload["body"]) * 0.8)]
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")

    req = urllib.request.Request(url, data=data, method="POST", headers={
        "Content-Type": "application/json",
        "Authorization": "Bearer " + token,
    })
    try:
        with urllib.request.urlopen(req, timeout=a.timeout) as resp:
            out = json.loads(resp.read().decode("utf-8") or "{}")
    except urllib.error.HTTPError as e:
        detail = e.read().decode("utf-8", "replace")[:500]
        log("HTTP %d from %s: %s (source=%s title=%r)" % (e.code, url, detail, a.source, a.title))
        return 1
    except Exception as e:  # network error, timeout, bad JSON
        log("request to %s failed: %s (source=%s title=%r)" % (url, e, a.source, a.title))
        return 1
    if out.get("delivered") or out.get("deduplicated"):
        print(json.dumps(out, ensure_ascii=False))
        return 0
    log("not delivered: %s" % json.dumps(out, ensure_ascii=False))
    return 1


if __name__ == "__main__":
    sys.exit(main())
```

### `/usr/local/sbin/thinkbot-mdadm-hook` (mdadm `PROGRAM`)

```sh
#!/bin/sh
# mdadm PROGRAM hook -> ThinkBot owner notification.
# Install: install -o root -g root -m 0755 thinkbot-mdadm-hook /usr/local/sbin/thinkbot-mdadm-hook
# mdadm.conf:  PROGRAM /usr/local/sbin/thinkbot-mdadm-hook
# mdadm calls: PROGRAM <event> <md-device> [<component-device>]
# Test:        mdadm --monitor --scan --oneshot --test
EVENT="$1"; MD="$2"; COMP="$3"
HOST="$(hostname -s)"

case "$EVENT" in
  Fail|FailSpare|DegradedArray|DeviceDisappeared) LEVEL=critical ;;
  SparesMissing|RebuildStarted|MoveSpare)          LEVEL=warn ;;
  *)                                               LEVEL=info ;;   # RebuildNN, RebuildFinished, SpareActive, NewArray, TestMessage
esac

TITLE="mdadm $EVENT on $MD${COMP:+ ($COMP)}"
BODY="$(printf 'host: %s\nevent: %s\narray: %s\ncomponent: %s\n\n/proc/mdstat:\n%s\n' \
  "$HOST" "$EVENT" "$MD" "${COMP:--}" "$(cat /proc/mdstat 2>/dev/null)")"

printf '%s' "$BODY" | /usr/local/sbin/thinkbot-notify \
  -s "$HOST/mdadm" -l "$LEVEL" -t "$TITLE" -b - -k "mdadm/$MD/$EVENT/${COMP:-}" \
  || logger -t thinkbot-mdadm-hook "notify failed: $TITLE"
exit 0
```

### `/etc/smartmontools/run.d/60thinkbot-notify` (smartd `-M exec`)

```sh
#!/bin/sh
# smartd -> ThinkBot owner notification.
#
# Debian's smartd.conf already uses "-M exec /usr/share/smartmontools/smartd-runner",
# which runs every script in /etc/smartmontools/run.d/ with the message file as $1.
# Install: install -o root -g root -m 0755 60thinkbot-notify /etc/smartmontools/run.d/60thinkbot-notify
#   (run-parts --lsbsysinit: the file name must not contain a dot)
# Alternatively point smartd at this script directly: "-M exec /etc/smartmontools/run.d/60thinkbot-notify".
# Test: add "-M test" to the DEVICESCAN line, restart smartd, then remove it again.
MSGFILE="$1"
HOST="$(hostname -s)"
DEV="${SMARTD_DEVICESTRING:-${SMARTD_DEVICE:-unknown}}"
TYPE="${SMARTD_FAILTYPE:-unknown}"

case "$TYPE" in
  EmailTest)         LEVEL=info ;;
  Temperature|Usage) LEVEL=warn ;;
  *)                 LEVEL=critical ;;  # Health, SelfTest, CurrentPendingSector, OfflineUncorrectableSector, ErrorCount, Failed*...
esac

TITLE="SMART $TYPE on $DEV"
{
  printf 'host: %s\ndevice: %s\ninfo: %s\nfirst seen: %s\n\n' "$HOST" "$DEV" "${SMARTD_DEVICEINFO:--}" "${SMARTD_TFIRST:--}"
  if [ -n "$SMARTD_FULLMESSAGE" ]; then printf '%s\n' "$SMARTD_FULLMESSAGE"
  elif [ -n "$MSGFILE" ] && [ -r "$MSGFILE" ]; then cat "$MSGFILE"
  else printf '%s\n' "${SMARTD_MESSAGE:-}"; fi
} | /usr/local/sbin/thinkbot-notify \
  -s "$HOST/smartd" -l "$LEVEL" -t "$TITLE" -b - -k "smartd/$DEV/$TYPE" \
  || logger -t thinkbot-smartd-hook "notify failed: $TITLE"
exit 0
```

### Scrub / cron examples

```sh
# /etc/cron.d/thinkbot-md-mismatch — after Debian's monthly checkarray
30 8 * * * root for f in /sys/block/md*/md/mismatch_cnt; do n=$(cat "$f"); [ "$n" -gt 0 ] && \
  /usr/local/sbin/thinkbot-notify -s "$(hostname -s)/mdcheck" -l warn \
  -t "RAID scrub mismatch on $(echo $f | cut -d/ -f4): $n" -k "mdcheck/$f/$n" >/dev/null; done

# any cron job: report failures only
0 3 * * * root /usr/local/bin/backup.sh >/var/log/backup.log 2>&1 || \
  tail -n 40 /var/log/backup.log | /usr/local/sbin/thinkbot-notify -s maid/backup -l warn \
  -t "nightly backup failed" -b - -k backup-nightly >/dev/null
```

## Security notes

- Token ≠ admin password/session; the route is outside cookie auth. Tokens are
  bot-scoped, hashed at rest, compared in constant time, revocable, and `last_used_at`
  is tracked. Query-string tokens are not accepted (they end up in access logs).
- The request logger does not pre-read or log notify request bodies.
- External content is treated as data: plain-text delivery, sanitized input, capped
  sizes, persona call without tools, and the history entry is explicitly marked as
  external data.
