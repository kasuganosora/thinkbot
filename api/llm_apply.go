package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kasuganosora/thinkbot/agent/bot"
	"github.com/kasuganosora/thinkbot/config"
	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/llm"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/log"
)

// LLMApplyResult 告诉调用方期望配置有没有变成正在使用的快照。
type LLMApplyResult struct {
	Applied         bool   `json:"applied"`
	Pending         bool   `json:"pending"`
	Message         string `json:"message"`
	DesiredMain     string `json:"desiredMain"`
	DesiredLight    string `json:"desiredLight"`
	DesiredVision   string `json:"desiredVision"`
	EffectiveMain   string `json:"effectiveMain"`
	EffectiveLight  string `json:"effectiveLight"`
	EffectiveVision string `json:"effectiveVision"`
}

// testPersistEffective 让测试在写入生效键之前失败。生产环境保持 nil。
var testPersistEffective func(ids []string) error

// testAfterEffectivePublish 让测试在快照已经换上之后失败，提交必须把键和客户端一起退回。
var testAfterEffectivePublish func(id string) error

func afterEffectivePublish(id string) error {
	if testAfterEffectivePublish == nil {
		return nil
	}
	return testAfterEffectivePublish(id)
}

func (s *BotService) EffectiveLLM(id string) (main, light, vision, applyErr string) {
	if s == nil || s.store == nil {
		return "", "", "", ""
	}
	return s.store.GetString(config.BotLLMKey(id, "main"), ""),
		s.store.GetString(config.BotLLMKey(id, "light"), ""),
		s.store.GetString(config.BotLLMKey(id, "vision_effective"), ""),
		s.store.GetString(llmApplyErrorKey(id), "")
}

func llmApplyErrorKey(botID string) string {
	return "bot." + botID + ".llm_apply_error"
}

