package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/kasuganosora/thinkbot/dao"
	"github.com/kasuganosora/thinkbot/util/errs"
	"github.com/kasuganosora/thinkbot/util/idgen"
)

// ============================================================================
// Bot 浏览器 Cookie 管理 Handler
//
// 路由前缀：/api/bots/:id/browser/cookies
// 权限：requirePermission(auth.PermBotManage)
//
// 安全红线（见 docs/sandbox-browser-image-design.md §10.4）：
//   - 列表接口 value 默认掩码；单条 ?reveal=true 才返回完整值
//   - 日志只记 domain + 条数，绝不记 value
//   - 不给 agent 任何读 cookie 的工具
// ============================================================================

// handleListBotBrowserCookies GET 列表（value 默认掩码）。
func (s *Server) handleListBotBrowserCookies(c *gin.Context) {
	botID := c.Param("id")
	var cookies []dao.BotBrowserCookie
	if err := s.db.WithContext(c.Request.Context()).Where("bot_id = ?", botID).Order("domain, name").Find(&cookies).Error; err != nil {
		Fail(c, errs.Wrap(err, "list browser cookies"))
		return
	}
	views := make([]dao.BrowserCookieView, 0, len(cookies))
	for _, ck := range cookies {
		views = append(views, ck.ToView(true))
	}
	OK(c, gin.H{"cookies": views})
}

// handleGetBotBrowserCookie GET 单条（?reveal=true 返回完整值）。
func (s *Server) handleGetBotBrowserCookie(c *gin.Context) {
	botID := c.Param("id")
	cid := c.Param("cid")
	var ck dao.BotBrowserCookie
	if err := s.db.WithContext(c.Request.Context()).Where("id = ? AND bot_id = ?", cid, botID).First(&ck).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Fail(c, errs.NotFound("cookie 不存在"))
			return
		}
		Fail(c, err)
		return
	}
	reveal := c.Query("reveal") == "true"
	OK(c, ck.ToView(!reveal))
}

