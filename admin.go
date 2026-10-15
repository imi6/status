package main

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

const sessionCookie = "status_session"
const sessionTTL = 7 * 24 * time.Hour

var afterConfigChange = func() { go refresh() }

var (
	sessionsMu sync.Mutex
	sessions   = map[string]time.Time{}
	loginMu    sync.Mutex
	loginFails = map[string]loginState{}
)

type loginState struct {
	fails     int
	lockUntil time.Time
}

func writeConfigFile(c Config) error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(configPath, b, 0600)
}

func cloneConfig(c Config) (Config, error) {
	b, err := json.Marshal(c)
	if err != nil {
		return Config{}, err
	}
	var out Config
	if err := json.Unmarshal(b, &out); err != nil {
		return Config{}, err
	}
	return out, nil
}

func updateConfig(mut func(*Config) error) error {
	mu.Lock()
	defer mu.Unlock()
	next, err := cloneConfig(cfg)
	if err != nil {
		return err
	}
	if err := mut(&next); err != nil {
		return err
	}
	normalizeConfig(&next)
	if err := saveMonitorGroups(next.Groups); err != nil {
		return err
	}
	stored := next
	stored.Groups = []Group{}
	if err := writeConfigFile(stored); err != nil {
		return err
	}
	cfg = next
	return nil
}

func cleanName(name, what string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errors.New(what + "不能为空")
	}
	if len([]rune(name)) > 80 {
		return "", errors.New(what + "过长")
	}
	if strings.ContainsAny(name, "|\r\n") {
		return "", errors.New(what + "包含无效字符")
	}
	return name, nil
}

func findGroup(c *Config, name string) int {
	for i := range c.Groups {
		if c.Groups[i].Name == name {
			return i
		}
	}
	return -1
}

func findSub(g *Group, name string) int {
	for i := range g.SubGroups {
		if g.SubGroups[i].Name == name {
			return i
		}
	}
	return -1
}

func addGroup(c *Config, name string, manual bool) error {
	name, err := cleanName(name, "分组名称")
	if err != nil {
		return err
	}
	if findGroup(c, name) >= 0 {
		return errors.New("分组已存在")
	}
	c.Groups = append(c.Groups, Group{Name: name, ManualRefresh: manual, SubGroups: []SubGroup{}})
	return nil
}

func updateGroup(c *Config, name, newName string, manual bool) error {
	name = strings.TrimSpace(name)
	idx := findGroup(c, name)
	if idx < 0 {
		return errors.New("分组不存在")
	}
	newName, err := cleanName(newName, "分组名称")
	if err != nil {
		return err
	}
	if newName != name && findGroup(c, newName) >= 0 {
		return errors.New("分组已存在")
	}
	c.Groups[idx].Name = newName
	c.Groups[idx].ManualRefresh = manual
	return nil
}

func deleteGroup(c *Config, name string) error {
	idx := findGroup(c, strings.TrimSpace(name))
	if idx < 0 {
		return errors.New("分组不存在")
	}
	c.Groups = append(c.Groups[:idx], c.Groups[idx+1:]...)
	return nil
}

func addSubGroup(c *Config, group, name, slug string) error {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	name, err := cleanName(name, "子组名称")
	if err != nil {
		return err
	}
	if findSub(&c.Groups[gi], name) >= 0 {
		return errors.New("子组已存在")
	}
	slug = strings.TrimSpace(slug)
	if strings.ContainsAny(slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	c.Groups[gi].SubGroups = append(c.Groups[gi].SubGroups, SubGroup{Name: name, KumaSlug: slug, Models: []Model{}})
	return nil
}

func updateSubGroup(c *Config, group, name, newName, slug string) error {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&c.Groups[gi], strings.TrimSpace(name))
	if si < 0 {
		return errors.New("子组不存在")
	}
	newName, err := cleanName(newName, "子组名称")
	if err != nil {
		return err
	}
	if newName != strings.TrimSpace(name) && findSub(&c.Groups[gi], newName) >= 0 {
		return errors.New("子组已存在")
	}
	slug = strings.TrimSpace(slug)
	if strings.ContainsAny(slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	c.Groups[gi].SubGroups[si].Name = newName
	c.Groups[gi].SubGroups[si].KumaSlug = slug
	return nil
}

func copySubGroup(c *Config, group, name, newName, slug string) error {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&c.Groups[gi], strings.TrimSpace(name))
	if si < 0 {
		return errors.New("子组不存在")
	}
	newName, err := cleanName(newName, "子组名称")
	if err != nil {
		return err
	}
	if findSub(&c.Groups[gi], newName) >= 0 {
		return errors.New("子组已存在")
	}
	slug = strings.TrimSpace(slug)
	if strings.ContainsAny(slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	src := c.Groups[gi].SubGroups[si]
	models := make([]Model, len(src.Models))
	copy(models, src.Models)
	c.Groups[gi].SubGroups = append(c.Groups[gi].SubGroups, SubGroup{Name: newName, KumaSlug: slug, Models: models})
	return nil
}

func deleteSubGroup(c *Config, group, name string) error {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&c.Groups[gi], strings.TrimSpace(name))
	if si < 0 {
		return errors.New("子组不存在")
	}
	g := &c.Groups[gi]
	g.SubGroups = append(g.SubGroups[:si], g.SubGroups[si+1:]...)
	return nil
}

