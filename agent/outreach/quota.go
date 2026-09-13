package outreach

import (
	"context"
	"time"

	"github.com/kasuganosora/thinkbot/dao"
)

// GateDecision 是配额闸门的结果。Allowed=false 时 Status 为跳过原因。
type GateDecision struct {
	Allowed bool
	Status  string
	Detail  string
}

// CheckGate 在 LLM 之前判定能不能发。硬/软条件读该平台自己的数。
func CheckGate(ctx context.Context, repo *Repo, cfg Config, c dao.OutreachCommitment, now time.Time) (GateDecision, error) {
	plat := cfg.Platform(c.ChannelType)
	if !plat.Enabled {
		return GateDecision{Status: StatusSkippedPlatformDisabled, Detail: "platform disabled"}, nil
	}

	hard := isHard(c.Kind)

	last, err := repo.LastInbound(ctx, c.BotID, c.IdentityKey, c.ChannelType)
	if err != nil {
		return GateDecision{}, err
	}
	if !last.IsZero() && plat.QuietHours > 0 {
		quiet := time.Duration(plat.QuietHours * float64(time.Hour))
		if now.Sub(last) < quiet {
			if !hard || !plat.HardBypassQuiet {
				return GateDecision{
					Status: StatusSkippedQuiet,
					Detail: "within quiet window",
				}, nil
			}
		}
	}

	since := now.Add(-24 * time.Hour)
	n, err := repo.CountSentSince(ctx, c.BotID, c.IdentityKey, c.ChannelType, since)
	if err != nil {
		return GateDecision{}, err
	}
	if plat.MaxPerUserPerDay > 0 && int(n) >= plat.MaxPerUserPerDay {
		if !hard || !plat.HardBypassDailyCap {
			return GateDecision{
				Status: StatusSkippedQuota,
				Detail: "daily cap reached",
			}, nil
		}
	}

	return GateDecision{Allowed: true}, nil
}