// handleCreateBotBrowserCookie POST 新增单条。
func (s *Server) handleCreateBotBrowserCookie(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Domain   string  `json:"domain"`
		Name     string  `json:"name"`
		Value    string  `json:"value"`
		Path     string  `json:"path"`
		Expires  float64 `json:"expires"` // 与 import/recovery 一致，接受浮点秒（浏览器导出常带小数）
		HTTPOnly bool    `json:"httpOnly"`
		Secure   bool    `json:"secure"`
		SameSite string  `json:"sameSite"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	if req.Domain == "" || req.Name == "" {
		Fail(c, errs.BadRequest("domain 和 name 必填"))
		return
	}
	if req.Path == "" {
		req.Path = "/"
	}
	ck := dao.BotBrowserCookie{
		ID:       idgen.New("bc"),
		BotID:    botID,
		Domain:   req.Domain,
		Name:     req.Name,
		Value:    req.Value,
		Path:     req.Path,
		Expires:  int64(req.Expires),
		HTTPOnly: req.HTTPOnly,
		Secure:   req.Secure,
		SameSite: req.SameSite,
	}
	// 同键（bot_id,domain,name,path）视为同一 cookie：面板重复新增等价于更新其值，
	// 而非插入重复行（有 DB 唯一索引兜底，直接 Create 会撞约束报错）。
	if err := s.db.WithContext(c.Request.Context()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "bot_id"}, {Name: "domain"}, {Name: "name"}, {Name: "path"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "expires", "http_only", "secure", "same_site", "updated_at"}),
	}).Create(&ck).Error; err != nil {
		Fail(c, errs.Wrap(err, "create browser cookie"))
		return
	}
	s.logger.Infow("browser cookie created", "bot", botID, "domain", req.Domain, "name", req.Name)
	OK(c, ck.ToView(false))
}

// handleUpdateBotBrowserCookie PUT 编辑（部分字段）。
func (s *Server) handleUpdateBotBrowserCookie(c *gin.Context) {
	botID := c.Param("id")
	cid := c.Param("cid")
	var ck dao.BotBrowserCookie
	if err := s.db.WithContext(c.Request.Context()).Where("id = ? AND bot_id = ?", cid, botID).First(&ck).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			Fail(c, errs.NotFound("cookie 不存在"))
			return
		}
		Fail(c, err)
		return
	}
	var req struct {
		Domain   *string  `json:"domain"`
		Name     *string  `json:"name"`
		Value    *string  `json:"value"`
		Path     *string  `json:"path"`
		Expires  *float64 `json:"expires"` // 与 import/recovery 一致，接受浮点秒
		HTTPOnly *bool    `json:"httpOnly"`
		Secure   *bool    `json:"secure"`
		SameSite *string  `json:"sameSite"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	if req.Domain != nil {
		ck.Domain = *req.Domain
	}
	if req.Name != nil {
		ck.Name = *req.Name
	}
	if req.Value != nil {
		ck.Value = *req.Value
	}
	if req.Path != nil {
		ck.Path = *req.Path
	}
	if req.Expires != nil {
		ck.Expires = int64(*req.Expires)
	}
	if req.HTTPOnly != nil {
		ck.HTTPOnly = *req.HTTPOnly
	}
	if req.Secure != nil {
		ck.Secure = *req.Secure
	}
	if req.SameSite != nil {
		ck.SameSite = *req.SameSite
	}
	if err := s.db.WithContext(c.Request.Context()).Save(&ck).Error; err != nil {
		// 把 domain/name/path 改成与同 bot 下另一条相同会撞唯一索引，
		// 原始 SQL 错误对用户不可读，这里转成明确提示。
		if errors.Is(err, gorm.ErrDuplicatedKey) || strings.Contains(err.Error(), "UNIQUE constraint failed") {
			Fail(c, errs.BadRequest("同 bot 下已存在相同 domain + name + path 的 cookie"))
			return
		}
		Fail(c, errs.Wrap(err, "update browser cookie"))
		return
	}
	s.logger.Infow("browser cookie updated", "bot", botID, "id", cid, "domain", ck.Domain)
	OK(c, ck.ToView(false))
}

// handleDeleteBotBrowserCookie DELETE 删除单条。
func (s *Server) handleDeleteBotBrowserCookie(c *gin.Context) {
	botID := c.Param("id")
	cid := c.Param("cid")
	res := s.db.WithContext(c.Request.Context()).Where("id = ? AND bot_id = ?", cid, botID).Delete(&dao.BotBrowserCookie{})
	if res.Error != nil {
		Fail(c, errs.Wrap(res.Error, "delete browser cookie"))
		return
	}
	if res.RowsAffected == 0 {
		Fail(c, errs.NotFound("cookie 不存在"))
		return
	}
	s.logger.Infow("browser cookie deleted", "bot", botID, "id", cid)
	OK(c, nil)
}

// handleClearBotBrowserCookies DELETE 清空（?domain= 可按域清）。
func (s *Server) handleClearBotBrowserCookies(c *gin.Context) {
	botID := c.Param("id")
	domain := c.Query("domain")
	q := s.db.WithContext(c.Request.Context()).Where("bot_id = ?", botID)
	if domain != "" {
		q = q.Where("domain = ?", domain)
	}
	res := q.Delete(&dao.BotBrowserCookie{})
	if res.Error != nil {
		Fail(c, errs.Wrap(res.Error, "clear browser cookies"))
		return
	}
	s.logger.Infow("browser cookies cleared", "bot", botID, "domain", domain, "count", res.RowsAffected)
	OK(c, gin.H{"deleted": res.RowsAffected})
}

