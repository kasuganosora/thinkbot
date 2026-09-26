# notify — external programs → bot → owner

Implements `POST /api/notify` (body names the bot) and `POST /api/bots/{id}/notify`
(HTTP wiring, bot-context assembly and adapters in `api/handler_notify.go`).
See [docs/notify.md](../docs/notify.md) for the endpoint spec, modes, config keys,
token CLI, network restriction (isolated listener) and mdadm/smartd hook scripts.

| file | content |
|---|---|
| `config.go` | levels/modes (`bot` default, `raw`; `persona` = alias of `bot`)/statuses, `Config` + `LoadConfig` (global `notify.*` + per-bot `bot.<id>.notify.*`, `persona_*` key aliases) |
| `token.go` | `TokenStore`: create (plain shown once) with a bot scope (ids or `*`), list, revoke, `Authenticate` (SHA-256 at rest, constant-time compare), `TokenAllows`, `MigrateTokens` (backfills scope of pre-scope tokens) |
| `netutil.go` | CIDR parsing, `ClientIP` that honours XFF/X-Real-IP only from trusted proxies |
| `format.go` | request validation/sanitization, raw and compact raw rendering, `ComposeBot` (critical appends compact raw facts), history note |
| `botmode.go` | `LLMBot`: tool-less call on the bot's main model with its real identity, memory and owner history (`BotContextSource`), notification as escaped JSON data; reasoning/output cap via `llm.InternalPolicy` purpose `notify`; output cleaning and caps |
| `ratelimit.go` | per-key token bucket |
| `service.go` | orchestration: validate → dedup (per bot) → rate limit (token+bot+source) → resolve → render → deliver → audit → history |
| `cli.go` | `thinkbot notify-token create (--bot … | --all-bots)|list|revoke` |
