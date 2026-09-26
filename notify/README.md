# notify — external programs → bot → owner

Implements `POST /api/bots/{id}/notify` (HTTP wiring in `api/handler_notify.go`).
See [docs/notify.md](../docs/notify.md) for the endpoint spec, config keys, token CLI,
network restriction and mdadm/smartd hook scripts.

| file | content |
|---|---|
| `config.go` | levels/modes/statuses, `Config` + `LoadConfig` (global `notify.*` + per-bot `bot.<id>.notify.*`) |
| `token.go` | `TokenStore`: create (plain shown once), list, revoke, `Authenticate` (SHA-256 at rest, constant-time compare, bot scope) |
| `netutil.go` | CIDR parsing, `ClientIP` that honours XFF/X-Real-IP only from trusted proxies |
| `format.go` | request validation/sanitization, raw text rendering, persona composition (critical keeps raw verbatim), history text |
| `persona.go` | `LLMPersona`: tool-less LLM rewrite with the notification as escaped JSON data; output cleaning and caps |
| `ratelimit.go` | per-key token bucket |
| `service.go` | orchestration: validate → dedup → rate limit → resolve → render → deliver → audit → history |
| `cli.go` | `thinkbot notify-token create|list|revoke` |
