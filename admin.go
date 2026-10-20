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

var afterConfigChange = func() { go refresh(nil) }

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
	if len(next.Sites) == 0 {
		next.Sites = configSites(next)
	}
	if err := saveMonitorSites(next.Sites); err != nil {
		return err
	}
	stored := next
	stored.Groups = []Group{}
	stored.Sites = nil
	if err := writeConfigFile(stored); err != nil {
		return err
	}
	cfg = next
	noteScheduleChange()
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

func cleanRatio(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if len([]rune(v)) > 16 {
		return "", errors.New("倍率过长")
	}
	if strings.ContainsAny(v, "<>\r\n") {
		return "", errors.New("倍率包含无效字符")
	}
	return v, nil
}

func groupsAt(c *Config, site string) (*[]Group, error) {
	site = strings.TrimSpace(site)
	if len(c.Sites) == 0 {
		if c.Groups == nil {
			c.Groups = []Group{}
		}
		return &c.Groups, nil
	}
	if site == "" {
		if len(c.Sites) == 1 {
			if c.Sites[0].Groups == nil {
				c.Sites[0].Groups = []Group{}
			}
			return &c.Sites[0].Groups, nil
		}
		return nil, errors.New("请选择站点")
	}
	for i := range c.Sites {
		if c.Sites[i].Name == site {
			if c.Sites[i].Groups == nil {
				c.Sites[i].Groups = []Group{}
			}
			return &c.Sites[i].Groups, nil
		}
	}
	return nil, errors.New("站点不存在")
}

func findGroup(groups []Group, name string) int {
	for i := range groups {
		if groups[i].Name == name {
			return i
		}
	}
	return -1
}

func addSite(c *Config, name string, interval int) error {
	name, err := cleanName(name, "站点名称")
	if err != nil {
		return err
	}
	interval, err = cleanSiteInterval(interval)
	if err != nil {
		return err
	}
	for _, site := range c.Sites {
		if site.Name == name {
			return errors.New("站点已存在")
		}
	}
	if c.Sites == nil {
		c.Sites = []Site{}
	}
	c.Sites = append(c.Sites, Site{Name: name, Interval: interval, Groups: []Group{}})
	return nil
}

func updateSite(c *Config, name, newName string, interval int) error {
	name = strings.TrimSpace(name)
	idx := -1
	for i := range c.Sites {
		if c.Sites[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("站点不存在")
	}
	newName, err := cleanName(newName, "站点名称")
	if err != nil {
		return err
	}
	interval, err = cleanSiteInterval(interval)
	if err != nil {
		return err
	}
	if newName != name {
		for _, site := range c.Sites {
			if site.Name == newName {
				return errors.New("站点已存在")
			}
		}
	}
	c.Sites[idx].Name = newName
	c.Sites[idx].Interval = interval
	return nil
}

func deleteSite(c *Config, name string) error {
	name = strings.TrimSpace(name)
	idx := -1
	for i := range c.Sites {
		if c.Sites[i].Name == name {
			idx = i
			break
		}
	}
	if idx < 0 {
		return errors.New("站点不存在")
	}
	if len(c.Sites) == 1 {
		return errors.New("至少保留一个站点")
	}
	c.Sites = append(c.Sites[:idx], c.Sites[idx+1:]...)
	return nil
}

func reorderSites(c *Config, names []string) error {
	if len(names) != len(c.Sites) {
		return errors.New("排序不完整")
	}
	byName := make(map[string]Site, len(c.Sites))
	for _, site := range c.Sites {
		byName[site.Name] = site
	}
	next := make([]Site, 0, len(names))
	seen := map[string]bool{}
	for _, name := range names {
		name = strings.TrimSpace(name)
		site, ok := byName[name]
		if !ok || seen[name] {
			return errors.New("排序包含未知站点")
		}
		seen[name] = true
		next = append(next, site)
	}
	c.Sites = next
	return nil
}

func findSub(g *Group, name string) int {
	for i := range g.SubGroups {
		if g.SubGroups[i].Name == name {
			return i
		}
	}
	return -1
}

func addGroup(c *Config, site, name string, manual bool) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	name, err = cleanName(name, "分组名称")
	if err != nil {
		return err
	}
	if findGroup(*groups, name) >= 0 {
		return errors.New("分组已存在")
	}
	*groups = append(*groups, Group{Name: name, ManualRefresh: manual, SubGroups: []SubGroup{}})
	return nil
}

