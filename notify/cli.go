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
  create --bot <botID> [--name <label>]   create a token (printed ONCE; only its hash is stored)
  list   [--bot <botID>]                  list tokens (id, bot, name, created, last used, revoked)
  revoke [--bot <botID>] <tokenID>        revoke a token

The database is taken from $DB_PATH (default data/thinkbot.db). In the docker deployment run e.g.:
  docker exec -u thinkbot -w /app thinkbot /app/thinkbot notify-token create --bot <botID> --name maid-hooks
`

// RunCLI 执行 notify-token 子命令，返回进程退出码。
func RunCLI(ctx context.Context, db *gorm.DB, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, CLIUsage)
		return 2
	}
	// 只迁移本功能的表，避免 CLI 对运行中的服务库做全量迁移。
	if err := db.AutoMigrate(&dao.NotifyToken{}, &dao.NotifyEvent{}); err != nil {
		fmt.Fprintf(stderr, "migrate notify tables: %v\n", err)
		return 1
	}
	store := NewTokenStore(db)
	cmd, rest := args[0], args[1:]
	fs := flag.NewFlagSet("notify-token "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	bot := fs.String("bot", "", "bot id")
	name := fs.String("name", "", "token label (e.g. maid-hooks)")
	switch cmd {
	case "create":
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if strings.TrimSpace(*bot) == "" {
			fmt.Fprintln(stderr, "--bot is required")
			return 2
		}
		var cnt int64
		if err := db.Model(&dao.BotDefinition{}).Where("id = ?", *bot).Count(&cnt).Error; err == nil && cnt == 0 {
			fmt.Fprintf(stderr, "bot %q not found in bot_definitions\n", *bot)
			return 1
		}
		plain, row, err := store.Create(ctx, *bot, *name)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stderr, "created notify token id=%s bot=%s name=%q\n", row.ID, row.BotID, row.Name)
		fmt.Fprintln(stderr, "store it now (e.g. in a root-only 0600 file); it cannot be shown again:")
		fmt.Fprintln(stdout, plain)
		return 0
	case "list":
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		rows, err := store.List(ctx, *bot)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		tw := tabwriter.NewWriter(stdout, 0, 2, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tBOT\tNAME\tCREATED\tLAST_USED\tREVOKED")
		for _, r := range rows {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.ID, r.BotID, r.Name, fmtTime(&r.CreatedAt), fmtTime(r.LastUsedAt), fmtTime(r.RevokedAt))
		}
		_ = tw.Flush()
		return 0
	case "revoke":
		if err := fs.Parse(rest); err != nil {
			return 2
		}
		if fs.NArg() != 1 {
			fmt.Fprintln(stderr, "usage: thinkbot notify-token revoke [--bot <botID>] <tokenID>")
			return 2
		}
		ok, err := store.Revoke(ctx, *bot, fs.Arg(0))
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