// handleImportBotBrowserCookies POST 批量导入。
// 支持四种格式（见 parseBrowserCookieImport）：
//  1. Playwright storageState JSON: {"cookies":[{...}],"origins":[...]}
//  2. Netscape cookies.txt（# Netscape HTTP Cookie File，TAB 分隔）
//  3. 浏览器扩展导出的 JSON 数组：[{name,value,domain,path,...}]
//  4. HTTP Cookie 请求头：k=v; k2=v2（DevTools Network → Headers → Cookie 直接复制；
//     该格式不含域名，需请求体额外带 domain）
func (s *Server) handleImportBotBrowserCookies(c *gin.Context) {
	botID := c.Param("id")
	var req struct {
		Raw    string `json:"raw"`
		Clear  bool   `json:"clear"`  // 导入前是否先清空
		Domain string `json:"domain"` // 仅 Cookie header 格式需要（该格式不含域名）
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		Fail(c, errs.BadRequest("invalid request body: "+err.Error()))
		return
	}
	if strings.TrimSpace(req.Raw) == "" {
		Fail(c, errs.BadRequest("raw 不能为空"))
		return
	}
	parsed, err := parseBrowserCookieImport(req.Raw, req.Domain)
	if err != nil {
		Fail(c, errs.BadRequest("解析失败: "+err.Error()))
		return
	}
	if len(parsed) == 0 {
		Fail(c, errs.BadRequest("未解析到任何 cookie"))
		return
	}
	if req.Clear {
		if err := s.db.WithContext(c.Request.Context()).Where("bot_id = ?", botID).Delete(&dao.BotBrowserCookie{}).Error; err != nil {
			Fail(c, errs.Wrap(err, "clear before import"))
			return
		}
	}
	now := time.Now().UTC()
	for i := range parsed {
		parsed[i].ID = idgen.New("bc")
		parsed[i].BotID = botID
		parsed[i].CreatedAt = now
		parsed[i].UpdatedAt = now
	}
	// 原子 upsert：(bot_id,domain,name,path) 是浏览器判定「同一 cookie」的键，且有 DB 唯一索引。
	// 同键重复导入视为「用新值覆盖」而非新增一行——同一 cookie 存多行会被 addCookies 注入成
	// 相互覆盖的重复项，登录态表现为随机失效。
	if err := s.db.WithContext(c.Request.Context()).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "bot_id"}, {Name: "domain"}, {Name: "name"}, {Name: "path"}},
		DoUpdates: clause.AssignmentColumns([]string{"value", "expires", "http_only", "secure", "same_site", "updated_at"}),
	}).Create(&parsed).Error; err != nil {
		Fail(c, errs.Wrap(err, "import browser cookies"))
		return
	}
	s.logger.Infow("browser cookies imported", "bot", botID, "count", len(parsed))
	OK(c, gin.H{"imported": len(parsed)})
}

// handleExportBotBrowserCookies GET 导出 storageState（?confirm=1 二次确认）。
func (s *Server) handleExportBotBrowserCookies(c *gin.Context) {
	botID := c.Param("id")
	if c.Query("confirm") != "1" {
		Fail(c, errs.BadRequest("导出账号凭据需 confirm=1 二次确认"))
		return
	}
	var cookies []dao.BotBrowserCookie
	if err := s.db.WithContext(c.Request.Context()).Where("bot_id = ?", botID).Find(&cookies).Error; err != nil {
		Fail(c, errs.Wrap(err, "export browser cookies"))
		return
	}
	s.logger.Infow("browser cookies exported", "bot", botID, "count", len(cookies))
	c.Header("Content-Disposition", "attachment; filename=\"browser-cookies.json\"")
	c.JSON(http.StatusOK, buildStorageState(cookies))
}

// ============================================================================
// 导入解析
// ============================================================================

// rawCookie 是多种来源的 cookie 通用解析结构。
type rawCookie struct {
	Name     string  `json:"name"`
	Value    string  `json:"value"`
	Domain   string  `json:"domain"`
	Path     string  `json:"path"`
	Expires  float64 `json:"expires"`        // Playwright storageState（浮点秒级时间戳）
	ExpDate  float64 `json:"expirationDate"` // Chrome 扩展导出（亦可能为浮点）
	HTTPOnly bool    `json:"httpOnly"`
	Secure   bool    `json:"secure"`
	SameSite string  `json:"sameSite"` // no_restriction / lax / strict / none / ""
}

