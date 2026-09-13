package memory

import (
	"archive/tar"
	"bufio"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const memohSource = "memoh"

// MemohImportReport 是一次 Memoh 备份导入的计数。
type MemohImportReport struct {
	Imported    int      `json:"imported"`
	Skipped     int      `json:"skipped"`
	SkippedJunk int      `json:"skippedJunk"`
	Profiles    int      `json:"profiles"`
	Errors      []string `json:"errors,omitempty"`
}

type memohRawEntry struct {
	ID         string
	CreatedAt  time.Time
	Topic      string
	Type       string
	ProfileRef string
	Channel    string
	Body       string
}

type memohProfile struct {
	Name    string
	Channel string
	Target  string
	Notes   string
	AliasOf string
}

var (
	memohDateFile     = regexp.MustCompile(`(^|/)memory/\d{4}-\d{2}-\d{2}\.md$`)
	memohProfilesFile = regexp.MustCompile(`(^|/)PROFILES\.md$`)
	memohSameTopic    = regexp.MustCompile(`\[\s*↗[^\]]*\]`)
	memohIdleNeedles  = []string{
		"无事", "無新消息", "无新消息", "无任何发言", "無任何發言",
		"无人@", "無人@", "HEARTBEAT_OK", "无人需要回应", "無人需要回應",
		"无事需要处理", "無事需要處理",
	}
	memohSubstanceNeedles = []string{"聊了", "回复", "回覆", "发帖", "發帖", "debug", "教"}
)

// ImportMemohArchive 从 tar / tar.gz 流式导入 Memoh 工作空间记忆。
// 只读取 memory/YYYY-MM-DD.md 与 PROFILES.md，跳过 MEMORY.md 和媒体。
func ImportMemohArchive(ctx context.Context, store *TieredStore, botID string, r io.Reader) (*MemohImportReport, error) {
	if store == nil {
		return nil, fmt.Errorf("memoh import: store is nil")
	}
	br := bufio.NewReader(r)
	magic, err := br.Peek(2)
	if err != nil && err != io.EOF {
		return nil, fmt.Errorf("memoh import: peek: %w", err)
	}
	src := io.Reader(br)
	if len(magic) >= 2 && magic[0] == 0x1f && magic[1] == 0x8b {
		gz, gerr := gzip.NewReader(br)
		if gerr != nil {
			return nil, fmt.Errorf("memoh import: gzip: %w", gerr)
		}
		defer gz.Close()
		src = gz
	}
	entries, profiles, perr := parseMemohTar(src)
	if perr != nil {
		return nil, perr
	}
	return applyMemohImport(ctx, store, botID, entries, profiles)
}

// ImportMemohDir 从已解压目录导入（测试用）。
func ImportMemohDir(ctx context.Context, store *TieredStore, botID, dir string) (*MemohImportReport, error) {
	if store == nil {
		return nil, fmt.Errorf("memoh import: store is nil")
	}
	var entries []memohRawEntry
	var profiles []memohProfile
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(dir, p)
		if rerr != nil {
			return rerr
		}
		name := filepath.ToSlash(rel)
		data, rerr := os.ReadFile(p)
		if rerr != nil {
			return rerr
		}
		switch {
		case memohDateFile.MatchString(name):
			entries = append(entries, parseMemohDaily(string(data))...)
		case memohProfilesFile.MatchString(name):
			profiles = append(profiles, parseMemohProfiles(string(data))...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return applyMemohImport(ctx, store, botID, entries, profiles)
}

func parseMemohTar(r io.Reader) ([]memohRawEntry, []memohProfile, error) {
	tr := tar.NewReader(r)
	var entries []memohRawEntry
	var profiles []memohProfile
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("memoh import: tar: %w", err)
		}
		if hdr.FileInfo().IsDir() {
			continue
		}
		name := path.Clean(filepath.ToSlash(hdr.Name))
		switch {
		case memohDateFile.MatchString(name):
			data, rerr := io.ReadAll(io.LimitReader(tr, 8<<20))
			if rerr != nil {
				return nil, nil, fmt.Errorf("memoh import: read %s: %w", name, rerr)
			}
			entries = append(entries, parseMemohDaily(string(data))...)
		case memohProfilesFile.MatchString(name):
			data, rerr := io.ReadAll(io.LimitReader(tr, 2<<20))
			if rerr != nil {
				return nil, nil, fmt.Errorf("memoh import: read %s: %w", name, rerr)
			}
			profiles = append(profiles, parseMemohProfiles(string(data))...)
		}
	}
	return entries, profiles, nil
}

func normalizeMemohText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, `\n`, "\n")
	return s
}