func updateGroup(c *Config, site, name, newName string, manual bool) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	name = strings.TrimSpace(name)
	idx := findGroup(*groups, name)
	if idx < 0 {
		return errors.New("分组不存在")
	}
	newName, err = cleanName(newName, "分组名称")
	if err != nil {
		return err
	}
	if newName != name && findGroup(*groups, newName) >= 0 {
		return errors.New("分组已存在")
	}
	(*groups)[idx].Name = newName
	(*groups)[idx].ManualRefresh = manual
	return nil
}

func reorderGroups(c *Config, site string, names []string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	if len(names) != len(*groups) {
		return errors.New("排序不完整")
	}
	byName := make(map[string]Group, len(*groups))
	for _, g := range *groups {
		byName[g.Name] = g
	}
	next := make([]Group, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		g, ok := byName[name]
		if !ok || seen[name] {
			return errors.New("排序包含未知分组")
		}
		seen[name] = true
		next = append(next, g)
	}
	*groups = next
	return nil
}

func deleteGroup(c *Config, site, name string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	idx := findGroup(*groups, strings.TrimSpace(name))
	if idx < 0 {
		return errors.New("分组不存在")
	}
	*groups = append((*groups)[:idx], (*groups)[idx+1:]...)
	return nil
}

func addSubGroup(c *Config, site, group, name, slug, ratio string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	name, err = cleanName(name, "子组名称")
	if err != nil {
		return err
	}
	if findSub(&(*groups)[gi], name) >= 0 {
		return errors.New("子组已存在")
	}
	slug = strings.TrimSpace(slug)
	if strings.ContainsAny(slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	ratio, err = cleanRatio(ratio)
	if err != nil {
		return err
	}
	(*groups)[gi].SubGroups = append((*groups)[gi].SubGroups, SubGroup{Name: name, KumaSlug: slug, Ratio: ratio, Models: []Model{}})
	return nil
}

func updateSubGroup(c *Config, site, group, name, newName, slug, ratio string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&(*groups)[gi], strings.TrimSpace(name))
	if si < 0 {
		return errors.New("子组不存在")
	}
	newName, err = cleanName(newName, "子组名称")
	if err != nil {
		return err
	}
	if newName != strings.TrimSpace(name) && findSub(&(*groups)[gi], newName) >= 0 {
		return errors.New("子组已存在")
	}
	slug = strings.TrimSpace(slug)
	if strings.ContainsAny(slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	ratio, err = cleanRatio(ratio)
	if err != nil {
		return err
	}
	(*groups)[gi].SubGroups[si].Name = newName
	(*groups)[gi].SubGroups[si].KumaSlug = slug
	(*groups)[gi].SubGroups[si].Ratio = ratio
	return nil
}

func copySubGroup(c *Config, site, group, name, newName, slug, ratio string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&(*groups)[gi], strings.TrimSpace(name))
	if si < 0 {
		return errors.New("子组不存在")
	}
	newName, err = cleanName(newName, "子组名称")
	if err != nil {
		return err
	}
	if findSub(&(*groups)[gi], newName) >= 0 {
		return errors.New("子组已存在")
	}
	slug = strings.TrimSpace(slug)
	if strings.ContainsAny(slug, " \r\n") {
		return errors.New("Kuma slug 不能包含空格")
	}
	ratio, err = cleanRatio(ratio)
	if err != nil {
		return err
	}
	src := (*groups)[gi].SubGroups[si]
	models := make([]Model, len(src.Models))
	copy(models, src.Models)
	(*groups)[gi].SubGroups = append((*groups)[gi].SubGroups, SubGroup{Name: newName, KumaSlug: slug, Ratio: ratio, Models: models})
	return nil
}

func deleteSubGroup(c *Config, site, group, name string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&(*groups)[gi], strings.TrimSpace(name))
	if si < 0 {
		return errors.New("子组不存在")
	}
	g := &(*groups)[gi]
	g.SubGroups = append(g.SubGroups[:si], g.SubGroups[si+1:]...)
	return nil
}