func cleanModel(m *Model, prevKey string, creating bool, defaultBase string) error {
	m.ID = strings.TrimSpace(m.ID)
	m.Provider = strings.TrimSpace(m.Provider)
	m.BaseURL = strings.TrimRight(strings.TrimSpace(m.BaseURL), "/")
	if m.BaseURL == "" {
		m.BaseURL = strings.TrimSpace(defaultBase)
	}
	if m.BaseURL == "" {
		m.BaseURL = "https://zzshu.cc"
	}
	m.Key = strings.TrimSpace(m.Key)
	if m.ID == "" {
		return errors.New("模型 ID 不能为空")
	}
	if len([]rune(m.ID)) > 120 {
		return errors.New("模型 ID 过长")
	}
	if strings.ContainsAny(m.ID, "\"|\r\n") {
		return errors.New("模型 ID 不能包含引号、竖线或换行")
	}
	switch m.Provider {
	case "", "openai-compat", "openai-response", "google", "anthropic":
		if m.Provider == "" {
			m.Provider = "openai-compat"
		}
	default:
		return errors.New("不支持的协议")
	}
	base, err := normalizeBaseURL(m.Provider, m.BaseURL)
	if err != nil {
		return err
	}
	m.BaseURL = base
	if m.Key == "" {
		if creating || prevKey == "" {
			return errors.New("密钥不能为空")
		}
		m.Key = prevKey
	}
	if len(m.Key) > 4000 {
		return errors.New("密钥过长")
	}
	if m.Timeout == 0 {
		m.Timeout = 30
	}
	if m.Timeout < 1 || m.Timeout > 300 {
		return errors.New("超时需在 1 到 300 秒")
	}
	return nil
}

func hasModelID(sg SubGroup, id string, skip int) bool {
	for i := range sg.Models {
		if i == skip {
			continue
		}
		if sg.Models[i].ID == id {
			return true
		}
	}
	return false
}

func modelAt(c *Config, group, sub string, index int) (*Model, error) {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return nil, errors.New("分组不存在")
	}
	si := findSub(&c.Groups[gi], strings.TrimSpace(sub))
	if si < 0 {
		return nil, errors.New("子组不存在")
	}
	models := c.Groups[gi].SubGroups[si].Models
	if index < 0 || index >= len(models) {
		return nil, errors.New("模型不存在")
	}
	return &c.Groups[gi].SubGroups[si].Models[index], nil
}

func addModel(c *Config, group, sub string, m Model) error {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&c.Groups[gi], strings.TrimSpace(sub))
	if si < 0 {
		return errors.New("子组不存在")
	}
	if err := cleanModel(&m, "", true, c.DefaultBaseURL); err != nil {
		return err
	}
	if hasModelID(c.Groups[gi].SubGroups[si], m.ID, -1) {
		return errors.New("该子组已有相同模型 ID")
	}
	c.Groups[gi].SubGroups[si].Models = append(c.Groups[gi].SubGroups[si].Models, m)
	return nil
}

