package notify

import (
	"context"
	"flag"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"
	"time"

	"gorm.io/gorm"

	"github.com/kasuganosora/thinkbot/dao"
)

// CLIUsage 是 `thinkbot notify-token` 子命令的帮助文本。
const CLIUsage = `usage: thinkbot notify-token <command> [flags]

commands:
  create (--bot <botID> [--bot <botID> ...] | --all-bots) [--name <label>]
                                          create a token for the given bots (printed ONCE; only its hash is stored)
  list   [--bot <botID>]                  list tokens (id, scope, name, created, last used, revoked);
                                          --bot shows only tokens that can notify through that bot
  revoke <tokenID>                        revoke a token

A token can only notify through the bots in its scope (403 otherwise). Callers choose the bot per
request: POST /api/notify with {"bot": "<botID>", ...}.

The database is taken from $DB_PATH (default data/thinkbot.db). In the docker deployment run e.g.:
  docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token create --bot <botID> --name maid-hooks
`

// multiFlag 是可重复的字符串 flag（--bot a --bot b，也接受逗号分隔）。
type multiFlag []string

func (m *multiFlag) String() string { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error {
	*m = append(*m, v)
	return nil
}

// RunCLI 执行 notify-token 子命令，返回进程退出码。
func RunCLI(ctx context.Context, db *gorm.DB, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, CLIUsage)
		return 2
	}
	// 只迁移本功能的表（并回填旧 token 的作用域），避免 CLI 对运行中的服务库做全量迁移。
	if err := MigrateTokens(db); err != nil {
		fmt.Fprintf(stderr, "migrate notify tables: %v\n", err)
		return 1
	}
	store := NewTokenStore(db)
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("notify-token "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	var bots multiFlag
	fs.Var(&bots, "bot", "bot id (repeatable; comma-separated also accepted)")
	allBots := fs.Bool("all-bots", false, "token may notify through every bot")
	name := fs.String("name", "", "token label (e.g. maid-hooks)")
	switch cmd {
	case "create":
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if *allBots && len(bots) > 0 {
			fmt.Fprintln(stderr, "use either --bot or --all-bots, not both")
			return 2
		}
		scope := []string(bots)
		if *allBots {
			scope = []string{ScopeAll}
		}
		norm, err := NormalizeScope(scope)
		if err != nil {
			fmt.Fprintln(stderr, "--bot <botID> (repeatable) or --all-bots is required")
			if len(scope) > 0 {
				fmt.Fprintln(stderr, err)
			}
			return 2
		}
		if norm[0] != ScopeAll {
			for _, b := range norm {
				var cnt int64
				if err := db.Model(&dao.BotDefinition{}).Where("id = ?", b).Count(&cnt).Error; err == nil && cnt == 0 {
					fmt.Fprintf(stderr, "bot %q not found in bot_definitions\n", b)
					return 1
				}
			}
		}
		plain, row, err := store.Create(ctx, norm, *name)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stderr, "created notify token id=%s scope=%s name=%q\n", row.ID, row.Scope, row.Name)
		fmt.Fprintln(stderr, "store it now (e.g. in a root-only 0600 file); it cannot be shown again:")
		fmt.Fprintln(stdout, plain)
		return 0
	case "list":
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		filter := ""
		if len(bots) > 0 {
			filter = strings.TrimSpace(bots[0])
		}
		rows, err := store.List(ctx, filter)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tSCOPE\tNAME\tCREATED\tLAST_USED\tREVOKED")
		for i := range rows {
			r := &rows[i]
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, strings.Join(TokenScope(r), ","), r.Name, fmtTime(&r.CreatedAt), fmtTime(r.LastUsedAt), fmtTime(r.RevokedAt))
		}
		_ = tw.Flush()
		return 0
	case "revoke":
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: thinkbot notify-token revoke <tokenID>")
			return 2
		}
		filter := ""
		if len(bots) > 0 {
			filter = strings.TrimSpace(bots[0])
		}
		ok, err := store.Revoke(ctx, filter, fs.Arg(0))
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		if !ok {
			fmt.Fprintln(stderr, "token not found or already revoked")
			return 1
		}
		fmt.Fprintln(stdout, "revoked", fs.Arg(0))
		return 0
	default:
		fmt.Fprint(stderr, CLIUsage)
		return 2
	}
}

func fmtTime(t *time.Time) string {
	if t == nil || t.IsZero() {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04:05")
}