func reorderSubGroups(c *Config, site, group string, names []string) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	group = strings.TrimSpace(group)
	gi := findGroup(*groups, group)
	if gi < 0 {
		return errors.New("分组不存在")
	}
	subs := (*groups)[gi].SubGroups
	if len(names) != len(subs) {
		return errors.New("排序不完整")
	}
	byName := make(map[string]SubGroup, len(subs))
	for _, sg := range subs {
		byName[sg.Name] = sg
	}
	next := make([]SubGroup, 0, len(names))
	seen := make(map[string]bool, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		sg, ok := byName[name]
		if !ok || seen[name] {
			return errors.New("排序包含未知子组")
		}
		seen[name] = true
		next = append(next, sg)
	}
	(*groups)[gi].SubGroups = next
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
	case "", "openai-compat", "openai-response", "openai-image", "google", "google-image", "anthropic":
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

func modelAt(c *Config, site, group, sub string, index int) (*Model, error) {
	groups, err := groupsAt(c, site)
	if err != nil {
		return nil, err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return nil, errors.New("分组不存在")
	}
	si := findSub(&(*groups)[gi], strings.TrimSpace(sub))
	if si < 0 {
		return nil, errors.New("子组不存在")
	}
	models := (*groups)[gi].SubGroups[si].Models
	if index < 0 || index >= len(models) {
		return nil, errors.New("模型不存在")
	}
	return &(*groups)[gi].SubGroups[si].Models[index], nil
}

func testConfiguredModel(site, group, sub string, index int) (bool, int, string, error) {
	site = strings.TrimSpace(site)
	group = strings.TrimSpace(group)
	sub = strings.TrimSpace(sub)
	mu.RLock()
	cur, err := modelAt(&cfg, site, group, sub, index)
	var m Model
	var ratio string
	if err == nil {
		m = *cur
		if groups, gerr := groupsAt(&cfg, site); gerr == nil {
			if gi := findGroup(*groups, group); gi >= 0 {
				if si := findSub(&(*groups)[gi], sub); si >= 0 {
					ratio = (*groups)[gi].SubGroups[si].Ratio
				}
			}
		}
	}
	mu.RUnlock()
	if err != nil {
		return false, 0, "", err
	}
	up, lat, reason := checkOne(m)
	saveCheck(site, group, sub, m.ID, up, lat, reason)
	mu.RLock()
	sections := append([]Section(nil), statusV2.Sections...)
	mu.RUnlock()
	updateRow(site, group, SubGroup{Name: sub, Ratio: ratio}, m, &sections, up, lat)
	return up, lat, reason, nil
}

func addModel(c *Config, site, group, sub string, m Model) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&(*groups)[gi], strings.TrimSpace(sub))
	if si < 0 {
		return errors.New("子组不存在")
	}
	if err := cleanModel(&m, "", true, c.DefaultBaseURL); err != nil {
		return err
	}
	if hasModelID((*groups)[gi].SubGroups[si], m.ID, -1) {
		return errors.New("该子组已有相同模型 ID")
	}
	(*groups)[gi].SubGroups[si].Models = append((*groups)[gi].SubGroups[si].Models, m)
	return nil
}

func updateModel(c *Config, site, group, sub string, index int, m Model) error {
	cur, err := modelAt(c, site, group, sub, index)
	if err != nil {
		return err
	}
	prev := cur.Key
	if err := cleanModel(&m, prev, false, c.DefaultBaseURL); err != nil {
		return err
	}
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	si := findSub(&(*groups)[gi], strings.TrimSpace(sub))
	if hasModelID((*groups)[gi].SubGroups[si], m.ID, index) {
		return errors.New("该子组已有相同模型 ID")
	}
	*cur = m
	return nil
}

func deleteModel(c *Config, site, group, sub string, index int) error {
	groups, err := groupsAt(c, site)
	if err != nil {
		return err
	}
	gi := findGroup(*groups, strings.TrimSpace(group))
	if gi < 0 {
		return errors.New("分组不存在")
	}
	si := findSub(&(*groups)[gi], strings.TrimSpace(sub))
	if si < 0 {
		return errors.New("子组不存在")
	}
	models := (*groups)[gi].SubGroups[si].Models
	if index < 0 || index >= len(models) {
		return errors.New("模型不存在")
	}
	(*groups)[gi].SubGroups[si].Models = append(models[:index], models[index+1:]...)
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

func cleanStatusURL(v string) (string, error) {
	v = strings.TrimSpace(v)
	lower := strings.ToLower(v)
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(lower, prefix) {
			v = v[len(prefix):]
			break
		}
	}
	v = strings.TrimRight(strings.TrimSpace(v), "/")
	if v == "" {
		return "zzshu.si", nil
	}
	if len([]rune(v)) > 120 || strings.ContainsAny(v, " \r\n<>") || strings.Contains(v, "://") {
		return "", errors.New("监控网址不能包含空格、换行或尖括号")
	}
	return v, nil
}