func updateModel(c *Config, group, sub string, index int, m Model) error {
	cur, err := modelAt(c, group, sub, index)
	if err != nil {
		return err
	}
	prev := cur.Key
	if err := cleanModel(&m, prev, false, c.DefaultBaseURL); err != nil {
		return err
	}
	gi := findGroup(c, strings.TrimSpace(group))
	si := findSub(&c.Groups[gi], strings.TrimSpace(sub))
	if hasModelID(c.Groups[gi].SubGroups[si], m.ID, index) {
		return errors.New("该子组已有相同模型 ID")
	}
	*cur = m
	return nil
}

func deleteModel(c *Config, group, sub string, index int) error {
	gi := findGroup(c, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&c.Groups[gi], strings.TrimSpace(sub))
	if si < 0 {
		return errors.New("子组不存在")
	}
	models := c.Groups[gi].SubGroups[si].Models
	if index < 0 || index >= len(models) {
		return errors.New("模型不存在")
	}
	c.Groups[gi].SubGroups[si].Models = append(models[:index], models[index+1:]...)
	return nil
}

func cleanAdminPath(raw string) (string, error) {
	name := strings.Trim(strings.TrimSpace(raw), "/")
	if name == "" {
		return "", nil
	}
	if strings.Contains(name, "/") {
		return "", errors.New("后台路径只能是一级目录")
	}
	if len(name) < 8 || len(name) > 64 {
		return "", errors.New("后台路径长度需在 8 到 64")
	}
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			continue
		}
		return "", errors.New("后台路径只能包含字母、数字、连字符和下划线")
	}
	switch strings.ToLower(name) {
	case "admin", "api", "login", "manage", "manager", "dashboard", "console", "status", "config":
		return "", errors.New("后台路径过于常见，请换成不容易猜到的名称")
	}
	return name, nil
}

func mountAdmin(mux *http.ServeMux, raw string) (string, error) {
	name, err := cleanAdminPath(raw)
	if err != nil || name == "" {
		return "", err
	}
	base := "/" + name
	mux.HandleFunc("GET "+base, serveAdminPage)
	mux.HandleFunc("GET "+base+"/{$}", serveAdminPage)
	mux.HandleFunc(base+"/api/{action}", apiAdmin)
	return base, nil
}

func serveAdminPage(w http.ResponseWriter, r *http.Request) {
	serveAsset(w, r, "admin.html")
}

func cleanTelegram(t TelegramConfig) (TelegramConfig, error) {
	t.BotToken = strings.TrimSpace(t.BotToken)
	t.ChatID = strings.TrimSpace(t.ChatID)
	if t.BotToken == "" && t.ChatID == "" {
		return t, nil
	}
	if t.BotToken == "" || t.ChatID == "" {
		return t, errors.New("Telegram 需要同时填写 Bot Token 和群组 ID")
	}
	if len(t.BotToken) > 200 || strings.ContainsAny(t.BotToken, " \r\n") || !strings.Contains(t.BotToken, ":") {
		return t, errors.New("Bot Token 格式不正确")
	}
	if !validTelegramChat(t.ChatID) {
		return t, errors.New("群组 ID 需为数字，或以 @ 开头的频道用户名")
	}
	return t, nil
}