func parseMemohDaily(text string) []memohRawEntry {
	text = normalizeMemohText(text)
	parts := strings.Split(text, "## Entry ")
	out := make([]memohRawEntry, 0, len(parts))
	for i, part := range parts {
		if i == 0 {
			continue
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		nl := strings.IndexByte(part, '\n')
		heading := part
		rest := ""
		if nl >= 0 {
			heading = strings.TrimSpace(part[:nl])
			rest = part[nl+1:]
		}
		e := memohRawEntry{ID: heading}
		body, meta := splitYAMLFence(rest)
		e.Body = strings.TrimSpace(memohSameTopic.ReplaceAllString(body, ""))
		if v := meta["id"]; v != "" {
			e.ID = v
		}
		e.CreatedAt = parseMemohTime(meta["created_at"])
		e.Topic = meta["topic"]
		e.Type = meta["type"]
		e.ProfileRef = meta["profile_ref"]
		if e.ProfileRef == "" {
			e.ProfileRef = meta["profile_display_name"]
		}
		e.Channel = meta["channel"]
		if e.ID == "" {
			continue
		}
		out = append(out, e)
	}
	return out
}

func splitYAMLFence(rest string) (body string, meta map[string]string) {
	meta = map[string]string{}
	rest = strings.TrimSpace(rest)
	const open = "```yaml"
	if !strings.HasPrefix(rest, open) {
		return rest, meta
	}
	rest = rest[len(open):]
	if strings.HasPrefix(rest, "\n") {
		rest = rest[1:]
	}
	end := strings.Index(rest, "```")
	if end < 0 {
		return rest, meta
	}
	yamlBlock := rest[:end]
	body = strings.TrimSpace(rest[end+3:])
	for _, line := range strings.Split(yamlBlock, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") || strings.HasPrefix(trim, "metadata:") {
			continue
		}
		key, val, ok := splitYAMLKV(trim)
		if !ok {
			continue
		}
		meta[key] = val
	}
	return body, meta
}

func splitYAMLKV(line string) (string, string, bool) {
	i := strings.IndexByte(line, ':')
	if i <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(line[:i])
	val := strings.TrimSpace(line[i+1:])
	val = strings.Trim(val, `"'`)
	if key == "" {
		return "", "", false
	}
	return key, val, true
}

func parseMemohTime(s string) time.Time {
	s = strings.TrimSpace(strings.Trim(s, `"'`))
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{
		time.RFC3339,
		"2006-01-02T15:04:05Z07:00",
		"2006-01-02T15:04:05Z",
		"2006-01-02 15:04:05",
	} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

func parseMemohProfiles(text string) []memohProfile {
	text = normalizeMemohText(text)
	parts := strings.Split(text, "## ")
	var out []memohProfile
	for i, part := range parts {
		if i == 0 {
			continue
		}
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		nl := strings.IndexByte(part, '\n')
		title := part
		rest := ""
		if nl >= 0 {
			title = strings.TrimSpace(part[:nl])
			rest = part[nl+1:]
		}
		p := memohProfile{}
		if idx := strings.Index(strings.ToLower(title), "alias of "); idx >= 0 {
			alias := strings.TrimSpace(title[idx+len("alias of "):])
			if cut := strings.IndexAny(alias, ")\n"); cut >= 0 {
				alias = alias[:cut]
			}
			p.AliasOf = strings.TrimSpace(strings.Trim(alias, "()"))
		}
		if paren := strings.IndexByte(title, '('); paren > 0 {
			p.Name = strings.TrimSpace(title[:paren])
		} else {
			p.Name = title
		}
		for _, line := range strings.Split(rest, "\n") {
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
			key, val, ok := splitYAMLKV(line)
			if !ok {
				continue
			}
			switch strings.ToLower(key) {
			case "name":
				p.Name = val
			case "channel":
				p.Channel = val
			case "target":
				p.Target = val
			case "notes":
				p.Notes = val
			}
		}
		if p.Name == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func applyMemohImport(ctx context.Context, store *TieredStore, botID string, entries []memohRawEntry, profiles []memohProfile) (*MemohImportReport, error) {
	report := &MemohImportReport{}
	userOf := resolveMemohUsers(profiles)
	botScope := BotScope(strings.TrimSpace(botID))

	merged := mergeMemohProfiles(profiles, userOf)
	for uid, p := range merged {
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, err.Error())
			return report, nil
		}
		content := formatMemohProfile(p)
		if strings.TrimSpace(content) == "" {
			continue
		}
		id := memohEntryID("profile:" + uid)
		scope := UserScope(uid)
		if store.HasIDInTier(ctx, Tier3Profile, id) {
			report.Skipped++
			continue
		}
		if err := store.Append(ctx, TieredEntry{
			Entry: Entry{
				ID:         id,
				Scope:      scope,
				Content:    content,
				Category:   "fact",
				Source:     memohSource,
				Importance: 0.8,
				Metadata:   map[string]any{"memoh_id": "profile:" + uid},
				CreatedAt:  time.Now(),
			},
			Tier: Tier3Profile,
		}); err != nil {
			if len(report.Errors) < 20 {
				report.Errors = append(report.Errors, err.Error())
			}
			continue
		}
		report.Imported++
		report.Profiles++
	}

	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			report.Errors = append(report.Errors, err.Error())
			break
		}
		if memohIsJunk(e) {
			report.SkippedJunk++
			continue
		}
		scope := botScope
		if uid := userOf[normalizeMemohName(e.ProfileRef)]; uid != "" && !strings.HasPrefix(e.ProfileRef, "channel_identity:") {
			scope = UserScope(uid)
		}
		id := memohEntryID(e.ID)
		if store.HasIDInTier(ctx, Tier1LongTerm, id) {
			report.Skipped++
			continue
		}
		created := e.CreatedAt
		if created.IsZero() {
			created = time.Now()
		}
		if err := store.Append(ctx, TieredEntry{
			Entry: Entry{
				ID:         id,
				Scope:      scope,
				Content:    e.Body,
				Category:   "event",
				Source:     memohSource,
				Importance: 0.6,
				Metadata: map[string]any{
					"memoh_id": e.ID,
					"topic":    e.Topic,
					"channel":  e.Channel,
				},
				CreatedAt: created,
			},
			Tier: Tier1LongTerm,
		}); err != nil {
			if len(report.Errors) < 20 {
				report.Errors = append(report.Errors, err.Error())
			}
			continue
		}
		report.Imported++
	}
	return report, nil
}