// parseBrowserCookieImport 解析四种导入格式为 BotBrowserCookie 切片。
// defaultDomain 仅用于「Cookie header」格式（该格式不含域名信息），其余格式忽略此参数。
func parseBrowserCookieImport(raw string, defaultDomain string) ([]dao.BotBrowserCookie, error) {
	raw = strings.TrimSpace(raw)
	if isNetscape(raw) {
		return parseNetscapeCookies(raw)
	}
	// DevTools「Copy value」拿到的请求头形态：k=v; k=v; ...（既非 JSON 也非 TAB 分隔）
	if isCookieHeader(raw) {
		return parseCookieHeader(raw, defaultDomain)
	}
	return parseJSONCookies(raw)
}

// isCookieHeader 判断是否为 HTTP Cookie 请求头形态（DevTools Network → Headers → Cookie）。
// 特征：非 JSON 起始、无 TAB 分隔，且至少含一个 name=value 对。
// 允许开头带 "Cookie:" 前缀（直接从 DevTools 连字段名一起复制）。
func isCookieHeader(raw string) bool {
	s := strings.TrimSpace(strings.TrimPrefix(trimCookieHeaderPrefix(raw), "\n"))
	if s == "" {
		return false
	}
	if strings.HasPrefix(s, "{") || strings.HasPrefix(s, "[") || strings.Contains(s, "\t") {
		return false
	}
	// 必须存在 "=" 且等号前是合法 cookie 名（不含空格/分号）
	for _, seg := range strings.Split(s, ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		i := strings.Index(seg, "=")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(seg[:i])
		if name != "" && !strings.ContainsAny(name, " \t;") {
			return true
		}
	}
	return false
}

// trimCookieHeaderPrefix 去掉可能被一起复制进来的 "Cookie:" / "cookie:" 字段名前缀。
func trimCookieHeaderPrefix(raw string) string {
	s := strings.TrimSpace(raw)
	low := strings.ToLower(s)
	if strings.HasPrefix(low, "cookie:") {
		return strings.TrimSpace(s[len("cookie:"):])
	}
	return s
}