func validTelegramChat(id string) bool {
	if strings.HasPrefix(id, "@") {
		name := id[1:]
		if len(name) < 5 || len(name) > 32 {
			return false
		}
		for i, r := range name {
			ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_'
			if !ok || (i == 0 && r >= '0' && r <= '9') {
				return false
			}
		}
		return true
	}
	rest := id
	if strings.HasPrefix(rest, "-") {
		rest = rest[1:]
	}
	if rest == "" || len(rest) > 20 {
		return false
	}
	for _, r := range rest {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func applySettings(c *Config, port, interval, cooldown int, adminPath, defaultBase string, front FrontendConfig, kuma KumaConfig, telegram TelegramConfig) error {
	if port < 1 || port > 65535 {
		return errors.New("端口需在 1 到 65535")
	}
	if interval < 5 || interval > 86400 {
		return errors.New("检测间隔需在 5 到 86400 秒")
	}
	if cooldown < 5 || cooldown > 86400 {
		return errors.New("冷却时间需在 5 到 86400 秒")
	}
	front.Title = strings.TrimSpace(front.Title)
	if front.Title == "" {
		return errors.New("标题不能为空")
	}
	kuma.BaseURL = strings.TrimRight(strings.TrimSpace(kuma.BaseURL), "/")
	kuma.Slug = strings.TrimSpace(kuma.Slug)
	if kuma.BaseURL != "" {
		u, err := url.Parse(kuma.BaseURL)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return errors.New("Kuma 地址需以 http:// 或 https:// 开头")
		}
	}
	if strings.ContainsAny(kuma.Slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	pathName, err := cleanAdminPath(adminPath)
	if err != nil {
		return err
	}
	base, err := cleanDefaultBase(defaultBase)
	if err != nil {
		return err
	}
	telegram, err = cleanTelegram(telegram)
	if err != nil {
		return err
	}
	c.Port = port
	c.Interval = interval
	c.ManualRefreshCooldown = cooldown
	c.AdminPath = pathName
	c.DefaultBaseURL = base
	c.Frontend = front
	c.Kuma = kuma
	c.Telegram = telegram
	return nil
}

func passwordMatch(got, want string) bool {
	if want == "" {
		return false
	}
	g := sha256.Sum256([]byte(got))
	w := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(g[:], w[:]) == 1
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func loginLocked(ip string) (bool, int) {
	loginMu.Lock()
	defer loginMu.Unlock()
	st := loginFails[ip]
	if time.Now().Before(st.lockUntil) {
		return true, int(time.Until(st.lockUntil).Seconds()) + 1
	}
	return false, 0
}

func markLoginFailure(ip string) (bool, int) {
	loginMu.Lock()
	defer loginMu.Unlock()
	st := loginFails[ip]
	now := time.Now()
	if now.Before(st.lockUntil) {
		return true, int(st.lockUntil.Sub(now).Seconds()) + 1
	}
	st.fails++
	if st.fails >= 8 {
		st.fails = 0
		st.lockUntil = now.Add(time.Minute)
		loginFails[ip] = st
		return true, 60
	}
	loginFails[ip] = st
	return false, 0
}

func clearLoginFailure(ip string) {
	loginMu.Lock()
	delete(loginFails, ip)
	loginMu.Unlock()
}

func originOK(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

func newSession() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	sessionsMu.Lock()
	sessions[token] = time.Now().Add(sessionTTL)
	sessionsMu.Unlock()
	return token, nil
}

func sessionValid(token string) bool {
	if token == "" {
		return false
	}
	sessionsMu.Lock()
	defer sessionsMu.Unlock()
	exp, ok := sessions[token]
	if !ok || time.Now().After(exp) {
		delete(sessions, token)
		return false
	}
	return true
}

func dropSession(token string) {
	sessionsMu.Lock()
	delete(sessions, token)
	sessionsMu.Unlock()
}

func readSession(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func setSessionCookie(w http.ResponseWriter, token string, maxAge int) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   maxAge,
	})
}

func writeAdminJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("cache-control", "no-store")
	w.WriteHeader(code)
	b, _ := json.Marshal(v)
	w.Write(b)
}

func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(dst); err != nil {
		writeAdminJSON(w, 400, map[string]string{"error": "请求格式不正确"})
		return false
	}
	return true
}

func requireAdmin(w http.ResponseWriter, r *http.Request) bool {
	if !sessionValid(readSession(r)) {
		writeAdminJSON(w, 401, map[string]string{"error": "未登录"})
		return false
	}
	return true
}

func adminError(w http.ResponseWriter, err error) {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		writeAdminJSON(w, 400, map[string]string{"error": "请求格式不正确"})
		return
	}
	msg := err.Error()
	if strings.Contains(msg, "permission denied") || strings.Contains(msg, "Access is denied") || strings.Contains(msg, "read-only") {
		msg = "无法写入 config.json，请确认文件可写"
	}
	writeAdminJSON(w, 400, map[string]string{"error": msg})
}

func finishChange(w http.ResponseWriter, err error, probe bool) {
	if err != nil {
		adminError(w, err)
		return
	}
	if probe {
		afterConfigChange()
	}
	writeAdminJSON(w, 200, map[string]bool{"ok": true})
}