func cleanBoardNote(v, what string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if len([]rune(v)) > 120 || strings.ContainsAny(v, " \r\n<>") {
		return "", errors.New(what + "不能包含空格、换行或尖括号")
	}
	return v, nil
}

func splitChatIDs(parts ...string) []string {
	seen := map[string]bool{}
	var out []string
	for _, part := range parts {
		for _, piece := range strings.FieldsFunc(part, func(r rune) bool {
			return r == '\n' || r == '\r' || r == ',' || r == '，' || r == ';' || r == '；' || r == ' ' || r == '\t'
		}) {
			piece = strings.TrimSpace(piece)
			if piece == "" || seen[piece] {
				continue
			}
			seen[piece] = true
			out = append(out, piece)
		}
	}
	return out
}

func cleanTelegram(t TelegramConfig) (TelegramConfig, error) {
	t.BotToken = strings.TrimSpace(t.BotToken)
	t.ChatID = strings.TrimSpace(t.ChatID)
	t.ChatIDs = splitChatIDs(append([]string{t.ChatID}, t.ChatIDs...)...)
	if len(t.ChatIDs) > 0 {
		t.ChatID = t.ChatIDs[0]
	} else {
		t.ChatID = ""
	}
	var err error
	t.StatusURL, err = cleanStatusURL(t.StatusURL)
	if err != nil {
		return t, err
	}
	t.Website, err = cleanBoardNote(t.Website, "官网")
	if err != nil {
		return t, err
	}
	t.Channel, err = cleanBoardNote(t.Channel, "通知频道")
	if err != nil {
		return t, err
	}
	if t.ButtonsPerRow == 0 {
		t.ButtonsPerRow = 1
	}
	if t.ButtonsPerRow < 1 || t.ButtonsPerRow > 4 {
		return t, errors.New("每排按钮数需在 1 到 4")
	}
	if t.Buttons == nil {
		t.Buttons = legacyTelegramButtons(t)
	}
	t.Buttons, err = cleanTelegramButtons(t.Buttons)
	if err != nil {
		return t, err
	}
	if t.Interval < 0 || t.Interval > 10080 {
		return t, errors.New("推送间隔需在 0 到 10080 分钟")
	}
	if t.BotToken == "" && len(t.ChatIDs) == 0 {
		return t, nil
	}
	if t.BotToken == "" || len(t.ChatIDs) == 0 {
		return t, errors.New("Telegram 需要同时填写 Bot Token 和群组 ID")
	}
	if len(t.ChatIDs) > 20 {
		return t, errors.New("最多填写 20 个群组")
	}
	if len(t.BotToken) > 200 || strings.ContainsAny(t.BotToken, " \r\n") || !strings.Contains(t.BotToken, ":") {
		return t, errors.New("Bot Token 格式不正确")
	}
	for _, id := range t.ChatIDs {
		if !validTelegramChat(id) {
			return t, errors.New("群组 ID 需为数字，或以 @ 开头的频道用户名")
		}
	}
	return t, nil
}

func legacyTelegramButtons(t TelegramConfig) []TelegramButton {
	status, err := cleanStatusURL(t.StatusURL)
	if err != nil || status == "" {
		status = "zzshu.si"
	}
	out := []TelegramButton{{Name: "监控 " + status, URL: "https://" + status}}
	website := strings.TrimSpace(t.Website)
	if website != "" && !sameBoardLink(website, status) {
		if link, err := cleanButtonURL(website); err == nil {
			out = append(out, TelegramButton{Name: "官网 " + buttonHost(website), URL: link})
		}
	}
	channel := strings.TrimSpace(t.Channel)
	if channel != "" {
		if link, err := cleanButtonURL(channel); err == nil {
			out = append(out, TelegramButton{Name: "通知频道 " + buttonHost(channel), URL: link})
		}
	}
	return out
}

