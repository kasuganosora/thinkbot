<p align="center">
  <img src="web/public/icon-app.svg" alt="thinkbot" width="128" height="128" />
</p>

<h1 align="center">thinkbot</h1>

<p align="center">
  <b>When your server has a problem, it reaches you first, and you can talk to it about the problem.</b><br/>
  An open-source, self-hosted AI companion that stays online: it chats, remembers, and gets things done.
</p>

<p align="center">
  <a href="README.md">简体中文</a> · English
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

## Why use it

- **It reaches out to you**: server alerts, deploy results, and reminders you ask for land in your Telegram chat instead of sitting in a log file.
- **It remembers, and forgets on request**: layered long-term memory picks up conversations from weeks ago. Tell it to forget something and it deletes it, and it won't learn it back later. You control your privacy.
- **It can actually do things**: each bot has its own sandbox container where it can run commands, read and write files, use a browser, and search and fetch web pages. You can add skills and MCP tools too.
- **One deployment, many places to chat**: Web, Telegram and Misskey share the same bot, and context follows you across platforms. One deployment can host several bots, each with its own personality.
- **Pick your model**: OpenAI-compatible APIs (such as DeepSeek), OpenAI Responses, Anthropic, or Gemini. Switching takes a few clicks in the web UI.
- **You're in charge**: it's self-hosted and keeps its data in a local SQLite file. Tool permissions, usage quotas and user management all live in the web admin console.

## Supported platforms

| Platform | Status | What it can do |
|---|---|---|
| Web | ✅ Available | Built-in chat UI and admin console. Shows tool calls, plus the sandbox terminal and desktop |
| Telegram | ✅ Available | Private and group chats (mention it or reply to it); sends photos and files; group admin tools (pin, delete, ban…, enabled in tool permissions); **receives server alerts** |
| Misskey | ✅ Available | Replies to mentions, can follow timelines; posts, reacts, renotes, follows, searches, runs polls |
| Mastodon | 🚧 Coming soon | Adapter in progress on the [`feat/mastodon-channel`](https://github.com/kasuganosora/thinkbot/tree/feat/mastodon-channel) branch, not merged into master yet |

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
- **Help out in a group**: it answers when someone mentions or replies to it. With proactive engagement turned on, it decides for itself when to chime in, with rate limits so it doesn't spam.

### 7. More ways to use it

- **Several bots in one deployment**: each bot has its own persona, model, memory, sandbox container and platform accounts, and they stay separate.
- **One bot on several platforms**: the same bot can run on Telegram and Misskey at once, and what you talked about on one side carries over to the other (Mastodon coming soon).
- **Reminders and schedules**: ask it to "remind me at 9 tomorrow morning", or set up scheduled jobs so it works on a timetable.

## How memory works

In short, it has short-term and long-term memory, a bit like a person:

1. **Noting things down**: things worth remembering from a chat go into short-term memory first, which expires after about two weeks.
2. **Tidying up**: a background job regularly turns short-term memories into long-term ones (facts, preferences), merging duplicates and updating old ones. You can also turn on "dreaming": every night it scores recent memories, keeps the important ones and lets trivial ones fade, and updates its picture of you along the way.
3. **Recalling**: before it replies, it pulls up memories related to the current topic and matches them by meaning. So a hobby you mentioned weeks ago comes back on its own when a related topic comes up.
4. **Forgetting**: ask it to forget something and the related memories are deleted, with a marker left so it doesn't learn them back later. Admins can also view and delete individual memories under Bot Settings → Memory.

Long conversations are condensed into a summary automatically so the chat can keep going. Admins can also trim the current conversation with `/compact` or start over with `/clear`. More in [docs/memory.md](docs/memory.md).

## Quick start

You need a machine with Docker, an API key for a model provider, and (optionally) a Telegram bot token from [@BotFather](https://t.me/BotFather).

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
YAML
```

**3. Start it**

```bash
docker compose up -d --build
```

The first build takes a few minutes. Once `docker compose ps` shows `healthy`, you're ready.

**4. Finish setup in the web UI**

Open `http://localhost:8080`, log in with the account above, then:

1. **System Settings → Model Services** (系统设置 → 模型服务): add a provider, enter your API key, and add a model.
2. **System Settings → Bots → New Bot** (新建 Bot): create a bot and pick its main model.
3. **Bot Settings → Platforms** (平台): add Telegram, paste your bot token, and enable it. Check that the status at the top of the page says running (运行中). If it doesn't, click to enable the bot.
4. **User Settings → Generate code** (用户设置 → 生成授权码): send the `TB-XXXX-XXXX` code to your Telegram bot to link your account. Alerts need this step.

Now say hi to it in Telegram 👋 or just chat in the web UI.

> The web UI is in Simplified Chinese for now. The Chinese labels are in brackets above. After logging in, change your password under User Settings → Security (安全设置). Once the admin account exists, the two override lines do nothing and you can delete them.

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