func renameGroupHistory(oldName, newName string) {
	if db == nil || oldName == "" || oldName == newName {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET group_name = ? WHERE group_name = ?`, newName, oldName); err != nil {
		log.Println("rename group history:", err)
	}
}

func renameSubHistory(group, oldName, newName string) {
	if db == nil || oldName == newName {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET sub_name = ? WHERE group_name = ? AND sub_name = ?`, newName, group, oldName); err != nil {
		log.Println("rename subgroup history:", err)
	}
}

func renameModelHistory(group, sub, oldID, newID string) {
	if db == nil || oldID == newID {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET model_id = ? WHERE group_name = ? AND sub_name = ? AND model_id = ?`, newID, group, sub, oldID); err != nil {
		log.Println("rename model history:", err)
	}
}

func apiAdmin(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if !originOK(r) {
		writeAdminJSON(w, 403, map[string]string{"error": "拒绝跨站请求"})
		return
	}
	path := r.PathValue("action")
	if path == "" {
		const marker = "/api/"
		if i := strings.LastIndex(r.URL.Path, marker); i >= 0 {
			path = strings.Trim(r.URL.Path[i+len(marker):], "/")
		}
	}
	switch path {
	case "login":
		if r.Method != http.MethodPost {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		adminLogin(w, r)
	case "logout":
		if r.Method != http.MethodPost {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		adminLogout(w, r)
	case "session":
		if r.Method != http.MethodGet {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		adminSession(w, r)
	case "config":
		if !requireAdmin(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		adminGetConfig(w, r)
	case "settings":
		if !requireAdmin(w, r) {
			return
		}
		if r.Method != http.MethodPut {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		adminSaveSettings(w, r)
	case "password":
		if !requireAdmin(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		adminChangePassword(w, r)
	case "groups":
		if !requireAdmin(w, r) {
			return
		}
		adminGroups(w, r)
	case "subgroups":
		if !requireAdmin(w, r) {
			return
		}
		adminSubGroups(w, r)
	case "models":
		if !requireAdmin(w, r) {
			return
		}
		adminModels(w, r)
	case "upstream-models":
		if !requireAdmin(w, r) {
			return
		}
		adminUpstreamModels(w, r)
	case "telegram-test":
		if !requireAdmin(w, r) {
			return
		}
		adminTelegramTest(w, r)
	default:
		writeAdminJSON(w, 404, map[string]string{"error": "not found"})
	}
}

func adminLogin(w http.ResponseWriter, r *http.Request) {
	loadConfig()
	ip := clientIP(r)
	if locked, wait := loginLocked(ip); locked {
		writeAdminJSON(w, 429, map[string]any{"error": "尝试过多，请稍后再试", "retry": wait})
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	mu.RLock()
	want := cfg.AdminPassword
	mu.RUnlock()
	if want == "" {
		writeAdminJSON(w, 403, map[string]string{"error": "未设置 adminPassword，后台已关闭"})
		return
	}
	if !passwordMatch(body.Password, want) {
		if locked, wait := markLoginFailure(ip); locked {
			writeAdminJSON(w, 429, map[string]any{"error": "尝试过多，请稍后再试", "retry": wait})
			return
		}
		writeAdminJSON(w, 401, map[string]string{"error": "密码不正确"})
		return
	}
	clearLoginFailure(ip)
	token, err := newSession()
	if err != nil {
		writeAdminJSON(w, 500, map[string]string{"error": "无法创建会话"})
		return
	}
	setSessionCookie(w, token, int(sessionTTL.Seconds()))
	writeAdminJSON(w, 200, map[string]bool{"ok": true})
}

func adminLogout(w http.ResponseWriter, r *http.Request) {
	dropSession(readSession(r))
	setSessionCookie(w, "", -1)
	writeAdminJSON(w, 200, map[string]bool{"ok": true})
}

func adminSession(w http.ResponseWriter, r *http.Request) {
	loadConfig()
	mu.RLock()
	enabled := cfg.AdminPassword != ""
	mu.RUnlock()
	writeAdminJSON(w, 200, map[string]bool{
		"enabled": enabled,
		"ok":      enabled && sessionValid(readSession(r)),
	})
}

type adminConfigView struct {
	Port                  int            `json:"port"`
	Interval              int            `json:"interval"`
	ManualRefreshCooldown int            `json:"manualRefreshCooldown"`
	AdminPath             string         `json:"adminPath"`
	DefaultBaseURL        string         `json:"defaultBaseURL"`
	Frontend              FrontendConfig `json:"frontend"`
	Kuma                  KumaConfig     `json:"kuma"`
	Telegram              TelegramConfig `json:"telegram"`
	Groups                []Group        `json:"groups"`
}

func adminGetConfig(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	view := adminConfigView{
		Port:                  cfg.Port,
		Interval:              cfg.Interval,
		ManualRefreshCooldown: cfg.ManualRefreshCooldown,
		AdminPath:             cfg.AdminPath,
		DefaultBaseURL:        cfg.DefaultBaseURL,
		Frontend:              cfg.Frontend,
		Kuma:                  cfg.Kuma,
		Telegram:              cfg.Telegram,
		Groups:                cfg.Groups,
	}
	mu.RUnlock()
	if view.Groups == nil {
		view.Groups = []Group{}
	}
	writeAdminJSON(w, 200, view)
}

func adminSaveSettings(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port                  int            `json:"port"`
		Interval              int            `json:"interval"`
		ManualRefreshCooldown int            `json:"manualRefreshCooldown"`
		AdminPath             string         `json:"adminPath"`
		DefaultBaseURL        string         `json:"defaultBaseURL"`
		Frontend              FrontendConfig `json:"frontend"`
		Kuma                  KumaConfig     `json:"kuma"`
		Telegram              TelegramConfig `json:"telegram"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	err := updateConfig(func(c *Config) error {
		return applySettings(c, body.Port, body.Interval, body.ManualRefreshCooldown, body.AdminPath, body.DefaultBaseURL, body.Frontend, body.Kuma, body.Telegram)
	})
	finishChange(w, err, false)
}

func adminTelegramTest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var body TelegramConfig
	if !decodeBody(w, r, &body) {
		return
	}
	cleaned, err := cleanTelegram(body)
	if err != nil {
		writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	if cleaned.BotToken == "" {
		writeAdminJSON(w, 400, map[string]string{"error": "请先填写 Bot Token 和群组 ID"})
		return
	}
	if err := sendTelegram(cleaned.BotToken, cleaned.ChatID, "监控已连接到这个群。之后只在分组状态变化时推送。"); err != nil {
		writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeAdminJSON(w, 200, map[string]bool{"ok": true})
}

func adminChangePassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Current string `json:"current"`
		Next    string `json:"next"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	if len([]rune(strings.TrimSpace(body.Next))) < 6 {
		writeAdminJSON(w, 400, map[string]string{"error": "新密码至少 6 位"})
		return
	}
	ip := clientIP(r)
	if locked, wait := loginLocked(ip); locked {
		writeAdminJSON(w, 429, map[string]any{"error": "尝试过多，请稍后再试", "retry": wait})
		return
	}
	mu.RLock()
	current := cfg.AdminPassword
	mu.RUnlock()
	if !passwordMatch(body.Current, current) {
		markLoginFailure(ip)
		writeAdminJSON(w, 401, map[string]string{"error": "当前密码不正确"})
		return
	}
	clearLoginFailure(ip)
	next := body.Next
	err := updateConfig(func(c *Config) error {
		c.AdminPassword = next
		return nil
	})
	finishChange(w, err, false)
}

type groupBody struct {
	Name          string `json:"name"`
	NewName       string `json:"newName"`
	ManualRefresh bool   `json:"manualRefresh"`
}

func adminGroups(w http.ResponseWriter, r *http.Request) {
	var body groupBody
	if !decodeBody(w, r, &body) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		finishChange(w, updateConfig(func(c *Config) error {
			return addGroup(c, body.Name, body.ManualRefresh)
		}), true)
	case http.MethodPut:
		newName := body.NewName
		if strings.TrimSpace(newName) == "" {
			newName = body.Name
		}
		err := updateConfig(func(c *Config) error {
			return updateGroup(c, body.Name, newName, body.ManualRefresh)
		})
		if err == nil {
			renameGroupHistory(strings.TrimSpace(body.Name), strings.TrimSpace(newName))
		}
		finishChange(w, err, true)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteGroup(c, body.Name)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

type subBody struct {
	Group    string `json:"group"`
	Name     string `json:"name"`
	NewName  string `json:"newName"`
	KumaSlug string `json:"kumaSlug"`
	Copy     bool   `json:"copy"`
}

func adminSubGroups(w http.ResponseWriter, r *http.Request) {
	var body subBody
	if !decodeBody(w, r, &body) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		err := updateConfig(func(c *Config) error {
			if body.Copy {
				return copySubGroup(c, body.Group, body.Name, body.NewName, body.KumaSlug)
			}
			return addSubGroup(c, body.Group, body.Name, body.KumaSlug)
		})
		finishChange(w, err, true)
	case http.MethodPut:
		newName := body.NewName
		if strings.TrimSpace(newName) == "" {
			newName = body.Name
		}
		err := updateConfig(func(c *Config) error {
			return updateSubGroup(c, body.Group, body.Name, newName, body.KumaSlug)
		})
		if err == nil {
			renameSubHistory(strings.TrimSpace(body.Group), strings.TrimSpace(body.Name), strings.TrimSpace(newName))
		}
		finishChange(w, err, true)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteSubGroup(c, body.Group, body.Name)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

type modelBody struct {
	Group    string `json:"group"`
	Subgroup string `json:"subgroup"`
	Index    int    `json:"index"`
	Model    Model  `json:"model"`
}

func adminModels(w http.ResponseWriter, r *http.Request) {
	var body modelBody
	if !decodeBody(w, r, &body) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		finishChange(w, updateConfig(func(c *Config) error {
			return addModel(c, body.Group, body.Subgroup, body.Model)
		}), true)
	case http.MethodPut:
		var oldID string
		err := updateConfig(func(c *Config) error {
			cur, err := modelAt(c, body.Group, body.Subgroup, body.Index)
			if err != nil {
				return err
			}
			oldID = cur.ID
			return updateModel(c, body.Group, body.Subgroup, body.Index, body.Model)
		})
		if err == nil {
			renameModelHistory(strings.TrimSpace(body.Group), strings.TrimSpace(body.Subgroup), oldID, strings.TrimSpace(body.Model.ID))
		}
		finishChange(w, err, true)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteModel(c, body.Group, body.Subgroup, body.Index)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func adminUpstreamModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		Provider string `json:"provider"`
		BaseURL  string `json:"baseURL"`
		Key      string `json:"key"`
		Group    string `json:"group"`
		Subgroup string `json:"subgroup"`
		Index    int    `json:"index"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	key := strings.TrimSpace(body.Key)
	if key == "" {
		mu.RLock()
		cur, err := modelAt(&cfg, body.Group, body.Subgroup, body.Index)
		if err == nil {
			key = cur.Key
		}
		mu.RUnlock()
	}
	if key == "" {
		writeAdminJSON(w, 400, map[string]string{"error": "密钥不能为空"})
		return
	}
	baseURL := strings.TrimSpace(body.BaseURL)
	if baseURL == "" {
		mu.RLock()
		baseURL = cfg.DefaultBaseURL
		mu.RUnlock()
	}
	ids, err := listUpstreamModels(body.Provider, baseURL, key)
	if err != nil {
		writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeAdminJSON(w, 200, map[string]any{"models": ids})
}

func listUpstreamModels(provider, baseURL, key string) ([]string, error) {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai-compat"
	}
	switch provider {
	case "openai-compat", "openai-response", "google", "anthropic":
	default:
		return nil, errors.New("不支持的协议")
	}
	endpoint, err := upstreamModelsURL(provider, baseURL)
	if err != nil {
		return nil, err
	}
	client := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return errors.New("重定向过多")
			}
			if req.URL.User != nil || (req.URL.Scheme != "http" && req.URL.Scheme != "https") || req.URL.Host == "" {
				return errors.New("重定向地址无效")
			}
			return nil
		},
	}

	seen := map[string]struct{}{}
	var ids []string
	pageURL := endpoint
	for page := 0; page < 5 && len(ids) < 300; page++ {
		req, err := http.NewRequest(http.MethodGet, pageURL, nil)
		if err != nil {
			return nil, errors.New("地址无效")
		}
		switch provider {
		case "google":
			q := req.URL.Query()
			q.Set("key", key)
			req.URL.RawQuery = q.Encode()
		case "anthropic":
			req.Header.Set("x-api-key", key)
			req.Header.Set("anthropic-version", "2023-06-01")
		default:
			req.Header.Set("authorization", "Bearer "+key)
		}
		resp, err := client.Do(req)
		if err != nil {
			return nil, errors.New("无法连接模型接口")
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		resp.Body.Close()
		if readErr != nil {
			return nil, errors.New("读取模型列表失败")
		}
		if resp.StatusCode >= 300 {
			return nil, fmt.Errorf("模型接口返回 %d，请检查地址和密钥", resp.StatusCode)
		}
		batch, hasMore, lastID := parseUpstreamModelPage(provider, raw)
		added := 0
		for _, id := range batch {
			if _, ok := seen[id]; ok || !acceptableModelID(id) {
				continue
			}
			seen[id] = struct{}{}
			ids = append(ids, id)
			added++
			if len(ids) >= 300 {
				break
			}
		}
		if !hasMore || lastID == "" || added == 0 || len(ids) >= 300 {
			break
		}
		next, err := url.Parse(endpoint)
		if err != nil {
			break
		}
		q := next.Query()
		q.Set("after_id", lastID)
		next.RawQuery = q.Encode()
		pageURL = next.String()
	}
	if len(ids) == 0 {
		return nil, errors.New("接口没有返回可选模型")
	}
	sort.Strings(ids)
	return ids, nil
}