// ApplyDesiredLLM 按定义和视觉槽解析模型。
// 解析失败时不替换正在跑的快照，也不改已生效的键。
// 机器人没在跑时只记下已通过校验的生效键，等启动再用。
func (s *BotService) ApplyDesiredLLM(ctx context.Context, id string) (LLMApplyResult, error) {
	res := LLMApplyResult{}
	if s == nil || s.store == nil {
		return res, errs.Internal("bot service is not available")
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	return s.applyDesiredLocked(ctx, id)
}

func (s *BotService) applyDesiredLocked(ctx context.Context, id string) (LLMApplyResult, error) {
	res := LLMApplyResult{}
	def, err := s.GetDefinition(id)
	if err != nil {
		return res, err
	}
	vision := s.store.GetString(config.BotLLMKey(id, "vision"), "")
	res.DesiredMain = def.LLMMain
	res.DesiredLight = def.LLMLight
	res.DesiredVision = vision
	res.EffectiveMain = s.store.GetString(config.BotLLMKey(id, "main"), "")
	res.EffectiveLight = s.store.GetString(config.BotLLMKey(id, "light"), "")
	prevVision := s.store.GetString(config.BotLLMKey(id, "vision_effective"), "")

	builder := config.NewBuilder(s.store, s.logger)
	bundle, err := bot.CreateLLMBundleFrom(builder, id, def.LLMMain, def.LLMLight, vision)
	if err != nil {
		msg := safeApplyMessage(err)
		_ = s.store.Set(ctx, llmApplyErrorKey(id), msg)
		res.Message = msg
		return res, nil
	}
	s.mu.RLock()
	slot := s.llmSlots[id]
	running := s.botInstances[id] != nil
	s.mu.RUnlock()
	prev := slotSnapshot(slot)
	if err := s.persistEffectiveLLM(ctx, id, def.LLMMain, def.LLMLight, vision); err != nil {
		_ = s.store.Set(ctx, llmApplyErrorKey(id), safeApplyMessage(err))
		res.Message = safeApplyMessage(err)
		return res, err
	}
	if running && slot != nil {
		slot.PublishRaw(bot.SnapshotFromBundle(bundle))
		if err := afterEffectivePublish(id); err != nil {
			if prev != nil {
				slot.PublishRaw(prev)
			}
			_ = s.persistEffectiveLLM(ctx, id, res.EffectiveMain, res.EffectiveLight, prevVision)
			_ = s.store.Set(ctx, llmApplyErrorKey(id), safeApplyMessage(err))
			res.Message = safeApplyMessage(err)
			return res, err
		}
	}
	_ = s.store.Set(ctx, llmApplyErrorKey(id), "")
	res.EffectiveMain = def.LLMMain
	res.EffectiveLight = def.LLMLight
	res.EffectiveVision = vision

	if running && slot != nil {
		res.Applied = true
		res.Message = "running bot is using the saved models"
		return res, nil
	}
	res.Pending = true
	res.Message = "saved; applies when the bot starts"
	return res, nil
}

func (s *BotService) persistEffectiveLLM(ctx context.Context, id, mainID, lightID, visionID string) error {
	return s.persistEffectiveBatch(ctx, []effectiveRow{{id: id, main: mainID, light: lightID, vision: visionID}})
}

type effectiveRow struct {
	id, main, light, vision string
}

// commitEffectivePublication 先写生效键，再发布运行状态。
// 发布失败时把键退回发布前的值。写入失败时不发布。
func (s *BotService) commitEffectivePublication(ctx context.Context, id string, row effectiveRow, write bool, install func() error) error {
	prevMain := s.store.GetString(config.BotLLMKey(id, "main"), "")
	prevLight := s.store.GetString(config.BotLLMKey(id, "light"), "")
	prevVision := s.store.GetString(config.BotLLMKey(id, "vision_effective"), "")
	if write {
		if err := s.persistEffectiveLLM(ctx, row.id, row.main, row.light, row.vision); err != nil {
			return err
		}
	}
	if install != nil {
		if err := install(); err != nil {
			if write {
				_ = s.persistEffectiveLLM(ctx, id, prevMain, prevLight, prevVision)
			}
			_ = s.store.Set(ctx, llmApplyErrorKey(id), safeApplyMessage(err))
			return err
		}
	}
	if write && (row.vision == "" || row.vision == s.store.GetString(config.BotLLMKey(id, "vision"), "")) {
		_ = s.store.Set(ctx, llmApplyErrorKey(id), "")
	}
	return nil
}

func (s *BotService) persistEffectiveBatch(ctx context.Context, rows []effectiveRow) error {
	ids := make([]string, len(rows))
	kv := make(map[string]string, len(rows)*3)
	for i, row := range rows {
		ids[i] = row.id
		kv[config.BotLLMKey(row.id, "main")] = row.main
		kv[config.BotLLMKey(row.id, "light")] = row.light
		kv[config.BotLLMKey(row.id, "vision_effective")] = row.vision
	}
	if testPersistEffective != nil {
		if err := testPersistEffective(ids); err != nil {
			return err
		}
	}
	if err := s.store.SetMany(ctx, kv); err != nil {
		return errs.Wrap(err, "persist effective models")
	}
	return nil
}

// chooseStartupModels 决定进程启动时用哪一套模型。
// 期望值能解析就用期望值。期望值非空但解析失败时，如果上一套生效值仍能解析，
// 继续用生效值，不把失败的期望值写成正在运行的配置。
// 期望的主模型为空表示未设置，这时不回退到过期的生效值。
func chooseStartupModels(desiredMain, desiredLight, effectiveMain, effectiveLight string, resolve func(string) error) (mainID, lightID string, fromDesired bool, err error) {
	desiredMain = strings.TrimSpace(desiredMain)
	desiredLight = strings.TrimSpace(desiredLight)
	effectiveMain = strings.TrimSpace(effectiveMain)
	effectiveLight = strings.TrimSpace(effectiveLight)
	if desiredMain == "" {
		return "", "", false, fmt.Errorf("no main LLM assigned")
	}
	desiredErr := resolvePair(desiredMain, desiredLight, resolve)
	if desiredErr == nil {
		return desiredMain, desiredLight, true, nil
	}
	if effectiveMain != "" && resolvePair(effectiveMain, effectiveLight, resolve) == nil {
		return effectiveMain, effectiveLight, false, desiredErr
	}
	return "", "", false, desiredErr
}

type startupLLM struct {
	bundle                                   *bot.LLMBundle
	mainID, lightID, visionID                string
	fromDesired                              bool
	desiredMain, desiredLight, desiredVision string
	material                                 string
}

func (s *BotService) prepareStartupLLM(def *dao.BotDefinition) (startupLLM, error) {
	out := startupLLM{}
	if s == nil || s.store == nil || def == nil {
		return out, errs.BadRequest("bot service is not available")
	}
	id := def.ID
	builder := config.NewBuilder(s.store, s.logger)
	desiredVision := strings.TrimSpace(s.store.GetString(config.BotLLMKey(id, "vision"), ""))
	effectiveVision := strings.TrimSpace(s.store.GetString(config.BotLLMKey(id, "vision_effective"), ""))
	resolveID := func(modelID string) error {
		_, err := builder.ResolveLLMModel(modelID)
		return err
	}
	mainID, lightID, fromDesired, selErr := chooseStartupModels(
		def.LLMMain, def.LLMLight,
		s.store.GetString(config.BotLLMKey(id, "main"), ""),
		s.store.GetString(config.BotLLMKey(id, "light"), ""),
		resolveID,
	)
	if mainID == "" {
		_ = s.store.Set(context.Background(), llmApplyErrorKey(id), safeApplyMessage(selErr))
		return out, errs.Wrap(selErr, "bot_service: create LLM bundle")
	}
	if !fromDesired {
		_ = s.store.Set(context.Background(), llmApplyErrorKey(id), safeApplyMessage(selErr))
		if s.logger != nil {
			s.logger.Warnw("bot start keeping last effective models; desired selection did not resolve",
				"bot_id", id, "err", selErr)
		}
	}
	useVision, _, visionErr := chooseStartupVision(desiredVision, effectiveVision, resolveID)
	if visionErr != nil {
		_ = s.store.Set(context.Background(), llmApplyErrorKey(id), safeApplyMessage(visionErr))
	}
	bundle, err := bot.CreateLLMBundleFrom(builder, id, mainID, lightID, useVision)
	if err != nil {
		_ = s.store.Set(context.Background(), llmApplyErrorKey(id), safeApplyMessage(err))
		return out, errs.Wrap(err, "bot_service: create LLM bundle")
	}
	out.bundle = bundle
	out.mainID, out.lightID, out.visionID = mainID, lightID, useVision
	out.fromDesired = fromDesired
	out.desiredMain, out.desiredLight, out.desiredVision = def.LLMMain, def.LLMLight, desiredVision
	out.material = s.llmMaterial(def)
	return out, nil
}

// llmMaterial 覆盖会改变客户端的服务商内容，不只是模型 ID。
func (s *BotService) llmMaterial(def *dao.BotDefinition) string {
	if s == nil || s.store == nil || def == nil {
		return ""
	}
	h := sha256.New()
	builder := config.NewBuilder(s.store, s.logger)
	ids := []string{def.LLMMain, def.LLMLight, s.store.GetString(config.BotLLMKey(def.ID, "vision"), "")}
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		h.Write([]byte(id))
		h.Write([]byte{0})
		md, err := builder.ResolveLLMModel(id)
		if err != nil {
			h.Write([]byte(err.Error()))
			continue
		}
		raw, _ := s.store.Get("provider." + md.ProviderID)
		h.Write([]byte(raw))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

var testAfterPrepare func(botID string)
var testFailStart func(botID string) error
var testBeforeCommit func(botID string)
var testReadyTimeout time.Duration

func startupDesiredChanged(def *dao.BotDefinition, vision string, prepared startupLLM) bool {
	if def == nil {
		return true
	}
	return def.LLMMain != prepared.desiredMain || def.LLMLight != prepared.desiredLight || strings.TrimSpace(vision) != prepared.desiredVision
}

func chooseStartupVision(desired, effective string, resolve func(string) error) (use string, fromDesired bool, err error) {
	desired = strings.TrimSpace(desired)
	effective = strings.TrimSpace(effective)
	if desired == "" {
		return "", true, nil
	}
	if err = resolve(desired); err == nil {
		return desired, true, nil
	}
	if effective != "" && resolve(effective) == nil {
		return effective, false, err
	}
	return "", false, err
}

func resolvePair(mainID, lightID string, resolve func(string) error) error {
	if err := resolve(mainID); err != nil {
		return err
	}
	if lightID != "" && lightID != mainID {
		if err := resolve(lightID); err != nil {
			return err
		}
	}
	return nil
}

func safeApplyMessage(err error) string {
	if err == nil {
		return ""
	}
	msg := log.RedactSecrets(err.Error())
	if len(msg) > 240 {
		msg = msg[:240]
	}
	return msg
}

// reapplyRunningBots 在服务商变更后为依赖它的运行中机器人重建快照。
// 任何一个解析失败都不替换，避免一部分机器人已经换了、另一部分还停在旧客户端。
func (s *BotService) reapplyRunningBots(ctx context.Context, providerID string) (bool, string) {
	if s == nil || providerID == "" {
		return true, ""
	}
	s.applyMu.Lock()
	defer s.applyMu.Unlock()
	s.mu.RLock()
	ids := make([]string, 0, len(s.llmSlots))
	for id, slot := range s.llmSlots {
		if slot == nil {
			continue
		}
		cur := slot.Current()
		if cur != nil && cur.UsesProvider(providerID) {
			ids = append(ids, id)
			continue
		}
		if s.desiredUsesProvider(id, providerID) {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	if len(ids) == 0 {
		return true, ""
	}
	type ready struct {
		id, main, light, vision string
		snap                    *bot.LLMSnapshot
	}
	built := make([]ready, 0, len(ids))
	builder := config.NewBuilder(s.store, s.logger)
	for _, id := range ids {
		def, err := s.GetDefinition(id)
		if err != nil {
			return false, safeApplyMessage(err)
		}
		vision := s.store.GetString(config.BotLLMKey(id, "vision"), "")
		bundle, err := bot.CreateLLMBundleFrom(builder, id, def.LLMMain, def.LLMLight, vision)
		if err != nil {
			msg := safeApplyMessage(err)
			_ = s.store.Set(ctx, llmApplyErrorKey(id), msg)
			return false, msg
		}
		built = append(built, ready{id: id, main: def.LLMMain, light: def.LLMLight, vision: vision, snap: bot.SnapshotFromBundle(bundle)})
	}
	// 全部解析成功后一次写入生效键。写入失败则一个快照都不换。
	rows := make([]effectiveRow, len(built))
	for i, item := range built {
		rows[i] = effectiveRow{id: item.id, main: item.main, light: item.light, vision: item.vision}
	}
	if err := s.persistEffectiveBatch(ctx, rows); err != nil {
		return false, safeApplyMessage(err)
	}
	for _, item := range built {
		s.mu.RLock()
		slot := s.llmSlots[item.id]
		s.mu.RUnlock()
		if slot == nil {
			continue
		}
		slot.PublishRaw(item.snap)
		_ = s.store.Set(ctx, llmApplyErrorKey(item.id), "")
	}
	return true, ""
}

func slotSnapshot(slot *bot.LLMSlot) *bot.LLMSnapshot {
	if slot == nil {
		return nil
	}
	cur := slot.Current()
	if cur == nil {
		return nil
	}
	cp := *cur
	return &cp
}

// RefuseProviderRemoval 在仍有机器人选择该服务商的模型时拒绝停用或删除。
// 不改机器人的模型选择，也不改服务商记录。
func (s *BotService) RefuseProviderRemoval(providerID string, modelIDs []string) error {
	bots := s.botsReferencingModels(modelIDs)
	if len(bots) == 0 {
		return nil
	}
	return errs.Conflict("provider " + providerID + " is still selected by " + strings.Join(bots, ", ") + "; assign those bots to another model before disabling or deleting it")
}

func (s *BotService) botsReferencingModels(modelIDs []string) []string {
	want := map[string]bool{}
	for _, id := range modelIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			want[id] = true
		}
	}
	if len(want) == 0 || s == nil {
		return nil
	}
	defs, err := s.ListDefinitions()
	if err != nil {
		return nil
	}
	var out []string
	for _, def := range defs {
		hit := want[def.LLMMain] || want[def.LLMLight]
		if !hit && s.store != nil {
			for _, role := range []string{"main", "light", "vision", "vision_effective"} {
				if want[s.store.GetString(config.BotLLMKey(def.ID, role), "")] {
					hit = true
					break
				}
			}
		}
		if hit {
			out = append(out, def.ID)
		}
	}
	sort.Strings(out)
	return out
}

func (s *BotService) desiredUsesProvider(botID, providerID string) bool {
	def, err := s.GetDefinition(botID)
	if err != nil || s.store == nil {
		return false
	}
	builder := config.NewBuilder(s.store, s.logger)
	vision := s.store.GetString(config.BotLLMKey(botID, "vision"), "")
	for _, modelID := range []string{def.LLMMain, def.LLMLight, vision} {
		if strings.TrimSpace(modelID) == "" {
			continue
		}
		md, err := builder.ResolveLLMModel(modelID)
		if err == nil && md.ProviderID == providerID {
			return true
		}
	}
	return false
}

func (s *BotService) notePublicFailure(botID, traceID string, err error) {
	text := llm.UserFailureText(err)
	if text == "" || traceID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.publicFailures == nil {
		s.publicFailures = map[string]string{}
	}
	s.publicFailures[botID+":"+traceID] = text
}

// PublicFailure 给渠道一条固定说明。没有时返回空字符串。
func (s *BotService) PublicFailure(botID, traceID string) string {
	if s == nil || traceID == "" {
		return ""
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publicFailures[botID+":"+traceID]
}