func resolveMemohUsers(profiles []memohProfile) map[string]string {
	parent := map[string]string{}
	var find func(string) string
	find = func(n string) string {
		n = normalizeMemohName(n)
		if n == "" {
			return ""
		}
		p, ok := parent[n]
		if !ok || p == n {
			parent[n] = n
			return n
		}
		r := find(p)
		parent[n] = r
		return r
	}
	for _, p := range profiles {
		if p.Name == "" {
			continue
		}
		find(p.Name)
		if p.AliasOf != "" {
			ra, rb := find(p.Name), find(p.AliasOf)
			if ra != "" && rb != "" && ra != rb {
				parent[ra] = rb
			}
		}
	}
	rootID := map[string]string{}
	for _, p := range profiles {
		r := find(p.Name)
		if r == "" {
			continue
		}
		if t := strings.TrimSpace(p.Target); t != "" {
			rootID[r] = t
		} else if rootID[r] == "" {
			rootID[r] = slugMemohName(r)
		}
	}
	out := map[string]string{}
	for _, p := range profiles {
		r := find(p.Name)
		id := rootID[r]
		if id == "" {
			id = slugMemohName(p.Name)
		}
		out[normalizeMemohName(p.Name)] = id
		if p.Target != "" {
			out[normalizeMemohName(p.Target)] = id
		}
	}
	return out
}

func mergeMemohProfiles(profiles []memohProfile, userOf map[string]string) map[string]memohProfile {
	out := map[string]memohProfile{}
	for _, p := range profiles {
		uid := userOf[normalizeMemohName(p.Name)]
		if uid == "" {
			uid = slugMemohName(p.Name)
		}
		cur := out[uid]
		if p.Name != "" {
			if cur.Name == "" {
				cur.Name = p.Name
			} else if !strings.Contains(cur.Name, p.Name) {
				cur.Name = cur.Name + " / " + p.Name
			}
		}
		if p.Channel != "" && !strings.Contains(cur.Channel, p.Channel) {
			if cur.Channel == "" {
				cur.Channel = p.Channel
			} else {
				cur.Channel = cur.Channel + "," + p.Channel
			}
		}
		if p.Target != "" {
			cur.Target = p.Target
		}
		if p.Notes != "" {
			if cur.Notes == "" {
				cur.Notes = p.Notes
			} else {
				cur.Notes = cur.Notes + "\n" + p.Notes
			}
		}
		out[uid] = cur
	}
	return out
}

func formatMemohProfile(p memohProfile) string {
	var b strings.Builder
	if p.Name != "" {
		fmt.Fprintf(&b, "Name: %s\n", p.Name)
	}
	if p.Channel != "" {
		fmt.Fprintf(&b, "Channel: %s\n", p.Channel)
	}
	if p.Target != "" {
		fmt.Fprintf(&b, "Target: %s\n", p.Target)
	}
	if p.Notes != "" {
		fmt.Fprintf(&b, "Notes: %s", p.Notes)
	}
	return strings.TrimSpace(b.String())
}

func memohIsJunk(e memohRawEntry) bool {
	topic := strings.ToLower(e.Topic)
	if strings.Contains(topic, "heartbeat") || strings.Contains(topic, "心跳") {
		return true
	}
	if strings.EqualFold(e.Type, "user_profile") {
		return true
	}
	body := strings.TrimSpace(e.Body)
	if utf8.RuneCountInString(body) < 20 {
		return true
	}
	bl := strings.ToLower(body)
	if strings.HasPrefix(bl, "[profile]") || strings.HasPrefix(bl, "user personality profile") {
		return true
	}
	if memohHasNeedle(body, memohIdleNeedles) && !memohHasNeedle(body, memohSubstanceNeedles) {
		return true
	}
	return false
}

func memohHasNeedle(s string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(s, n) {
			return true
		}
	}
	return false
}

func memohEntryID(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return "memoh_" + hex.EncodeToString(sum[:8])
}

func normalizeMemohName(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '('); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.TrimPrefix(s, "@")
	return strings.ToLower(s)
}

func slugMemohName(s string) string {
	s = normalizeMemohName(s)
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == ' ' || r == '-' || r == '_':
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		sum := sha256.Sum256([]byte(s))
		return hex.EncodeToString(sum[:6])
	}
	return out
}