func upstreamModelsURL(provider, baseURL string) (string, error) {
	base, err := normalizeBaseURL(provider, baseURL)
	if err != nil {
		return "", err
	}
	return base + "/models", nil
}

func normalizeBaseURL(provider, raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("地址需以 http:// 或 https:// 开头")
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "openai-compat"
	}
	path := stripAPISuffix(strings.TrimRight(u.Path, "/"))
	switch provider {
	case "openai-compat", "openai-response", "anthropic":
		for strings.HasSuffix(path, "/v1/v1") {
			path = strings.TrimSuffix(path, "/v1")
		}
		if !strings.HasSuffix(path, "/v1") {
			path += "/v1"
		}
	case "google":
		if path == "" {
			path = "/v1beta"
		}
	default:
		return "", errors.New("不支持的协议")
	}
	u.Path = path
	u.RawQuery = ""
	u.Fragment = ""
	out := strings.TrimRight(u.String(), "/")
	if len(out) > 500 {
		return "", errors.New("地址过长")
	}
	return out, nil
}

func cleanDefaultBase(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		raw = "https://zzshu.cc"
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("默认接口地址需是网址，例如 https://zzshu.cc")
	}
	u.RawQuery = ""
	u.Fragment = ""
	out := strings.TrimRight(u.String(), "/")
	if len(out) > 500 {
		return "", errors.New("默认接口地址过长")
	}
	return out, nil
}

