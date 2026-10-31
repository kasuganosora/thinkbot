# ThinkBot sandbox channel — ops runbook (T5)

Baseline: master `8a91f68` (#8 hard-cut). TCP transport (#9 `a4e1fcd`) is picked up on the next deploy; Linux prod stays on unix by default.

General deploy, backup, and image notes also live in [deploy.md](deploy.md). This page does not name production hosts.

## Scope (v1)

- Channel: file + command + process only (no PTY / screenshot).
- Hard cut: builtin bots use channel; probe miss → destroy+ensure (Volume A `/data` kept).
- No `auto`/`exec` fallback path for builtin images.
- Channel daemon is container **PID 1** / exec-form `ENTRYPOINT` (no shell wrapper).

## Linux (production)

### Paths

| Role | Host | Container |
|------|------|-----------|
| Volume A (data) | named volume `thinkbot-bot-<botID>` | `/data` |
| Volume B (channel) | `/var/run/thinkbot-bots/thinkbot-channel-<botID>/` | `/run/thinkbot` |
| Socket | `…/channel.sock` (0600) | `/run/thinkbot/channel.sock` |
| One-shot token | `…/token` (0600) | read into memory then deleted |

Token: HKDF(server-secret, botID), not stored in DB, not left in `/proc/1/environ`.

Labels: `thinkbot.channel.version=1`, `thinkbot.channel.protocol=1`.  
HEALTHCHECK: `thinkbot-channel health` (Ping needs no token).

### Deploy (main)

1. Optional but recommended: prebuild the EnsureImage bot context **on the host** so the first main start does not crash-loop on in-container chromium build.
2. Deploy only service `thinkbot` (`docker compose up -d --no-deps thinkbot`). Do not touch gateway / tbtest / tb2.
3. Tag rollback: `thinkbot:rollback-<prev-short-sha>` before switching.
4. SQLite online backup before switch; verify integrity.
5. After up: `/health` revision, RestartCount 0, channels reconnect.
6. Existing builtin bots: recreate on first channel probe failure (or manually destroy+ensure). Volume A kept; desktop session drops briefly. Clean host run dir on destroy: `/var/run/thinkbot-bots/thinkbot-channel-<botID>/`.

### Smoke

- New/recreated bot: `docker top` / inspect → PID 1 = `thinkbot-channel`.
- Labels present; container healthy.
- Host `/var/run/thinkbot-bots/` exists; do not print token contents.
- Upload / Exec / process via channel after recreate.

### Rollback

```bash
docker tag thinkbot:rollback-<prev> thinkbot:local
docker compose up -d --no-deps thinkbot
```

Or checkout the previous detach and restore local compose edits, then the same tag and up.  
Gateway unchanged. Old channel bot images may remain; unused ones can be pruned later.

## Windows / TCP (after #9)

Config `sandbox.channel.transport`:

- `auto` (default): Linux→unix, Windows→TCP
- `http` or `tcp`: TCP frame transport (**not** REST/HTTP requests)
- `unix`: force socket

TCP notes for T5:

- Same frame protocol + Hello token in the frame body (reject `?token=` / query auth).
- Host dials only `127.0.0.1`; publish `127.0.0.1:<dynamic>:17931`.
- Inside the container, listen is `0.0.0.0:17931` (Docker publish needs it). Peers on the same Docker network can reach it; Health.Ping is unauthenticated. **Do not enable `transport=http|tcp` on Linux prod** unless the Docker network is trusted.
- No TLS on loopback.

## Checklist (release)

- [ ] DB backup + integrity
- [ ] rollback image tagged
- [ ] host EnsureImage prebuild (if bot Dockerfile/channel hash changed)
- [ ] main up; health + channels
- [ ] one bot recreate → PID1 + labels + channel ops
- [ ] destroy cleans host run dir
- [ ] DEPLOY.md note (blur hosts/secrets)