// parseCookieHeader 解析 "k=v; k2=v2" 请求头形态。
// 该格式只携带 name/value，没有域名/过期/属性信息，故：
//   - domain 取调用方传入的 defaultDomain（必填，否则无法注入到正确站点）；
//   - path 固定 "/"，expires=0（会话级），属性留空。
//
// 会跳过 cookie 属性关键字（Path/Domain/Expires/Max-Age/Secure/HttpOnly/SameSite），
// 因为用户可能误把 Set-Cookie 的内容贴进来。
func parseCookieHeader(raw string, defaultDomain string) ([]dao.BotBrowserCookie, error) {
	domain := strings.TrimSpace(defaultDomain)
	if domain == "" {
		return nil, fmt.Errorf("Cookie header 格式不含域名，请填写「适用域名」后再导入")
	}
	attrKeys := map[string]bool{
		"path": true, "domain": true, "expires": true, "max-age": true,
		"secure": true, "httponly": true, "samesite": true, "priority": true,
		"partitioned": true,
	}
	seen := make(map[string]bool)
	out := make([]dao.BotBrowserCookie, 0)
	for _, seg := range strings.Split(trimCookieHeaderPrefix(raw), ";") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		i := strings.Index(seg, "=")
		if i <= 0 {
			continue
		}
		name := strings.TrimSpace(seg[:i])
		value := strings.TrimSpace(seg[i+1:])
		if name == "" || strings.ContainsAny(name, " \t") {
			continue
		}
		if attrKeys[strings.ToLower(name)] {
			continue
		}
		// 同名后出现的覆盖前一个（与浏览器行为一致），并保持原有顺序
		if seen[name] {
			for j := range out {
				if out[j].Name == name {
					out[j].Value = value
					break
				}
			}
			continue
		}
		seen[name] = true
		out = append(out, dao.BotBrowserCookie{
			Domain: domain,
			Name:   name,
			Value:  value,
			Path:   "/",
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Cookie header 格式未解析到 name=value 对")
	}
	return out, nil
}

// isNetscape 判断是否为 Netscape cookies.txt 格式。
func isNetscape(raw string) bool {
	if strings.HasPrefix(raw, "# Netscape") {
		return true
	}
	// 含 TAB 且非 JSON 起始（storageState 以 { 起、扩展数组以 [ 起）
	if strings.Contains(raw, "\t") && !strings.HasPrefix(raw, "{") && !strings.HasPrefix(raw, "[") {
		return true
	}
	return false
}

// parseJSONCookies 解析 storageState（含 cookies 键）或纯数组 JSON。
func parseJSONCookies(raw string) ([]dao.BotBrowserCookie, error) {
	var withCookies struct {
		Cookies []rawCookie `json:"cookies"`
	}
	if err := json.Unmarshal([]byte(raw), &withCookies); err == nil && len(withCookies.Cookies) > 0 {
		return toCookies(withCookies.Cookies), nil
	}
	var arr []rawCookie
	if err := json.Unmarshal([]byte(raw), &arr); err == nil && len(arr) > 0 {
		return toCookies(arr), nil
	}
	return nil, fmt.Errorf("JSON 解析失败或为空（期望 storageState 或 cookie 数组）")
}

// parseNetscapeCookies 解析 Netscape cookies.txt。
// 每行：domain \t includeSubdomains \t path \t secure \t expires \t name \t value
func parseNetscapeCookies(raw string) ([]dao.BotBrowserCookie, error) {
	out := make([]dao.BotBrowserCookie, 0)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 7 {
			continue
		}
		out = append(out, dao.BotBrowserCookie{
			Domain:  f[0],
			Path:    f[2],
			Secure:  strings.EqualFold(f[3], "TRUE"),
			Expires: parseInt64(f[4]),
			Name:    f[5],
			Value:   f[6],
		})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("Netscape 格式未解析到 cookie")
	}
	return out, nil
}

// toCookies 把通用 rawCookie 转为 dao 模型，归一化 sameSite / path。
func toCookies(in []rawCookie) []dao.BotBrowserCookie {
	out := make([]dao.BotBrowserCookie, 0, len(in))
	for _, c := range in {
		// Playwright / Chrome 导出的 expires 为浮点秒，DB 以 int64 存储，截断取整。
		exp := int64(c.Expires)
		if exp == 0 {
			exp = int64(c.ExpDate)
		}
		ss := c.SameSite
		switch strings.ToLower(ss) {
		case "no_restriction", "none":
			ss = "None"
		case "strict":
			ss = "Strict"
		case "lax":
			ss = "Lax"
		default:
			ss = ""
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		out = append(out, dao.BotBrowserCookie{
			Domain:   c.Domain,
			Name:     c.Name,
			Value:    c.Value,
			Path:     path,
			Expires:  exp,
			HTTPOnly: c.HTTPOnly,
			Secure:   c.Secure,
			SameSite: ss,
		})
	}
	return out
}

// buildStorageState 把 DB cookie 组装为 Playwright storageState JSON。
func buildStorageState(cookies []dao.BotBrowserCookie) map[string]any {
	cs := make([]map[string]any, 0, len(cookies))
	for _, c := range cookies {
		cs = append(cs, map[string]any{
			"name":     c.Name,
			"value":    c.Value,
			"domain":   c.Domain,
			"path":     c.Path,
			"expires":  c.Expires,
			"httpOnly": c.HTTPOnly,
			"secure":   c.Secure,
			"sameSite": c.SameSite,
		})
	}
	return map[string]any{"cookies": cs, "origins": []any{}}
}

// parseInt64 解析 epoch 秒，失败返回 0（session cookie）。
func parseInt64(s string) int64 {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil {
		return 0
	}
	return v
}
