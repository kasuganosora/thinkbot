package outreach

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// Repo 是承诺 / 对账 / last-seen 的 SQLite 仓储。
type Repo struct {
	db *gorm.DB
}

// NewRepo 创建仓储。db 不可为 nil。
func NewRepo(db *gorm.DB) *Repo {
	return &Repo{db: db}
}

// ResolveIdentityKey 查身份绑定后生成配额主键。
func (r *Repo) ResolveIdentityKey(ctx context.Context, channelType, userID string) string {
	channelType = strings.ToLower(strings.TrimSpace(channelType))
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return IdentityKey(channelType, userID, "")
	}
	if channelType == "web" {
		return IdentityKey(channelType, userID, "")
	}
	if r == nil || r.db == nil {
		return IdentityKey(channelType, userID, "")
	}
	var m dao.IdentityMapping
	err := r.db.WithContext(ctx).
		Where("platform = ? AND platform_user_id = ?", channelType, userID).
		First(&m).Error
	if err == nil && m.UserID != 0 {
		return IdentityKey(channelType, userID, fmt.Sprintf("%d", m.UserID))
	}
	return IdentityKey(channelType, userID, "")
}

// CreateCommitment 写入一条 pending 承诺。
func (r *Repo) CreateCommitment(ctx context.Context, c *dao.OutreachCommitment) error {
	if c.ID == "" {
		c.ID = idgen.New("oc")
	}
	if c.Status == "" {
		c.Status = dao.OutreachPending
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	return r.db.WithContext(ctx).Create(c).Error
}

// GetCommitment 按 id 读取（限定 bot）。
func (r *Repo) GetCommitment(ctx context.Context, botID, id string) (*dao.OutreachCommitment, error) {
	var c dao.OutreachCommitment
	err := r.db.WithContext(ctx).Where("id = ? AND bot_id = ?", id, botID).First(&c).Error
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// ListPending 列出某 bot（可选平台 / 身份）的 pending 承诺。
func (r *Repo) ListPending(ctx context.Context, botID, platform, identityKey string) ([]dao.OutreachCommitment, error) {
	q := r.db.WithContext(ctx).Where("bot_id = ? AND status = ?", botID, dao.OutreachPending)
	if platform != "" {
		q = q.Where("channel_type = ?", platform)
	}
	if identityKey != "" {
		q = q.Where("identity_key = ?", identityKey)
	}
	var rows []dao.OutreachCommitment
	err := q.Order("due_at ASC").Find(&rows).Error
	return rows, err
}

// Cancel 将 pending 标为 cancelled。已投递的不改。
func (r *Repo) Cancel(ctx context.Context, botID, id string) error {
	res := r.db.WithContext(ctx).Model(&dao.OutreachCommitment{}).
		Where("id = ? AND bot_id = ? AND status = ?", id, botID, dao.OutreachPending).
		Update("status", dao.OutreachCancelled)
	if res.Error != nil {
		return res.Error
	}
	if res.RowsAffected == 0 {
		return gorm.ErrRecordNotFound
	}
	return nil
}

// ExpireStale 把 due_at 早于 cutoff 仍 pending 的标 expired，返回行数。
func (r *Repo) ExpireStale(ctx context.Context, botID string, cutoff time.Time) (int64, error) {
	res := r.db.WithContext(ctx).Model(&dao.OutreachCommitment{}).
		Where("bot_id = ? AND status = ? AND due_at < ?", botID, dao.OutreachPending, cutoff).
		Update("status", dao.OutreachExpired)
	return res.RowsAffected, res.Error
}

// ListDue 返回已到期且未过期窗口的 pending 承诺（硬条件优先）。
func (r *Repo) ListDue(ctx context.Context, botID string, now time.Time) ([]dao.OutreachCommitment, error) {
	oldest := now.Add(-ExpireAfter)
	var rows []dao.OutreachCommitment
	err := r.db.WithContext(ctx).
		Where("bot_id = ? AND status = ? AND due_at <= ? AND due_at >= ?",
			botID, dao.OutreachPending, now, oldest).
		Order("CASE WHEN kind = 'reminder' THEN 0 ELSE 1 END, due_at ASC").
		Find(&rows).Error
	return rows, err
}

// HasSentRecord 是否已有该承诺的 sent 对账（防止发送成功但 mark delivered 失败后重复发）。
func (r *Repo) HasSentRecord(ctx context.Context, commitmentID string) (bool, error) {
	if commitmentID == "" {
		return false, nil
	}
	var n int64
	err := r.db.WithContext(ctx).Model(&dao.OutreachRecord{}).
		Where("commitment_id = ? AND status = ?", commitmentID, StatusSent).
		Limit(1).Count(&n).Error
	return n > 0, err
}

// MarkDelivered 把承诺标为已投递。
func (r *Repo) MarkDelivered(ctx context.Context, id, recordID string, at time.Time) error {
	return r.db.WithContext(ctx).Model(&dao.OutreachCommitment{}).
		Where("id = ?", id).
		Updates(map[string]any{
			"status":              dao.OutreachDelivered,
			"delivered_at":        at,
			"delivered_record_id": recordID,
		}).Error
}

// InsertRecord 写入对账记录。
func (r *Repo) InsertRecord(ctx context.Context, rec *dao.OutreachRecord) error {
	if rec.ID == "" {
		rec.ID = idgen.New("or")
	}
	if rec.CreatedAt.IsZero() {
		rec.CreatedAt = time.Now().UTC()
	}
	return r.db.WithContext(ctx).Create(rec).Error
}

// ListRecords 列出对账记录（最新在前）。
func (r *Repo) ListRecords(ctx context.Context, botID, status, platform string, limit int) ([]dao.OutreachRecord, error) {
	if limit <= 0 || limit > 200 {
		limit = 100
	}
	q := r.db.WithContext(ctx).Where("bot_id = ?", botID)
	if status != "" && status != "all" {
		q = q.Where("status = ?", status)
	}
	if platform != "" {
		q = q.Where("channel_type = ?", platform)
	}
	var rows []dao.OutreachRecord
	err := q.Order("created_at DESC").Limit(limit).Find(&rows).Error
	return rows, err
}

// CountSentSince 统计滑动窗口内已发出的条数（按身份+平台）。
func (r *Repo) CountSentSince(ctx context.Context, botID, identityKey, channelType string, since time.Time) (int64, error) {
	var n int64
	err := r.db.WithContext(ctx).Model(&dao.OutreachRecord{}).
		Where("bot_id = ? AND identity_key = ? AND channel_type = ? AND status = ? AND created_at > ?",
			botID, identityKey, channelType, StatusSent, since).
		Count(&n).Error
	return n, err
}

// LastInbound 读取静默窗起点。零值表示从未入站。
func (r *Repo) LastInbound(ctx context.Context, botID, identityKey, channelType string) (time.Time, error) {
	var row dao.OutreachLastSeen
	err := r.db.WithContext(ctx).
		Where("bot_id = ? AND identity_key = ? AND channel_type = ?", botID, identityKey, channelType).
		First(&row).Error
	if err == gorm.ErrRecordNotFound {
		return time.Time{}, nil
	}
	return row.LastInbound, err
}

// TouchInbound 记录一次真实用户入站。
func (r *Repo) TouchInbound(ctx context.Context, botID, identityKey, channelType string, at time.Time) error {
	row := dao.OutreachLastSeen{
		BotID:       botID,
		IdentityKey: identityKey,
		ChannelType: channelType,
		LastInbound: at,
	}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "bot_id"}, {Name: "identity_key"}, {Name: "channel_type"}},
		DoUpdates: clause.AssignmentColumns([]string{"last_inbound"}),
	}).Create(&row).Error
}
