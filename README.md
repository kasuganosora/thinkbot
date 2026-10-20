**English** | [简体中文](README.zh-CN.md) | [日本語](README.ja.md) | [Русский](README.ru.md)

<p align="center">
  <img src="web/public/icon-app.svg" alt="thinkbot" width="128" height="128" />
</p>

<h1 align="center">thinkbot</h1>

<p align="center">
  <img src="https://img.shields.io/badge/Go-1.27+-00ADD8?style=flat-square&logo=go&logoColor=white" alt="Go 1.27+" />
  <img src="https://img.shields.io/badge/Docker-compose-2496ED?style=flat-square&logo=docker&logoColor=white" alt="Docker compose" />
  <img src="https://img.shields.io/github/license/kasuganosora/thinkbot?style=flat-square" alt="License" />
  <img src="https://img.shields.io/github/stars/kasuganosora/thinkbot?style=flat-square" alt="Stars" />
  <img src="https://img.shields.io/github/last-commit/kasuganosora/thinkbot?style=flat-square" alt="Last commit" />
</p>

<p align="center">
  <b>When your server has a problem, it reaches you first, and you can talk to it about the problem.</b><br/>
  An open-source, self-hosted AI companion that stays online: it chats, remembers, and gets things done.
</p>

<p align="center">
  <a href="README.zh-CN.md">简体中文</a> · <b>English</b> · <a href="#quick-start">Quick start</a> · <a href="docs/configuration.md">Configuration</a> · <a href="docs/deploy.md">Deployment</a>
</p>

---

You chat with it in Telegram, and it remembers what you told it. When a disk, a RAID array, or a deploy script runs into trouble, it messages you first. Reply "is this serious?" and it picks up from there. It can also run commands in its own sandbox, look things up on the web, and edit code, then tell you how it went.

thinkbot is for people who want an **AI assistant they control**: indie developers, people who run their own servers, and anyone who wants a bot in a group chat or on the Fediverse. A single `docker compose` command starts it, and your data stays on your own machine.

```text
🤖 [server-1/smartd · INFO] sda and sdb each sent a SMART EmailTest.
   That's smartd testing the notification channel, not a fault. Both disks are fine.
🙋 yes, they're testing
🤖 Got it. sda and sdb each sent one, so the alert channel works for both disks.
   If a real SMART problem shows up, you'll hear about it right away.
```

## Meet thinkbot

thinkbot isn't just an assistant that answers whatever you ask. It has a personality (you write it), remembers what you've talked about, chimes in naturally in group chats, and is the first to find you when a server goes wrong. It doesn't try to reply to everything; it aims to be an **always-on companion that knows when to speak and can be relied on**.

## Highlights

- **A personality, not a help-desk voice**: its persona lives in SOUL.md, so you decide how it talks, its quirks and its interests. It uses the same voice for small talk, group banter and alert reports.
- **Hangs out in group chats, and knows when to stay quiet**: you don't have to @ it every time. Its chat rhythm settings decide whether it joins in: a speaking tendency, debouncing for bursts of messages, and a cap on consecutive replies. When someone mentions or replies to it, it always answers.

- **It reaches out to you**: server alerts, deploy results, and reminders you ask for land in your Telegram chat instead of sitting in a log file.
- **It remembers, and forgets on request**: layered long-term memory picks up conversations from weeks ago. Tell it to forget something and it deletes it, and it won't learn it back later. You control your privacy.
- **It can actually do things**: each bot has its own sandbox container where it can run commands, read and write files, use a browser, and search and fetch web pages. You can add skills and MCP tools too.
- **One deployment, many places to chat**: Web, Telegram, Misskey, Mastodon and WeCom share the same bot, and context follows you across platforms. One deployment can host several bots, each with its own personality.
- **Pick your model**: OpenAI-compatible APIs (such as DeepSeek), OpenAI Responses, Anthropic, or Gemini. Switching takes a few clicks in the web UI.
- **You're in charge**: it's self-hosted and keeps its data in a local SQLite file. Tool permissions, usage quotas and user management all live in the web admin console.

## Supported platforms

| Platform | Status | What it can do |
|---|---|---|
| Web | ✅ Available | Built-in chat UI and admin console. Shows tool calls, plus the sandbox terminal and desktop |
| Telegram | ✅ Available | Private and group chats (mention it or reply to it); sends photos and files; group admin tools (pin, delete, ban…, enabled in tool permissions); **receives server alerts** |
| Misskey | ✅ Available | Replies to mentions, can follow timelines; posts, reacts, renotes, follows, searches, runs polls |
| Mastodon | 🚧 In development | Code merged, not yet tested in production. Replies to mentions and replies, optional timeline streams; posting, favourites, boosts and more. Set the instance URL and access token in the web UI under Bot Settings → Platforms |
| WeCom (企业微信) | ✅ Available | Smart-bot long connection (no public IP or callback URL needed); direct chats and group @-mentions; receives images and files |

## What you can do with it

### 1. When your server has a problem, it reaches you first