func cleanTelegramButtons(buttons []TelegramButton) ([]TelegramButton, error) {
	if len(buttons) > 12 {
		return nil, errors.New("最多 12 个按钮")
	}
	out := make([]TelegramButton, 0, len(buttons))
	for _, button := range buttons {
		name := strings.TrimSpace(button.Name)
		raw := strings.TrimSpace(button.URL)
		if name == "" && raw == "" {
			continue
		}
		if name == "" || raw == "" {
			return nil, errors.New("按钮需要同时填写名称和链接")
		}
		if len([]rune(name)) > 64 || strings.ContainsAny(name, "\r\n") {
			return nil, errors.New("按钮名称不能超过 64 个字，也不能换行")
		}
		link, err := cleanButtonURL(raw)
		if err != nil {
			return nil, err
		}
		out = append(out, TelegramButton{Name: name, URL: link})
	}
	return out, nil
}

func cleanButtonURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", errors.New("按钮链接不能为空")
	}
	if strings.ContainsAny(raw, " \r\n<>\"") {
		return "", errors.New("按钮链接不能包含空格或换行")
	}
	if strings.HasPrefix(raw, "@") {
		name := raw[1:]
		if !validButtonChannel(name) {
			return "", errors.New("频道名需以 @ 开头，后面是 5 到 32 位字母、数字或下划线")
		}
		return "https://t.me/" + name, nil
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + strings.TrimLeft(raw, "/")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return "", errors.New("按钮链接需为网址，或 @频道名")
	}
	if len(u.String()) > 500 {
		return "", errors.New("按钮链接过长")
	}
	return u.String(), nil
}

func buttonHost(v string) string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "@") {
		return v
	}
	lower := strings.ToLower(v)
	for _, prefix := range []string{"https://", "http://"} {
		if strings.HasPrefix(lower, prefix) {
			v = v[len(prefix):]
			break
		}
	}
	v = strings.TrimRight(v, "/")
	if i := strings.IndexAny(v, "/?#"); i >= 0 {
		v = v[:i]
	}
	return v
}