func stripAPISuffix(path string) string {
	suffixes := []string{"/chat/completions", "/completions", "/responses", "/messages", "/models"}
	for {
		changed := false
		for _, suffix := range suffixes {
			if strings.HasSuffix(path, suffix) {
				path = strings.TrimRight(strings.TrimSuffix(path, suffix), "/")
				changed = true
			}
		}
		if !changed {
			return path
		}
	}
}

func parseUpstreamModelPage(provider string, raw []byte) (ids []string, hasMore bool, lastID string) {
	var page struct {
		Data []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"data"`
		Models []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"models"`
		HasMore bool   `json:"has_more"`
		LastID  string `json:"last_id"`
	}
	if json.Unmarshal(raw, &page) != nil {
		return nil, false, ""
	}
	for _, item := range page.Data {
		ids = append(ids, normalizeUpstreamModelID(provider, item.ID, item.Name))
	}
	for _, item := range page.Models {
		ids = append(ids, normalizeUpstreamModelID(provider, item.ID, item.Name))
	}
	return ids, page.HasMore, strings.TrimSpace(page.LastID)
}

func normalizeUpstreamModelID(provider, id, name string) string {
	id = strings.TrimSpace(id)
	if id == "" {
		id = strings.TrimSpace(name)
	}
	if provider == "google" {
		id = strings.TrimPrefix(id, "models/")
	}
	return id
}

func acceptableModelID(id string) bool {
	if id == "" || len([]rune(id)) > 120 {
		return false
	}
	return !strings.ContainsAny(id, "\"|\r\n")
}