Have smartd, mdadm, cron, a backup job or a deploy script call `thinkbot-notify`. The alert arrives in the bot's own words, in **the same Telegram chat you already use with it**. Alerts are saved to the chat history, so you can ask follow-up questions and the bot knows which alert you mean.

```text
🤖 [server-1/mdadm · INFO] RebuildFinished on /dev/md/md3 (mismatches found: 140672)
   The routine check on md3 finished with 140672 mismatches. The array is still [UU]
   and both disks are online. Nothing urgent, but worth a look when you have time.
🙋 no action needed
🤖 OK.

        …the next day…

🤖 [server-1/mdadm · WARN] RebuildStarted on /dev/md/md3
   md3 started another check, but progress is stuck at 89.6% at 0K/sec.
   It looks hung. Worth checking.

        …later…

🤖 [server-1/mdadm · INFO] RebuildFinished on /dev/md/md3 (mismatches found: 32384)
   This one finished. Mismatches are down to 32384 and the array is healthy.
```

Deploy scripts work the same way: send the result to the bot as the last step, then reply "any errors?" to keep going. It answers based on what the notification contained. Setup steps: [Configuration › notify quick setup](docs/configuration.md#告警通知notify快速接入).

> Notifications have their own delivery path. The bot's "don't reply to this" logic can't swallow them. If the model fails or times out, the raw text is sent instead. warn and critical alerts always include the original alert text. Repeated alerts are deduplicated and rate limited. Delivery is Telegram-only for now.

### 2. Hand it a multi-step job

```text
🙋 Run eslint and vitest on my local commit in blog-app, and push if it's clean
🤖 Some dependencies were missing, so I installed them.
   ESLint found 2 errors, but they already exist on the base commit, so I fixed them
   in a separate commit.
   1 vitest test fails, and it fails on the base commit too, so it's unrelated.
   The first push failed. I checked the remote config, fixed it, and pushed.
   Summary: 8 files changed, 0 ESLint errors, pushed.
```

Every command runs inside that bot's own sandbox container, never on the host.

### 3. Look things up, with sources

```text
🙋 Don't go from memory this time. Actually search the web for today's release notes for X
🤖 Found them. This is based on the official release page: … The three main changes are: …
   Source: X's official release notes page (link)
```

`web_search` and `web_fetch` are built in. You can configure search backends under System Settings → Search, and it falls back to the next one if a backend fails.

### 4. Use skills

```text
🙋 Load the code-review skill with use_skill and tell me its first principle
🤖 Loaded. Its first principle is: …
```

A skill is a folder with a `SKILL.md` file in it. Put it in `skills/` or add it from the admin console.

### 5. It remembers you, and forgets when you ask

```text
        …three weeks ago…
🙋 My usual coffee is an oat latte, light on sugar
🙋 I'm building a little budgeting app on the side

        …today…
🙋 Morning! Help me figure out what to do first today
🤖 Morning! Maybe start with a light-sugar oat latte ☕, then finish the export
   feature you got stuck on in the budgeting app?

🙋 Forget everything about that weekend trip plan
🤖 Done. I've deleted the memories about the trip plan and won't bring it up again.
```

### 6. Have fun with it

- **Chat with a persona**: write its SOUL.md (personality, way of talking) under Bot Settings → Persona, and it stays in character whether you're talking anime or games.
- **Play a text adventure**: it can give you choices to pick from. Telegram shows them as buttons, Misskey as a native poll, and the Web UI as a card.
- **Show it pictures**: if the main model supports images, or you've set up a vision model, it can read your screenshots and memes. For stickers it sees the sticker's emoji, so it can play along with that.
- **Help out in a group**: it always answers when someone mentions or replies to it. For how it joins in otherwise, see "Hang out in group chats" below.

### 7. Hang out in group chats

Add it to a Telegram group and you don't need to @ it every time. Ordinary group messages also go through its chat rhythm: it picks some of them to answer naturally and just listens to the rest (it can still remember what it heard).

```text
🧑 Jay: anyone going to the comic con this weekend?
🧑 Lin: me! still haven't decided who to cosplay though
🤖 Lin, how about the lead from last year's mecha show? The props are easy and hardly anyone does her
🧑 Jay: lol it actually knows this stuff
🧑 Lin: @thinkbot should I bring a wig?
🤖 Bring one. Once you start sweating, your own hair will be the first thing to collapse
```

Tune it per chat type (private / group / channel) under Bot Settings → Platforms → Chat rhythm:

- **Speaking tendency** (0.01–1.0, groups default to 0.4): for messages that don't mention it, the chance that it speaks up. 1.0 means it replies to everything.
- **Debounce quiet wait** (groups default to 3 s): several messages in a row are treated as one, so it doesn't jump in after each line.
- **Consecutive reply cap** (groups default to 3): after a few turns in a row it holds back so it doesn't take over the chat.
- Mentions and replies to the bot skip these limits and always get an answer. Private chats have rhythm off by default (instant replies), and web chat is never throttled.

It can also decide on its own that a message needs no reply and just take note of it. For Misskey / Mastodon timeline posts you can additionally turn on proactive engagement (`engagement.*` settings, with observer / lurker / moderator / active presets), which scores relevance and applies rate limits before it joins in. See [docs/configuration.md](docs/configuration.md).

### 8. More ways to use it

- **Several bots in one deployment**: each bot has its own persona, model, memory, sandbox container and platform accounts, and they stay separate.
- **One bot on several platforms**: the same bot can run on Telegram and Misskey at once, and what you talked about on one side carries over to the other (Mastodon support is in development: code merged, not yet tested in production).
- **Reminders and schedules**: ask it to "remind me at 9 tomorrow morning", or set up scheduled jobs so it works on a timetable.

## How memory works

In short, it has short-term and long-term memory, a bit like a person:

1. **Noting things down**: things worth remembering from a chat go into short-term memory first, which expires after about two weeks.
2. **Tidying up**: a background job regularly turns short-term memories into long-term ones (facts, preferences), merging duplicates and updating old ones. You can also turn on "dreaming": every night it scores recent memories, keeps the important ones and lets trivial ones fade, and updates its picture of you along the way.
3. **Recalling**: before it replies, it pulls up memories related to the current topic. Matching is lexical, not semantic: there is no embedding model; when the sqlite-vec extension is available, memories are indexed as local character-bigram hash vectors, otherwise keyword matching is used. So a hobby you mentioned weeks ago can come back when the conversation uses similar words.
4. **Forgetting**: ask it to forget something and the related memories are deleted, with a marker left so it doesn't learn them back later. Admins can also view and delete individual memories under Bot Settings → Memory.

Long conversations are condensed into a summary automatically so the chat can keep going. Admins can also trim the current conversation with `/compact` or start over with `/clear`. More in [docs/memory.md](docs/memory.md).

## Quick start

You need a machine with Docker (including the `docker compose` plugin, v2.24 or newer), an API key for a model provider, and (optionally) a Telegram bot token from [@BotFather](https://t.me/BotFather).

**1. Download and prepare the config**

```bash
git clone https://github.com/kasuganosora/thinkbot.git && cd thinkbot
cp .env.example .env
mkdir -p data/container logs/container
```

The defaults in `.env` work as-is, so you don't need to edit anything.

**2. Set up the first admin account** (use your own password, at least 6 characters)

```bash
cat > docker-compose.override.yml <<'YAML'
services:
  thinkbot:
    environment:
      AUTH_BOOTSTRAP_ADMIN: admin
      AUTH_BOOTSTRAP_PASSWORD: change-me-please
    # The default port mapping "8080:8080" is open to your LAN / the internet.
    # Keep these two lines to allow access from this machine only:
    ports: !override
      - "127.0.0.1:8080:8080"
YAML
```

> `!override` needs Docker Compose v2.24+. Without it, `ports` in the override is **merged** with the default `8080:8080` instead of replacing it, and the ports clash. If it runs on a server you need to reach remotely, delete the two `ports` lines and protect it with a reverse proxy or firewall.

**3. Start it**

```bash
docker compose up -d --build
```

The first build takes a few minutes (if you get a permission error, use `sudo` or add your user to the `docker` group). Once `docker compose ps` shows `healthy`, you're ready.

**4. Finish setup in the web UI**

Open `http://localhost:8080`, log in with the account above, then:

1. **System Settings → Model Services** (系统设置 → 模型服务): add a provider, enter your API key, add a model, and **turn on the provider's Enabled (启用) switch**. New providers start disabled, and starting a bot on a disabled provider fails with `LLM "…" not found in config`.
2. **System Settings → Bots → New Bot** (新建 Bot): create a bot and pick its main model.
3. **Bot Settings → Platforms** (平台): add Telegram, paste your bot token, and enable it. Check that the status at the top of the page says running (运行中). New bots start stopped, so click to enable it. The first start builds the bot's sandbox image (about 1.4 GB), which takes a minute or two.
4. **User Settings → Generate code** (用户设置 → 生成授权码): send the `TB-XXXX-XXXX` code to your Telegram bot to link your account. Alerts need this step.

Now say hi to it in Telegram 👋 or just chat in the web UI.

> The web UI is in Simplified Chinese for now. The Chinese labels are in brackets above. After logging in, change your password under User Settings → Security (安全设置). Once the admin account exists, the two `AUTH_BOOTSTRAP_*` lines in the override do nothing and you can delete them.

## Next steps

Most detailed docs are in Chinese. [docs/notify.md](docs/notify.md) is in English.

| Topic | Where |
|---|---|
| All config options, first admin, tool permissions, alert setup | [docs/configuration.md](docs/configuration.md) |
| Deployment details, updates, backup, rollback, migration | [docs/deploy.md](docs/deploy.md) |
| Memory system | [docs/memory.md](docs/memory.md) |
| Full notify API spec | [docs/notify.md](docs/notify.md) |
| Features in depth, architecture, project layout, tech stack | [docs/architecture.md](docs/architecture.md) |
| Running without Docker, frontend dev, Swagger, tests | [docs/development.md](docs/development.md) |

## License

MIT