func validButtonChannel(name string) bool {
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

func cleanNotice(front *FrontendConfig) error {
	front.NoticeTitle = strings.TrimSpace(front.NoticeTitle)
	front.Notice = strings.TrimSpace(front.Notice)
	if front.Notice == "" {
		if front.NoticeTitle != "" {
			return errors.New("填写了公告标题时，公告内容不能为空")
		}
		return nil
	}
	if len([]rune(front.NoticeTitle)) > 40 {
		return errors.New("公告标题不能超过 40 个字")
	}
	if len([]rune(front.Notice)) > 2000 {
		return errors.New("公告内容不能超过 2000 个字")
	}
	return nil
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
	if err := cleanNotice(&front); err != nil {
		return err
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

func renameSiteHistory(oldName, newName string) {
	if db == nil || oldName == "" || oldName == newName {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET site_name = ? WHERE site_name = ?`, newName, oldName); err != nil {
		log.Println("rename site history:", err)
	}
}

func renameGroupHistory(site, oldName, newName string) {
	if db == nil || oldName == "" || oldName == newName {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET group_name = ? WHERE site_name = ? AND group_name = ?`, newName, site, oldName); err != nil {
		log.Println("rename group history:", err)
	}
}

func renameSubHistory(site, group, oldName, newName string) {
	if db == nil || oldName == newName {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET sub_name = ? WHERE site_name = ? AND group_name = ? AND sub_name = ?`, newName, site, group, oldName); err != nil {
		log.Println("rename subgroup history:", err)
	}
}

func renameModelHistory(site, group, sub, oldID, newID string) {
	if db == nil || oldID == newID {
		return
	}
	if _, err := db.Exec(`UPDATE checks SET model_id = ? WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ?`, newID, site, group, sub, oldID); err != nil {
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
	case "sites":
		if !requireAdmin(w, r) {
			return
		}
		adminSites(w, r)
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
	case "model-test":
		if !requireAdmin(w, r) {
			return
		}
		adminTestModel(w, r)
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
	case "telegram-log":
		if !requireAdmin(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"logs": telegramLogs()})
	case "clear-history":
		if !requireAdmin(w, r) {
			return
		}
		adminClearHistory(w, r)
	case "community":
		if !requireAdmin(w, r) {
			return
		}
		adminCommunity(w, r)
	case "community-codes":
		if !requireAdmin(w, r) {
			return
		}
		adminCommunityCodes(w, r)
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
	Sites                 []Site         `json:"sites"`
	Groups                []Group        `json:"groups,omitempty"`
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
		Sites:                 configSites(cfg),
		Groups:                nil,
	}
	mu.RUnlock()
	if view.Sites == nil {
		view.Sites = []Site{}
	}
	buttons, perRow := telegramButtons(view.Telegram)
	view.Telegram.Buttons = buttons
	view.Telegram.ButtonsPerRow = perRow
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

func adminClearHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	n, err := clearProbeHistory()
	if err != nil {
		writeAdminJSON(w, 500, map[string]string{"error": "清空失败"})
		log.Println("clear history:", err)
		return
	}
	writeAdminJSON(w, 200, map[string]any{"ok": true, "deleted": n})
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
	mu.RLock()
	title := cfg.Frontend.Title
	sections := make([]Section, len(statusV2.Sections))
	for i, sec := range statusV2.Sections {
		sections[i] = Section{Name: sec.Name, Rows: append([]GroupResp(nil), sec.Rows...)}
	}
	mu.RUnlock()
	now := time.Now()
	images, err := renderStatusBoard(title, sections, now)
	if err != nil {
		noteTelegram("测试状态图生成失败：" + err.Error())
		writeAdminJSON(w, 500, map[string]string{"error": "状态图生成失败：" + err.Error()})
		return
	}
	markup := telegramKeyboard(cleaned.Buttons, cleaned.ButtonsPerRow)
	if err := sendTelegramPhotos(cleaned.BotToken, cleaned.ChatIDs, images, "测试推送", markup); err != nil {
		noteTelegram("测试状态图发送失败：" + err.Error())
		writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	noteTelegram(fmt.Sprintf("测试状态图已发到 %d 个群", len(cleaned.ChatIDs)))
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

type siteBody struct {
	Name     string   `json:"name"`
	NewName  string   `json:"newName"`
	Interval int      `json:"interval"`
	Order    []string `json:"order"`
}

func adminSites(w http.ResponseWriter, r *http.Request) {
	var body siteBody
	if !decodeBody(w, r, &body) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		finishChange(w, updateConfig(func(c *Config) error {
			return addSite(c, body.Name, body.Interval)
		}), false)
	case http.MethodPut:
		newName := body.NewName
		if strings.TrimSpace(newName) == "" {
			newName = body.Name
		}
		err := updateConfig(func(c *Config) error {
			return updateSite(c, body.Name, newName, body.Interval)
		})
		if err == nil {
			renameSiteHistory(strings.TrimSpace(body.Name), strings.TrimSpace(newName))
			renameLiveSite(strings.TrimSpace(body.Name), strings.TrimSpace(newName))
		}
		finishChange(w, err, false)
	case http.MethodPatch:
		finishChange(w, updateConfig(func(c *Config) error {
			return reorderSites(c, body.Order)
		}), false)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteSite(c, body.Name)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

type groupBody struct {
	Site          string   `json:"site"`
	Name          string   `json:"name"`
	NewName       string   `json:"newName"`
	ManualRefresh bool     `json:"manualRefresh"`
	Order         []string `json:"order"`
}

func adminGroups(w http.ResponseWriter, r *http.Request) {
	var body groupBody
	if !decodeBody(w, r, &body) {
		return
	}
	switch r.Method {
	case http.MethodPost:
		finishChange(w, updateConfig(func(c *Config) error {
			return addGroup(c, body.Site, body.Name, body.ManualRefresh)
		}), true)
	case http.MethodPut:
		newName := body.NewName
		if strings.TrimSpace(newName) == "" {
			newName = body.Name
		}
		err := updateConfig(func(c *Config) error {
			return updateGroup(c, body.Site, body.Name, newName, body.ManualRefresh)
		})
		if err == nil {
			renameGroupHistory(strings.TrimSpace(body.Site), strings.TrimSpace(body.Name), strings.TrimSpace(newName))
		}
		finishChange(w, err, true)
	case http.MethodPatch:
		err := updateConfig(func(c *Config) error {
			return reorderGroups(c, body.Site, body.Order)
		})
		if err == nil {
			reorderSections(strings.TrimSpace(body.Site), body.Order)
		}
		finishChange(w, err, false)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteGroup(c, body.Site, body.Name)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

type subBody struct {
	Site     string   `json:"site"`
	Group    string   `json:"group"`
	Name     string   `json:"name"`
	NewName  string   `json:"newName"`
	KumaSlug string   `json:"kumaSlug"`
	Ratio    string   `json:"ratio"`
	Copy     bool     `json:"copy"`
	Order    []string `json:"order"`
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
				return copySubGroup(c, body.Site, body.Group, body.Name, body.NewName, body.KumaSlug, body.Ratio)
			}
			return addSubGroup(c, body.Site, body.Group, body.Name, body.KumaSlug, body.Ratio)
		})
		if err == nil {
			syncLiveRatios()
		}
		finishChange(w, err, true)
	case http.MethodPut:
		newName := body.NewName
		if strings.TrimSpace(newName) == "" {
			newName = body.Name
		}
		err := updateConfig(func(c *Config) error {
			return updateSubGroup(c, body.Site, body.Group, body.Name, newName, body.KumaSlug, body.Ratio)
		})
		if err == nil {
			renameSubHistory(strings.TrimSpace(body.Site), strings.TrimSpace(body.Group), strings.TrimSpace(body.Name), strings.TrimSpace(newName))
			renameLiveSubgroup(strings.TrimSpace(body.Site), strings.TrimSpace(body.Group), strings.TrimSpace(body.Name), strings.TrimSpace(newName))
			syncLiveRatios()
		}
		finishChange(w, err, true)
	case http.MethodPatch:
		err := updateConfig(func(c *Config) error {
			return reorderSubGroups(c, body.Site, body.Group, body.Order)
		})
		if err == nil {
			reorderSectionRows(strings.TrimSpace(body.Site), strings.TrimSpace(body.Group), body.Order)
		}
		finishChange(w, err, false)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteSubGroup(c, body.Site, body.Group, body.Name)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

type modelBody struct {
	Site     string `json:"site"`
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
			return addModel(c, body.Site, body.Group, body.Subgroup, body.Model)
		}), true)
	case http.MethodPut:
		var oldID string
		err := updateConfig(func(c *Config) error {
			cur, err := modelAt(c, body.Site, body.Group, body.Subgroup, body.Index)
			if err != nil {
				return err
			}
			oldID = cur.ID
			return updateModel(c, body.Site, body.Group, body.Subgroup, body.Index, body.Model)
		})
		if err == nil {
			renameModelHistory(strings.TrimSpace(body.Site), strings.TrimSpace(body.Group), strings.TrimSpace(body.Subgroup), oldID, strings.TrimSpace(body.Model.ID))
		}
		finishChange(w, err, true)
	case http.MethodDelete:
		finishChange(w, updateConfig(func(c *Config) error {
			return deleteModel(c, body.Site, body.Group, body.Subgroup, body.Index)
		}), true)
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func adminTestModel(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	var body struct {
		Site     string `json:"site"`
		Group    string `json:"group"`
		Subgroup string `json:"subgroup"`
		Index    int    `json:"index"`
	}
	if !decodeBody(w, r, &body) {
		return
	}
	up, lat, reason, err := testConfiguredModel(body.Site, body.Group, body.Subgroup, body.Index)
	if err != nil {
		writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
		return
	}
	writeAdminJSON(w, 200, map[string]any{
		"ok":      true,
		"up":      up,
		"latency": lat,
		"reason":  reason,
	})
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
		Site     string `json:"site"`
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
		cur, err := modelAt(&cfg, body.Site, body.Group, body.Subgroup, body.Index)
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
	case "openai-compat", "openai-response", "openai-image", "google", "google-image", "anthropic":
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
		case "google", "google-image":
			q := req.URL.Query()
			q.Set("key", key)
			req.URL.RawQuery = q.Encode()
			req.Header.Set("authorization", "Bearer "+key)
			req.Header.Set("x-goog-api-key", key)
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
	case "openai-compat", "openai-response", "openai-image", "anthropic":
		for strings.HasSuffix(path, "/v1/v1") {
			path = strings.TrimSuffix(path, "/v1")
		}
		if !strings.HasSuffix(path, "/v1") {
			path += "/v1"
		}
	case "google", "google-image":
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
	suffixes := []string{"/chat/completions", "/completions", "/responses", "/images/generations", "/messages", "/models"}
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
