package main

import (
	"bytes"
	"crypto/tls"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// FrontendConfig 前端展示配置
type FrontendConfig struct {
	Title       string `json:"title"`
	Icon        string `json:"icon,omitempty"`
	Subtitle    string `json:"subtitle,omitempty"`
	Footer      string `json:"footer,omitempty"`
	NoticeTitle string `json:"noticeTitle,omitempty"`
	Notice      string `json:"notice,omitempty"`
}

// Config 配置文件结构
type Config struct {
	Port                  int            `json:"port"`
	Interval              int            `json:"interval"`
	ManualRefreshCooldown int            `json:"manualRefreshCooldown"` // 手动刷新全站冷却时间（秒），默认60
	AdminPassword         string         `json:"adminPassword"`         // 管理后台密码，留空则关闭后台
	AdminPath             string         `json:"adminPath"`             // 后台路径名，留空则不注册入口
	DefaultBaseURL        string         `json:"defaultBaseURL"`        // 新建模型时使用的默认接口地址
	Frontend              FrontendConfig `json:"frontend"`
	Kuma                  KumaConfig     `json:"kuma"`
	Telegram              TelegramConfig `json:"telegram"`
	Sites                 []Site         `json:"sites,omitempty"`
	Groups                []Group        `json:"groups"`
}

type TelegramConfig struct {
	BotToken      string           `json:"botToken"`
	ChatID        string           `json:"chatID"`
	ChatIDs       []string         `json:"chatIDs,omitempty"`
	Interval      int              `json:"interval"`  // 群推送间隔（分钟）。0 表示状态一变化就推送
	StatusURL     string           `json:"statusURL"` // 旧版底部监控地址；未配置按钮时仍用来生成默认按钮
	Website       string           `json:"website"`   // 旧版底部官网
	Channel       string           `json:"channel"`   // 旧版底部通知频道
	Buttons       []TelegramButton `json:"buttons"`
	ButtonsPerRow int              `json:"buttonsPerRow,omitempty"` // 每排按钮数，1 到 4
}

type TelegramButton struct {
	Name string `json:"name"`
	URL  string `json:"url"`
}

type KumaConfig struct {
	BaseURL string `json:"baseURL"`
	Slug    string `json:"slug"`
}

type Site struct {
	Name     string  `json:"name"`
	Interval int     `json:"interval,omitempty"` // 单独检测间隔（秒）。0 表示用全局间隔
	Groups   []Group `json:"groups"`
}

type Group struct {
	Name          string     `json:"name"`
	ManualRefresh bool       `json:"manualRefresh"` // 是否支持手动刷新
	SubGroups     []SubGroup `json:"subGroups"`
}

type SubGroup struct {
	Name     string  `json:"name"`
	KumaSlug string  `json:"kumaSlug"`
	Ratio    string  `json:"ratio,omitempty"` // 展示倍率，留空则状态页不显示
	Models   []Model `json:"models"`
}

type Model struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	BaseURL  string `json:"baseURL"`
	Key      string `json:"key"`
	Timeout  int    `json:"timeout"`
}

type OverallStatus struct {
	Status string `json:"status"`
	Label  string `json:"label"`
}

type GroupResp struct {
	Name          string                      `json:"name"`
	SubName       string                      `json:"sub_name"`
	Ratio         string                      `json:"ratio,omitempty"`
	Family        string                      `json:"family"`
	Status        string                      `json:"status"`
	Label         string                      `json:"label"`
	HealthScore   float64                     `json:"health_score"`
	LatencyMs     int                         `json:"latency_ms"`
	LastCheckedAt *time.Time                  `json:"last_checked_at,omitempty"`
	History       []HistoryItem               `json:"history"`
	Timelines     map[string][]TimelineBucket `json:"timelines,omitempty"`
}

type HistoryItem struct {
	S string  `json:"s"`
	V float64 `json:"v"`
	T int64   `json:"t,omitempty"`
	L int     `json:"l"`
}

type TimelineBucket struct {
	Start   int64   `json:"start"`
	End     int64   `json:"end"`
	Samples int     `json:"samples"`
	Up      int     `json:"up"`
	Rate    float64 `json:"success_rate"`
	Latency int     `json:"avg_latency_ms"`
	Reason  string  `json:"reason,omitempty"`
}

// 按外层group.name分组，返回分组列表和该分组下的所有行
type Section struct {
	Site string      `json:"site,omitempty"`
	Name string      `json:"name"`
	Rows []GroupResp `json:"rows"`
}

type SiteStatus struct {
	Name     string    `json:"name"`
	Sections []Section `json:"sections"`
}

type StatusRespV2 struct {
	Title                  string        `json:"title"`
	Overall                OverallStatus `json:"overall"`
	LastCompletedAt        *time.Time    `json:"last_completed_at,omitempty"`
	Checking               bool          `json:"checking"`
	Sites                  []SiteStatus  `json:"sites,omitempty"`
	Sections               []Section     `json:"sections"`
	ManualRefreshRemaining int           `json:"manual_refresh_remaining"` // 手动刷新剩余冷却秒数
}

var (
	cfg               Config
	mu                sync.RWMutex
	statusV2          StatusRespV2
	db                *sql.DB
	checking          bool
	lastManualRefresh time.Time
	manualRefreshMu   sync.RWMutex
	telegramBaselined bool
)

var configPath = "config.json"

func loadConfig() {
	b, err := os.ReadFile(configPath)
	if err != nil {
		log.Println("config read error:", err)
		return
	}
	var newCfg Config
	if err := json.Unmarshal(b, &newCfg); err != nil {
		log.Println("config parse error:", err)
		return
	}
	normalizeConfig(&newCfg)
	if db != nil {
		if ready, err := groupsReady(); err != nil {
			log.Println("groups ready:", err)
		} else if ready {
			sites, err := loadMonitorSites()
			if err != nil {
				log.Println("load sites:", err)
			} else {
				newCfg.Sites = sites
				newCfg.Groups = nil
			}
		}
	}
	mu.Lock()
	cfg = newCfg
	mu.Unlock()
}

func normalizeConfig(c *Config) {
	if c.Port == 0 {
		c.Port = 8080
	}
	if c.Interval == 0 {
		c.Interval = 60
	}
	if c.ManualRefreshCooldown == 0 {
		c.ManualRefreshCooldown = 60
	}
	if c.Frontend.Title == "" {
		c.Frontend.Title = "Service Status"
	}
	if c.Frontend.Footer == "" {
		c.Frontend.Footer = "Powered by go-status"
	}
	if strings.TrimSpace(c.DefaultBaseURL) == "" {
		c.DefaultBaseURL = "https://zzshu.cc"
	} else if cleaned, err := cleanDefaultBase(c.DefaultBaseURL); err == nil {
		c.DefaultBaseURL = cleaned
	}
	c.Telegram.BotToken = strings.TrimSpace(c.Telegram.BotToken)
	c.Telegram.ChatID = strings.TrimSpace(c.Telegram.ChatID)
	c.Telegram.ChatIDs = splitChatIDs(append([]string{c.Telegram.ChatID}, c.Telegram.ChatIDs...)...)
	if len(c.Telegram.ChatIDs) > 0 {
		c.Telegram.ChatID = c.Telegram.ChatIDs[0]
	}
	if cleaned, err := cleanStatusURL(c.Telegram.StatusURL); err == nil {
		c.Telegram.StatusURL = cleaned
	}
	c.Telegram.Website = strings.TrimSpace(c.Telegram.Website)
	c.Telegram.Channel = strings.TrimSpace(c.Telegram.Channel)
	if c.Groups == nil {
		c.Groups = []Group{}
	}
	if c.Sites == nil {
		c.Sites = []Site{}
	}
	normalizeGroupList(c.Groups)
	for i := range c.Sites {
		c.Sites[i].Name = strings.TrimSpace(c.Sites[i].Name)
		if c.Sites[i].Groups == nil {
			c.Sites[i].Groups = []Group{}
		}
		normalizeGroupList(c.Sites[i].Groups)
	}
}

func normalizeGroupList(groups []Group) {
	for i := range groups {
		if groups[i].SubGroups == nil {
			groups[i].SubGroups = []SubGroup{}
		}
		for j := range groups[i].SubGroups {
			if groups[i].SubGroups[j].Models == nil {
				groups[i].SubGroups[j].Models = []Model{}
			}
			for k := range groups[i].SubGroups[j].Models {
				m := &groups[i].SubGroups[j].Models[k]
				if m.Provider == "" {
					m.Provider = "openai-compat"
				}
				if m.Timeout <= 0 {
					m.Timeout = 30
				}
			}
		}
	}
}

func effectiveInterval(site Site, global int) int {
	if site.Interval >= 5 {
		return site.Interval
	}
	if global < 5 {
		return 60
	}
	return global
}

func cleanSiteInterval(seconds int) (int, error) {
	if seconds == 0 {
		return 0, nil
	}
	if seconds < 5 || seconds > 86400 {
		return 0, errors.New("站点检测间隔需在 5 到 86400 秒，留空则使用全局间隔")
	}
	return seconds, nil
}

func defaultSiteName(title string) string {
	title = strings.TrimSpace(title)
	if title == "" || title == "Service Status" {
		return "默认站点"
	}
	if len([]rune(title)) > 80 || strings.ContainsAny(title, "|\r\n") {
		return "默认站点"
	}
	return title
}

func configSites(c Config) []Site {
	if len(c.Sites) > 0 {
		return c.Sites
	}
	if len(c.Groups) == 0 {
		return nil
	}
	return []Site{{Name: defaultSiteName(c.Frontend.Title), Groups: c.Groups}}
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "status.db?_pragma=busy_timeout(5000)")
	if err != nil {
		log.Fatal(err)
	}
	db.SetMaxOpenConns(8)
	if err := ensureSchema(); err != nil {
		log.Fatal(err)
	}
	if err = ensureSiteInterval(); err != nil {
		log.Fatal(err)
	}
}

func ensureSchema() error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS checks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			site_name TEXT NOT NULL DEFAULT '',
			group_name TEXT NOT NULL,
			sub_name TEXT NOT NULL,
			model_id TEXT NOT NULL,
			up INTEGER NOT NULL,
			latency INTEGER NOT NULL,
			checked_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_checks_time ON checks(checked_at);
		CREATE INDEX IF NOT EXISTS idx_checks_group ON checks(site_name, group_name, sub_name, model_id, checked_at);
		CREATE TABLE IF NOT EXISTS monitor_sites (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			sort_order INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS monitor_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			site_id INTEGER NOT NULL DEFAULT 0,
			name TEXT NOT NULL,
			manual_refresh INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL,
			UNIQUE(site_id, name)
		);
		CREATE TABLE IF NOT EXISTS monitor_subgroups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			group_id INTEGER NOT NULL,
			name TEXT NOT NULL,
			kuma_slug TEXT NOT NULL DEFAULT '',
			sort_order INTEGER NOT NULL,
			UNIQUE(group_id, name)
		);
		CREATE TABLE IF NOT EXISTS monitor_models (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			subgroup_id INTEGER NOT NULL,
			model_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			base_url TEXT NOT NULL,
			api_key TEXT NOT NULL,
			timeout INTEGER NOT NULL,
			sort_order INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS app_meta (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	if err := ensureSubgroupRatio(); err != nil {
		return err
	}
	if err := ensureSiteInterval(); err != nil {
		return err
	}
	if err := ensureCheckReason(); err != nil {
		return err
	}
	return ensureCommunityTables()
}

func ensureCheckReason() error {
	has, err := columnExists("checks", "reason")
	if err != nil || has {
		return err
	}
	_, err = db.Exec(`ALTER TABLE checks ADD COLUMN reason TEXT NOT NULL DEFAULT ''`)
	return err
}

func ensureSiteInterval() error {
	has, err := columnExists("monitor_sites", "interval")
	if err != nil || has {
		return err
	}
	_, err = db.Exec(`ALTER TABLE monitor_sites ADD COLUMN interval INTEGER NOT NULL DEFAULT 0`)
	return err
}

func ensureSubgroupRatio() error {
	rows, err := db.Query(`PRAGMA table_info(monitor_subgroups)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return err
		}
		if name == "ratio" {
			return rows.Err()
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	_, err = db.Exec(`ALTER TABLE monitor_subgroups ADD COLUMN ratio TEXT NOT NULL DEFAULT ''`)
	return err
}

func groupsReady() (bool, error) {
	var v string
	err := db.QueryRow(`SELECT v FROM app_meta WHERE k = ?`, "groups_ready").Scan(&v)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

func columnExists(table, column string) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		if name == column {
			return true, rows.Err()
		}
	}
	return false, rows.Err()
}

func ensureSiteSchema(defaultSite string) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS monitor_sites (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE,
		sort_order INTEGER NOT NULL
	)`); err != nil {
		return err
	}
	hasSite, err := columnExists("monitor_groups", "site_id")
	if err != nil {
		return err
	}
	if !hasSite {
		if err := rebuildGroupsWithSite(defaultSite); err != nil {
			return err
		}
	}
	hasName, err := columnExists("checks", "site_name")
	if err != nil {
		return err
	}
	if !hasName {
		if _, err := db.Exec(`ALTER TABLE checks ADD COLUMN site_name TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_checks_site ON checks(site_name, group_name, sub_name, model_id, checked_at)`); err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE checks SET site_name = ? WHERE site_name = ''`, defaultSite)
	return err
}

func rebuildGroupsWithSite(defaultSite string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`INSERT INTO monitor_sites (name, sort_order) VALUES (?, 0)`, defaultSite)
	if err != nil {
		return err
	}
	siteID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TABLE monitor_groups_new (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		site_id INTEGER NOT NULL,
		name TEXT NOT NULL,
		manual_refresh INTEGER NOT NULL DEFAULT 0,
		sort_order INTEGER NOT NULL,
		UNIQUE(site_id, name)
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO monitor_groups_new (id, site_id, name, manual_refresh, sort_order)
		SELECT id, ?, name, manual_refresh, sort_order FROM monitor_groups`, siteID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE monitor_groups`); err != nil {
		return err
	}
	if _, err := tx.Exec(`ALTER TABLE monitor_groups_new RENAME TO monitor_groups`); err != nil {
		return err
	}
	return tx.Commit()
}

func loadMonitorSites() ([]Site, error) {
	rows, err := db.Query(`SELECT id, name, interval FROM monitor_sites ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	type siteRow struct {
		id, interval int
		name         string
	}
	var list []siteRow
	for rows.Next() {
		var item siteRow
		if err := rows.Scan(&item.id, &item.name, &item.interval); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var sites []Site
	for _, item := range list {
		groups, err := loadMonitorGroups(item.id)
		if err != nil {
			return nil, err
		}
		sites = append(sites, Site{Name: item.name, Interval: item.interval, Groups: groups})
	}
	if sites == nil {
		sites = []Site{}
	}
	return sites, nil
}

func loadMonitorGroups(siteID int) ([]Group, error) {
	rows, err := db.Query(`SELECT id, name, manual_refresh FROM monitor_groups WHERE site_id = ? ORDER BY sort_order, id`, siteID)
	if err != nil {
		return nil, err
	}
	type groupRow struct {
		id, manual int
		name       string
	}
	var list []groupRow
	for rows.Next() {
		var item groupRow
		if err := rows.Scan(&item.id, &item.name, &item.manual); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var groups []Group
	for _, item := range list {
		subs, err := loadMonitorSubgroups(item.id)
		if err != nil {
			return nil, err
		}
		groups = append(groups, Group{Name: item.name, ManualRefresh: item.manual == 1, SubGroups: subs})
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, nil
}

func loadMonitorSubgroups(groupID int) ([]SubGroup, error) {
	rows, err := db.Query(`SELECT id, name, kuma_slug, ratio FROM monitor_subgroups WHERE group_id = ? ORDER BY sort_order, id`, groupID)
	if err != nil {
		return nil, err
	}
	type subRow struct {
		id         int
		name, slug string
		ratio      string
	}
	var list []subRow
	for rows.Next() {
		var item subRow
		if err := rows.Scan(&item.id, &item.name, &item.slug, &item.ratio); err != nil {
			rows.Close()
			return nil, err
		}
		list = append(list, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var subs []SubGroup
	for _, item := range list {
		models, err := loadMonitorModels(item.id)
		if err != nil {
			return nil, err
		}
		subs = append(subs, SubGroup{Name: item.name, KumaSlug: item.slug, Ratio: strings.TrimSpace(item.ratio), Models: models})
	}
	if subs == nil {
		subs = []SubGroup{}
	}
	return subs, nil
}

func loadMonitorModels(subgroupID int) ([]Model, error) {
	rows, err := db.Query(`SELECT model_id, provider, base_url, api_key, timeout FROM monitor_models WHERE subgroup_id = ? ORDER BY sort_order, id`, subgroupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var models []Model
	for rows.Next() {
		var m Model
		if err := rows.Scan(&m.ID, &m.Provider, &m.BaseURL, &m.Key, &m.Timeout); err != nil {
			return nil, err
		}
		models = append(models, m)
	}
	if models == nil {
		models = []Model{}
	}
	return models, rows.Err()
}

func saveMonitorSites(sites []Site) error {
	if db == nil {
		return fmt.Errorf("数据库未就绪")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM monitor_models`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM monitor_subgroups`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM monitor_groups`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM monitor_sites`); err != nil {
		return err
	}
	for i, site := range sites {
		res, err := tx.Exec(`INSERT INTO monitor_sites (name, sort_order, interval) VALUES (?, ?, ?)`, site.Name, i, site.Interval)
		if err != nil {
			return err
		}
		siteID, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for j, g := range site.Groups {
			manual := 0
			if g.ManualRefresh {
				manual = 1
			}
			res, err = tx.Exec(`INSERT INTO monitor_groups (site_id, name, manual_refresh, sort_order) VALUES (?, ?, ?, ?)`, siteID, g.Name, manual, j)
			if err != nil {
				return err
			}
			gid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			for k, sg := range g.SubGroups {
				res, err = tx.Exec(`INSERT INTO monitor_subgroups (group_id, name, kuma_slug, ratio, sort_order) VALUES (?, ?, ?, ?, ?)`, gid, sg.Name, sg.KumaSlug, strings.TrimSpace(sg.Ratio), k)
				if err != nil {
					return err
				}
				sid, err := res.LastInsertId()
				if err != nil {
					return err
				}
				for n, m := range sg.Models {
					if _, err := tx.Exec(`INSERT INTO monitor_models (subgroup_id, model_id, provider, base_url, api_key, timeout, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?)`, sid, m.ID, m.Provider, m.BaseURL, m.Key, m.Timeout, n); err != nil {
						return err
					}
				}
			}
		}
	}
	if _, err := tx.Exec(`INSERT INTO app_meta (k, v) VALUES ('groups_ready', '1') ON CONFLICT(k) DO UPDATE SET v = '1'`); err != nil {
		return err
	}
	return tx.Commit()
}

func migrateGroups() {
	siteName := defaultSiteName(cfg.Frontend.Title)
	if err := ensureSiteSchema(siteName); err != nil {
		log.Fatal(err)
	}
	ready, err := groupsReady()
	if err != nil {
		log.Fatal(err)
	}
	if !ready {
		mu.RLock()
		sites := configSites(cfg)
		mu.RUnlock()
		if len(sites) == 0 {
			sites = []Site{{Name: siteName, Groups: []Group{}}}
		}
		if err := saveMonitorSites(sites); err != nil {
			log.Fatal(err)
		}
		mu.Lock()
		stored := cfg
		stored.Groups = []Group{}
		stored.Sites = nil
		mu.Unlock()
		if err := writeConfigFile(stored); err != nil {
			log.Println("config strip groups:", err)
		}
	}
	sites, err := loadMonitorSites()
	if err != nil {
		log.Fatal(err)
	}
	if len(sites) == 0 {
		sites = []Site{{Name: siteName, Groups: []Group{}}}
		if err := saveMonitorSites(sites); err != nil {
			log.Fatal(err)
		}
	}
	mu.Lock()
	cfg.Sites = sites
	cfg.Groups = nil
	mu.Unlock()
}

func saveCheck(siteName, groupName, subName, modelID string, up bool, latency int, reason string) {
	v := 0
	if up {
		v = 1
		reason = ""
	}
	reason = clipText(reason, 160)
	_, err := db.Exec(
		"INSERT INTO checks (site_name, group_name, sub_name, model_id, up, latency, checked_at, reason) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		siteName, groupName, subName, modelID, v, latency, time.Now().Unix(), reason,
	)
	if err != nil {
		log.Println("db insert error:", err)
	}
}

func getHistory(siteName, groupName, subName, modelID string, limit int) []HistoryItem {
	rows, err := db.Query(
		"SELECT up, latency, checked_at FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT ?",
		siteName, groupName, subName, modelID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var items []HistoryItem
	for rows.Next() {
		var up int
		var lat int
		var ts int64
		rows.Scan(&up, &lat, &ts)
		status := "down"
		v := 0.0
		if up == 1 {
			status = "up"
			if lat < 1000 {
				v = 1.0
			} else if lat < 3000 {
				v = 0.7
			} else if lat < 10000 {
				v = 0.4
			} else {
				v = 0.2
			}
		}
		items = append(items, HistoryItem{S: status, V: v, T: ts, L: lat})
	}
	// reverse to chronological order
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items
}

type probePoint struct {
	up     bool
	lat    int
	ts     int64
	reason string
}

func loadProbePoints(siteName, groupName, subName, modelID string, since int64) []probePoint {
	if db == nil {
		return nil
	}
	rows, err := db.Query(
		`SELECT up, latency, checked_at, reason FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? AND checked_at >= ? ORDER BY checked_at`,
		siteName, groupName, subName, modelID, since,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var points []probePoint
	for rows.Next() {
		var up, lat int
		var ts int64
		var reason string
		if rows.Scan(&up, &lat, &ts, &reason) != nil {
			continue
		}
		points = append(points, probePoint{up: up == 1, lat: lat, ts: ts, reason: reason})
	}
	return points
}

func buildTimelines(siteName, groupName, subName, modelID string, now time.Time) map[string][]TimelineBucket {
	since := now.Add(-84 * 24 * time.Hour).Unix()
	points := loadProbePoints(siteName, groupName, subName, modelID, since)
	return map[string][]TimelineBucket{
		"hour":  bucketPoints("hour", now, points),
		"probe": probeBuckets(now, points),
		"day":   trimLeadingEmpty(bucketPoints("day", now, points)),
		"week":  trimLeadingEmpty(bucketPoints("week", now, points)),
	}
}

func trimLeadingEmpty(buckets []TimelineBucket) []TimelineBucket {
	for i, bucket := range buckets {
		if bucket.Samples > 0 {
			return buckets[i:]
		}
	}
	return []TimelineBucket{}
}

// probeBuckets 按小时视图只保留最近 12 次检测，每次一格。
func probeBuckets(now time.Time, points []probePoint) []TimelineBucket {
	until := now.Unix()
	kept := make([]probePoint, 0, len(points))
	for _, p := range points {
		if p.ts > until {
			continue
		}
		kept = append(kept, p)
	}
	if len(kept) > 12 {
		kept = kept[len(kept)-12:]
	}
	out := make([]TimelineBucket, 0, len(kept))
	for _, p := range kept {
		up := 0
		rate := 0.0
		if p.up {
			up = 1
			rate = 100
		}
		out = append(out, TimelineBucket{
			Start:   p.ts,
			End:     p.ts,
			Samples: 1,
			Up:      up,
			Rate:    rate,
			Latency: p.lat,
			Reason:  p.reason,
		})
	}
	return out
}

func bucketPoints(gran string, now time.Time, points []probePoint) []TimelineBucket {
	loc := now.Location()
	var n int
	var step time.Duration
	var end time.Time
	switch gran {
	case "hour":
		n, step = 24, time.Hour
		end = now.In(loc).Truncate(time.Hour).Add(time.Hour)
	case "week":
		n, step = 12, 7*24*time.Hour
		end = weekStart(now.In(loc)).Add(step)
	default:
		n, step = 12, 24*time.Hour
		y, m, d := now.In(loc).Date()
		end = time.Date(y, m, d, 0, 0, 0, 0, loc).Add(24 * time.Hour)
	}
	start := end.Add(-time.Duration(n) * step)
	stepSec := int64(step.Seconds())
	out := make([]TimelineBucket, n)
	latSum := make([]int, n)
	startUnix := start.Unix()
	endUnix := end.Unix()
	for i := 0; i < n; i++ {
		b0 := start.Add(time.Duration(i) * step)
		out[i] = TimelineBucket{Start: b0.Unix(), End: b0.Add(step).Unix()}
	}
	for _, p := range points {
		if p.ts < startUnix || p.ts >= endUnix {
			continue
		}
		idx := int((p.ts - startUnix) / stepSec)
		if idx < 0 || idx >= n {
			continue
		}
		out[idx].Samples++
		if p.up {
			out[idx].Up++
		}
		latSum[idx] += p.lat
	}
	for i := range out {
		if out[i].Samples == 0 {
			continue
		}
		out[i].Rate = float64(out[i].Up) / float64(out[i].Samples) * 100
		out[i].Latency = latSum[i] / out[i].Samples
	}
	return out
}

func weekStart(t time.Time) time.Time {
	t = time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
	wd := int(t.Weekday())
	if wd == 0 {
		wd = 7
	}
	return t.AddDate(0, 0, -(wd - 1))
}

func calcHealthScore(siteName, groupName, subName, modelID string) float64 {
	rows, err := db.Query(
		"SELECT up FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT 100",
		siteName, groupName, subName, modelID,
	)
	if err != nil {
		return 0
	}
	defer rows.Close()
	var total, upCount int
	for rows.Next() {
		var up int
		rows.Scan(&up)
		total++
		if up == 1 {
			upCount++
		}
	}
	if total == 0 {
		return 100.0
	}
	return float64(upCount) / float64(total) * 100.0
}

func getLastCheck(siteName, groupName, subName, modelID string) (bool, int, time.Time) {
	var up int
	var lat int
	var ts int64
	err := db.QueryRow(
		"SELECT up, latency, checked_at FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT 1",
		siteName, groupName, subName, modelID,
	).Scan(&up, &lat, &ts)
	if err != nil {
		return false, 0, time.Time{}
	}
	return up == 1, lat, time.Unix(ts, 0)
}

// 最近一小时里只要有成功，偶发失败只算降级。连续失败满一小时才算异常。
func classifyProbeStatus(siteName, groupName, subName, modelID string, now time.Time) (string, string) {
	if db == nil {
		return "unknown", "无采样"
	}
	cutoff := now.Add(-time.Hour).Unix()
	rows, err := db.Query(
		`SELECT up, checked_at FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? AND checked_at >= ? ORDER BY checked_at`,
		siteName, groupName, subName, modelID, cutoff,
	)
	if err != nil {
		return "unknown", "无采样"
	}
	defer rows.Close()
	var ups []bool
	var times []int64
	for rows.Next() {
		var up int
		var ts int64
		if rows.Scan(&up, &ts) != nil {
			continue
		}
		ups = append(ups, up == 1)
		times = append(times, ts)
	}
	if len(ups) == 0 {
		return "unknown", "无采样"
	}
	if ups[len(ups)-1] {
		for _, up := range ups {
			if !up {
				return "degraded", "降级"
			}
		}
		return "up", "正常"
	}
	streakStart := times[0]
	allDown := true
	for i := len(ups) - 1; i >= 0; i-- {
		if ups[i] {
			allDown = false
			break
		}
		streakStart = times[i]
	}
	if allDown {
		var prevUp int
		var prevTs int64
		err := db.QueryRow(
			`SELECT up, checked_at FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? AND checked_at < ? ORDER BY checked_at DESC LIMIT 1`,
			siteName, groupName, subName, modelID, cutoff,
		).Scan(&prevUp, &prevTs)
		if err == nil && prevUp == 0 {
			streakStart = prevTs
		}
	}
	if now.Unix()-streakStart >= int64(time.Hour/time.Second) {
		return "down", "异常"
	}
	return "degraded", "降级"
}

func overallFromRows(sections []Section) (string, string) {
	var total, degraded, down int
	for _, sec := range sections {
		for _, r := range sec.Rows {
			switch r.Status {
			case "down":
				down++
				total++
			case "degraded":
				degraded++
				total++
			case "up":
				total++
			}
		}
	}
	if down > 0 {
		return "down", "部分异常"
	}
	if total > 0 && degraded*2 >= total {
		return "degraded", "降级"
	}
	return "up", "运行正常"
}

func cleanupOld() {
	cutoff := time.Now().Add(-84 * 24 * time.Hour).Unix()
	db.Exec(`DELETE FROM checks WHERE checked_at < ?`, cutoff)
}

var (
	historyEpoch        uint64
	appliedHistoryEpoch uint64
	telegramSyncedEpoch uint64
)

// clearProbeHistory 删除全部探测记录，并立刻让状态页回到无采样。
func clearProbeHistory() (int64, error) {
	if db == nil {
		return 0, errors.New("数据库未就绪")
	}
	res, err := db.Exec(`DELETE FROM checks`)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	resetTelegramQueue()
	atomic.AddUint64(&historyEpoch, 1)
	hydrateStatus()
	return n, nil
}

func refreshRowFromHistory(site, group string, row *GroupResp, now time.Time) {
	statusStr, label := classifyProbeStatus(site, group, row.SubName, row.Name, now)
	_, lat, lastAt := getLastCheck(site, group, row.SubName, row.Name)
	row.Status = statusStr
	row.Label = label
	row.HealthScore = calcHealthScore(site, group, row.SubName, row.Name)
	row.LatencyMs = lat
	row.History = getHistory(site, group, row.SubName, row.Name, 120)
	row.Timelines = buildTimelines(site, group, row.SubName, row.Name, now)
	if lastAt.IsZero() {
		row.LastCheckedAt = nil
	} else {
		at := lastAt
		row.LastCheckedAt = &at
	}
}

// ensureFreshHistory 清理发生在一轮探测中间时，把内存里的旧曲线换成数据库里的记录。
func ensureFreshHistory(sections []Section) {
	epoch := atomic.LoadUint64(&historyEpoch)
	if epoch == atomic.LoadUint64(&appliedHistoryEpoch) {
		return
	}
	now := time.Now()
	for i := range sections {
		for j := range sections[i].Rows {
			refreshRowFromHistory(sections[i].Site, sections[i].Name, &sections[i].Rows[j], now)
		}
	}
	atomic.StoreUint64(&appliedHistoryEpoch, epoch)
}

// 探测只发一句短话，并限制回复长度。必须从正文里读到模型写出的文字才算可用。
const (
	probePrompt    = "hi"
	probeMaxTokens = 16
)

func probeBody(provider, modelID string) ([]byte, error) {
	switch provider {
	case "openai-response":
		return json.Marshal(map[string]any{
			"model":      modelID,
			"input":      probePrompt,
			"max_tokens": probeMaxTokens,
		})
	case "google":
		return json.Marshal(map[string]any{
			"contents": []map[string]any{
				{"parts": []map[string]string{{"text": probePrompt}}},
			},
			"generationConfig": map[string]int{"maxOutputTokens": probeMaxTokens},
		})
	default:
		return json.Marshal(map[string]any{
			"model":      modelID,
			"max_tokens": probeMaxTokens,
			"messages":   []map[string]string{{"role": "user", "content": probePrompt}},
		})
	}
}

func checkOpenAICompat(m Model) (bool, int, string) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0, "接口地址不正确"
	}
	body, err := probeBody("openai-compat", m.ID)
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	code, resp, lat, netErr := doPost(base+"/chat/completions", m.Key, body, m.Timeout)
	return finishProbe(code, resp, lat, netErr, openaiCompatHasReply(resp))
}

func checkOpenAIImage(m Model) (bool, int, string) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0, "接口地址不正确"
	}
	body, err := json.Marshal(map[string]any{
		"model":  m.ID,
		"prompt": "red square",
		"n":      1,
		"size":   "1024x1024",
	})
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	code, resp, lat, netErr := doPost(base+"/images/generations", m.Key, body, m.Timeout)
	return finishProbe(code, resp, lat, netErr, imageHasResult(resp))
}

func imageHasResult(raw []byte) bool {
	var v struct {
		Error json.RawMessage `json:"error"`
		Data  []struct {
			URL string `json:"url"`
			B64 string `json:"b64_json"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &v) != nil || hasAPIError(v.Error) {
		return false
	}
	for _, item := range v.Data {
		if strings.TrimSpace(item.URL) != "" || strings.TrimSpace(item.B64) != "" {
			return true
		}
	}
	return false
}

func checkOpenAIResponse(m Model) (bool, int, string) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0, "接口地址不正确"
	}
	body, err := probeBody("openai-response", m.ID)
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	code, resp, lat, netErr := doPost(base+"/responses", m.Key, body, m.Timeout)
	return finishProbe(code, resp, lat, netErr, openaiResponseHasReply(resp))
}

func checkGoogle(m Model) (bool, int, string) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0, "接口地址不正确"
	}
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", base, m.ID, m.Key)
	body, err := probeBody("google", m.ID)
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	code, resp, lat, netErr := doPost(url, "", body, m.Timeout)
	return finishProbe(code, resp, lat, netErr, googleHasReply(resp))
}

func checkGoogleImage(m Model) (bool, int, string) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0, "接口地址不正确"
	}
	body, err := json.Marshal(map[string]any{
		"contents": []map[string]any{{
			"role": "user",
			"parts": []map[string]any{{
				"text": "red square",
			}},
		}},
		"generationConfig": map[string]any{
			"responseModalities": []string{"TEXT", "IMAGE"},
			"imageConfig": map[string]any{
				"aspectRatio": "1:1",
			},
		},
	})
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	endpoint := fmt.Sprintf("%s/models/%s:generateContent", base, m.ID)
	req, err := http.NewRequest(http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	req.Header.Set("content-type", "application/json")
	if m.Key != "" {
		req.Header.Set("authorization", "Bearer "+m.Key)
		req.Header.Set("x-goog-api-key", m.Key)
	}
	code, resp, lat, netErr := doReq(req, m.Timeout)
	return finishProbe(code, resp, lat, netErr, googleHasImage(resp))
}

func readyBase(m Model) (string, bool) {
	base, err := normalizeBaseURL(m.Provider, m.BaseURL)
	return base, err == nil
}

func checkAnthropic(m Model) (bool, int, string) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0, "接口地址不正确"
	}
	body, err := probeBody("anthropic", m.ID)
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	req, err := http.NewRequest("POST", base+"/messages", bytes.NewReader(body))
	if err != nil {
		return false, 0, "探测请求无法生成"
	}
	req.Header.Set("x-api-key", m.Key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	code, resp, lat, netErr := doReq(req, m.Timeout)
	return finishProbe(code, resp, lat, netErr, anthropicHasReply(resp))
}

func doPost(url, key string, body []byte, timeout int) (int, []byte, int, string) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0, "探测请求无法生成"
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	return doReq(req, timeout)
}

func doReq(req *http.Request, timeout int) (int, []byte, int, string) {
	if timeout == 0 {
		timeout = 30
	}
	client := &http.Client{
		Timeout: time.Duration(timeout) * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		},
	}
	start := time.Now()
	resp, err := client.Do(req)
	lat := int(time.Since(start).Milliseconds())
	if err != nil {
		return 0, nil, lat, shortNetErr(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return resp.StatusCode, nil, lat, "读取响应失败"
	}
	return resp.StatusCode, b, lat, ""
}

func finishProbe(code int, body []byte, lat int, netErr string, ok bool) (bool, int, string) {
	if netErr != "" {
		return false, lat, netErr
	}
	if code > 0 && code < 300 && ok {
		return true, lat, ""
	}
	return false, lat, probeFailReason(code, body)
}

func probeFailReason(code int, body []byte) string {
	if msg := apiErrorMessage(body); msg != "" {
		if code > 0 {
			return clipText(fmt.Sprintf("HTTP %d %s", code, msg), 120)
		}
		return clipText(msg, 120)
	}
	if code <= 0 {
		return "连接失败"
	}
	if code >= 300 {
		return fmt.Sprintf("HTTP %d", code)
	}
	return "接口成功但没有返回内容"
}

func apiErrorMessage(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var v struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Msg     string          `json:"msg"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return ""
	}
	if s := strings.TrimSpace(v.Message); s != "" {
		return s
	}
	if s := strings.TrimSpace(v.Msg); s != "" {
		return s
	}
	if len(v.Error) == 0 || string(v.Error) == "null" {
		return ""
	}
	var text string
	if json.Unmarshal(v.Error, &text) == nil {
		return strings.TrimSpace(text)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(v.Error, &obj) == nil {
		return strings.TrimSpace(obj.Message)
	}
	return ""
}

func shortNetErr(err error) string {
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "timeout"), strings.Contains(s, "deadline"):
		return "请求超时"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝"
	case strings.Contains(s, "no such host"):
		return "域名无法解析"
	case strings.Contains(s, "connection reset"):
		return "连接被重置"
	case strings.Contains(s, "tls"), strings.Contains(s, "certificate"):
		return "证书校验失败"
	default:
		return "网络错误"
	}
}

func clipText(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func hasAPIError(raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	return true
}

func textFromContent(raw json.RawMessage) string {
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

func openaiCompatHasReply(raw []byte) bool {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Choices []struct {
			Text    string `json:"text"`
			Message struct {
				Content json.RawMessage `json:"content"`
			} `json:"message"`
			Delta struct {
				Content json.RawMessage `json:"content"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(raw, &v) != nil || hasAPIError(v.Error) {
		return false
	}
	for _, c := range v.Choices {
		if strings.TrimSpace(c.Text) != "" || textFromContent(c.Message.Content) != "" || textFromContent(c.Delta.Content) != "" {
			return true
		}
	}
	return false
}

func openaiResponseHasReply(raw []byte) bool {
	var v struct {
		Error      json.RawMessage `json:"error"`
		OutputText string          `json:"output_text"`
		Output     []struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if json.Unmarshal(raw, &v) != nil || hasAPIError(v.Error) {
		return false
	}
	if strings.TrimSpace(v.OutputText) != "" {
		return true
	}
	for _, item := range v.Output {
		for _, part := range item.Content {
			if strings.TrimSpace(part.Text) != "" {
				return true
			}
		}
	}
	return false
}

func googleHasReply(raw []byte) bool {
	var v struct {
		Error      json.RawMessage `json:"error"`
		Candidates []struct {
			Content struct {
				Parts []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &v) != nil || hasAPIError(v.Error) {
		return false
	}
	for _, c := range v.Candidates {
		for _, p := range c.Content.Parts {
			if strings.TrimSpace(p.Text) != "" {
				return true
			}
		}
	}
	return false
}

func anthropicHasReply(raw []byte) bool {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &v) != nil || hasAPIError(v.Error) {
		return false
	}
	for _, part := range v.Content {
		if strings.TrimSpace(part.Text) != "" {
			return true
		}
	}
	return false
}

func googleHasImage(raw []byte) bool {
	var v struct {
		Error    json.RawMessage `json:"error"`
		Response json.RawMessage `json:"response"`
	}
	if json.Unmarshal(raw, &v) != nil || hasAPIError(v.Error) {
		return false
	}
	if geminiRawHasImage(raw) {
		return true
	}
	return len(v.Response) > 0 && geminiRawHasImage(v.Response)
}

func geminiRawHasImage(raw []byte) bool {
	var v struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					InlineData struct {
						MimeType string `json:"mimeType"`
						Data     string `json:"data"`
					} `json:"inlineData"`
					InlineDataSnake struct {
						MimeType string `json:"mime_type"`
						Data     string `json:"data"`
					} `json:"inline_data"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	for _, c := range v.Candidates {
		for _, p := range c.Content.Parts {
			if geminiInlineImage(p.InlineData.MimeType, p.InlineData.Data) || geminiInlineImage(p.InlineDataSnake.MimeType, p.InlineDataSnake.Data) {
				return true
			}
		}
	}
	return false
}

func geminiInlineImage(mime, data string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(mime)), "image/") && strings.TrimSpace(data) != ""
}

func isOpenAIFamilyImageModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, "gpt-image-") ||
		strings.HasPrefix(id, "dall-e") ||
		id == "grok-imagine" ||
		id == "grok-imagine-edit" ||
		strings.HasPrefix(id, "grok-imagine-image")
}

func isGeminiImageModel(id string) bool {
	id = strings.ToLower(strings.TrimSpace(id))
	return strings.HasPrefix(id, "gemini-") && (strings.HasSuffix(id, "-image") || strings.Contains(id, "-image-"))
}

func probeProvider(m Model) string {
	switch m.Provider {
	case "openai-image", "google-image":
		return m.Provider
	}
	if isGeminiImageModel(m.ID) && (m.Provider == "" || m.Provider == "google" || m.Provider == "openai-compat") {
		return "google-image"
	}
	if isOpenAIFamilyImageModel(m.ID) && (m.Provider == "" || m.Provider == "openai-compat" || m.Provider == "openai-response") {
		return "openai-image"
	}
	if m.Provider == "" {
		return "openai-compat"
	}
	return m.Provider
}

func checkOne(m Model) (bool, int, string) {
	switch probeProvider(m) {
	case "openai-compat":
		return checkOpenAICompat(m)
	case "openai-image":
		return checkOpenAIImage(m)
	case "openai-response":
		return checkOpenAIResponse(m)
	case "google":
		return checkGoogle(m)
	case "google-image":
		return checkGoogleImage(m)
	case "anthropic":
		return checkAnthropic(m)
	default:
		return checkOpenAICompat(m)
	}
}

func providerFamily(p string) string {
	switch p {
	case "openai-compat", "openai-response", "openai-image":
		return "openai"
	case "anthropic":
		return "claude"
	case "google", "google-image":
		return "gemini"
	default:
		return "other"
	}
}

func snapshotRow(site, gName string, sg SubGroup, m Model, lat int, lastAt, now time.Time) GroupResp {
	statusStr, label := classifyProbeStatus(site, gName, sg.Name, m.ID, now)
	row := GroupResp{
		Name:        m.ID,
		SubName:     sg.Name,
		Ratio:       strings.TrimSpace(sg.Ratio),
		Family:      providerFamily(m.Provider),
		Status:      statusStr,
		Label:       label,
		HealthScore: calcHealthScore(site, gName, sg.Name, m.ID),
		LatencyMs:   lat,
		History:     getHistory(site, gName, sg.Name, m.ID, 120),
		Timelines:   buildTimelines(site, gName, sg.Name, m.ID, now),
	}
	if !lastAt.IsZero() {
		row.LastCheckedAt = &lastAt
	}
	return row
}

// hydrateStatus 启动后立刻用已有检测记录画出分组，不必等新一轮探测结束。
func hydrateStatus() {
	if db == nil {
		return
	}
	mu.RLock()
	sites := configSites(cfg)
	title := cfg.Frontend.Title
	mu.RUnlock()
	now := time.Now()
	var sections []Section
	for _, site := range sites {
		for _, g := range site.Groups {
			var rows []GroupResp
			for _, sg := range g.SubGroups {
				for _, m := range sg.Models {
					_, lat, lastAt := getLastCheck(site.Name, g.Name, sg.Name, m.ID)
					rows = append(rows, snapshotRow(site.Name, g.Name, sg, m, lat, lastAt, now))
				}
			}
			if len(rows) > 0 {
				sections = append(sections, Section{Site: site.Name, Name: g.Name, Rows: rows})
			}
		}
	}
	sections = applyConfigOrder(sections, sites)
	overallStatus, overallLabel := overallFromRows(sections)
	mu.Lock()
	statusV2 = StatusRespV2{
		Title:    title,
		Overall:  OverallStatus{Status: overallStatus, Label: overallLabel},
		Checking: false,
		Sites:    packSites(sections, sites),
		Sections: sections,
	}
	mu.Unlock()
}

// updateRow 检测完一个模型后立即更新共享状态，前端轮询可见
func updateRow(site, gName string, sg SubGroup, m Model, sections *[]Section, up bool, lat int) {
	_ = up
	_, _, lastAt := getLastCheck(site, gName, sg.Name, m.ID)
	row := snapshotRow(site, gName, sg, m, lat, lastAt, time.Now())

	mu.Lock()
	ensureFreshHistory(*sections)
	*sections = upsertSectionRow(*sections, site, gName, row)
	*sections = applyConfigOrder(*sections, configSites(cfg))
	applyLiveRatios(*sections)

	overallStatus, overallLabel := overallFromRows(*sections)

	*sections = knownSections(*sections, configSites(cfg))
	statusV2 = StatusRespV2{
		Title:    cfg.Frontend.Title,
		Overall:  OverallStatus{Status: overallStatus, Label: overallLabel},
		Checking: checking,
		Sites:    packSites(*sections, configSites(cfg)),
		Sections: *sections,
	}
	mu.Unlock()
}

func packSites(sections []Section, sites []Site) []SiteStatus {
	grouped := map[string][]Section{}
	for _, sec := range sections {
		grouped[sec.Site] = append(grouped[sec.Site], sec)
	}
	out := make([]SiteStatus, 0, len(sites))
	for _, site := range sites {
		secs := grouped[site.Name]
		if secs == nil {
			secs = []Section{}
		}
		out = append(out, SiteStatus{Name: site.Name, Sections: secs})
	}
	return out
}

func knownSections(sections []Section, sites []Site) []Section {
	if len(sites) == 0 {
		return sections
	}
	ok := make(map[string]bool, len(sites))
	for _, site := range sites {
		ok[site.Name] = true
	}
	out := make([]Section, 0, len(sections))
	for _, sec := range sections {
		if ok[sec.Site] {
			out = append(out, sec)
		}
	}
	return out
}

// upsertSectionRow 在 sections 中插入或更新一行，用于逐个检测时实时更新前端
func upsertSectionRow(sections []Section, site, gName string, row GroupResp) []Section {
	idx := -1
	for i, sec := range sections {
		if sec.Site == site && sec.Name == gName {
			idx = i
			break
		}
	}
	if idx == -1 {
		return append(sections, Section{Site: site, Name: gName, Rows: []GroupResp{row}})
	}
	sec := &sections[idx]
	for i, r := range sec.Rows {
		if r.Name == row.Name && r.SubName == row.SubName {
			sec.Rows[i] = row
			return sections
		}
	}
	sec.Rows = append(sec.Rows, row)
	return sections
}

// applyConfigOrder 让状态页分组、子组和模型顺序跟后台保存的顺序一致。
func applyConfigOrder(sections []Section, sites []Site) []Section {
	siteIndex := make(map[string]int, len(sites))
	groupIndex := make(map[string]int)
	type rank struct{ sub, model int }
	rowIndex := make(map[string]rank)
	for i, site := range sites {
		siteIndex[site.Name] = i
		for j, g := range site.Groups {
			groupIndex[site.Name+"\x1f"+g.Name] = j
			for k, sg := range g.SubGroups {
				for n, m := range sg.Models {
					rowIndex[site.Name+"\x1f"+g.Name+"\x1f"+sg.Name+"\x1f"+m.ID] = rank{k, n}
				}
			}
		}
	}
	sort.SliceStable(sections, func(a, b int) bool {
		sa, oka := siteIndex[sections[a].Site]
		sb, okb := siteIndex[sections[b].Site]
		if !oka {
			sa = len(sites)
		}
		if !okb {
			sb = len(sites)
		}
		if sa != sb {
			return sa < sb
		}
		ga, oka := groupIndex[sections[a].Site+"\x1f"+sections[a].Name]
		gb, okb := groupIndex[sections[b].Site+"\x1f"+sections[b].Name]
		if !oka {
			ga = len(sites) + 1
		}
		if !okb {
			gb = len(sites) + 1
		}
		return ga < gb
	})
	for i := range sections {
		rows := sections[i].Rows
		prefix := sections[i].Site + "\x1f" + sections[i].Name + "\x1f"
		sort.SliceStable(rows, func(a, b int) bool {
			ka, oka := rowIndex[prefix+rows[a].SubName+"\x1f"+rows[a].Name]
			kb, okb := rowIndex[prefix+rows[b].SubName+"\x1f"+rows[b].Name]
			if !oka {
				ka = rank{len(sites) + 1, len(sites) + 1}
			}
			if !okb {
				kb = rank{len(sites) + 1, len(sites) + 1}
			}
			if ka.sub != kb.sub {
				return ka.sub < kb.sub
			}
			return ka.model < kb.model
		})
		sections[i].Rows = rows
	}
	return sections
}

// applyLiveRatios 把后台刚保存的倍率写进已经展示的卡片，不必等下一轮探测。
func applyLiveRatios(sections []Section) {
	ratios := make(map[string]string)
	for _, site := range configSites(cfg) {
		for _, g := range site.Groups {
			for _, sg := range g.SubGroups {
				ratios[site.Name+"\x1f"+g.Name+"\x1f"+sg.Name] = strings.TrimSpace(sg.Ratio)
			}
		}
	}
	for i := range sections {
		siteName := sections[i].Site
		if siteName == "" && len(configSites(cfg)) == 1 {
			siteName = configSites(cfg)[0].Name
		}
		for j := range sections[i].Rows {
			key := siteName + "\x1f" + sections[i].Name + "\x1f" + sections[i].Rows[j].SubName
			if ratio, ok := ratios[key]; ok {
				sections[i].Rows[j].Ratio = ratio
			}
		}
	}
}

func syncLiveRatios() {
	mu.Lock()
	applyLiveRatios(statusV2.Sections)
	mu.Unlock()
}

func renameLiveSite(oldName, newName string) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || oldName == newName {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range statusV2.Sections {
		if statusV2.Sections[i].Site == oldName {
			statusV2.Sections[i].Site = newName
		}
	}
	statusV2.Sites = packSites(statusV2.Sections, configSites(cfg))
}

func renameLiveSubgroup(site, group, oldName, newName string) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || oldName == newName {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range statusV2.Sections {
		if statusV2.Sections[i].Site != site || statusV2.Sections[i].Name != group {
			continue
		}
		for j := range statusV2.Sections[i].Rows {
			if statusV2.Sections[i].Rows[j].SubName == oldName {
				statusV2.Sections[i].Rows[j].SubName = newName
			}
		}
	}
}

func reorderSections(site string, names []string) {
	index := make(map[string]int, len(names))
	for i, name := range names {
		index[strings.TrimSpace(name)] = i
	}
	mu.Lock()
	defer mu.Unlock()
	var picked []Section
	var slots []int
	for i, sec := range statusV2.Sections {
		if sec.Site != site {
			continue
		}
		slots = append(slots, i)
		picked = append(picked, sec)
	}
	sort.SliceStable(picked, func(a, b int) bool {
		ia, oka := index[picked[a].Name]
		ib, okb := index[picked[b].Name]
		if !oka {
			ia = len(names)
		}
		if !okb {
			ib = len(names)
		}
		return ia < ib
	})
	for n, i := range slots {
		statusV2.Sections[i] = picked[n]
	}
	statusV2.Sites = packSites(statusV2.Sections, configSites(cfg))
}

func reorderSectionRows(site, group string, names []string) {
	index := make(map[string]int, len(names))
	for i, name := range names {
		index[strings.TrimSpace(name)] = i
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range statusV2.Sections {
		if statusV2.Sections[i].Site != site || statusV2.Sections[i].Name != group {
			continue
		}
		rows := statusV2.Sections[i].Rows
		sort.SliceStable(rows, func(a, b int) bool {
			ia, oka := index[rows[a].SubName]
			ib, okb := index[rows[b].SubName]
			if !oka {
				ia = len(names)
			}
			if !okb {
				ib = len(names)
			}
			return ia < ib
		})
		statusV2.Sections[i].Rows = rows
	}
	statusV2.Sites = packSites(statusV2.Sections, configSites(cfg))
}

// pruneStaleSections 根据 config 当前条目移除 sections 中已不存在的 group/subGroup/model 行
func pruneStaleSections(sections []Section, cfg Config) []Section {
	valid := make(map[string]bool)
	for _, site := range configSites(cfg) {
		for _, g := range site.Groups {
			for _, sg := range g.SubGroups {
				for _, m := range sg.Models {
					valid[site.Name+"|"+g.Name+"|"+sg.Name+"|"+m.ID] = true
				}
			}
		}
	}

	var result []Section
	for _, sec := range sections {
		var keptRows []GroupResp
		for _, row := range sec.Rows {
			key := sec.Site + "|" + sec.Name + "|" + row.SubName + "|" + row.Name
			if valid[key] {
				keptRows = append(keptRows, row)
			}
		}
		if len(keptRows) > 0 {
			result = append(result, Section{Site: sec.Site, Name: sec.Name, Rows: keptRows})
		}
	}
	return result
}

type probeTarget struct {
	site, gName, sgName, ratio string
	m                          Model
}

func runProbes(targets []probeTarget, sections *[]Section, saveFailure bool) (failed []probeTarget, totalLat, latCount int) {
	if len(targets) == 0 {
		return nil, 0, 0
	}
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	wg.Add(len(targets))
	for _, target := range targets {
		go func(target probeTarget) {
			defer wg.Done()
			up, lat, reason := checkOne(target.m)
			if up || saveFailure {
				saveCheck(target.site, target.gName, target.sgName, target.m.ID, up, lat, reason)
			}
			if !up && saveFailure && reason != "" {
				log.Printf("probe %s/%s: %s", target.gName, target.m.ID, reason)
			}
			if up {
				resultMu.Lock()
				totalLat += lat
				latCount++
				resultMu.Unlock()
			} else if !saveFailure {
				resultMu.Lock()
				failed = append(failed, target)
				resultMu.Unlock()
			}
			if up || saveFailure {
				updateRow(target.site, target.gName, SubGroup{Name: target.sgName, Ratio: target.ratio}, target.m, sections, up, lat)
			}
		}(target)
	}
	wg.Wait()
	return failed, totalLat, latCount
}

func refresh(only map[string]bool) bool {
	// 防止并发执行：如果已有一次检测正在运行，则跳过
	mu.Lock()
	if checking {
		mu.Unlock()
		return false
	}
	checking = true
	mu.Unlock()

	loadConfig()

	prevRows := map[string]string{}
	mu.RLock()
	prevOverall := statusV2.Overall.Status
	for _, sec := range statusV2.Sections {
		for _, r := range sec.Rows {
			prevRows[rowStatusKey(sec, r)] = r.Status
		}
	}
	mu.RUnlock()

	// 以当前已有状态为起点，本轮模型同时探测
	mu.RLock()
	sections := statusV2.Sections
	if sections == nil {
		sections = []Section{}
	}
	mu.RUnlock()

	// 根据 config 清理已不存在的条目，并立刻按后台顺序发布
	sections = pruneStaleSections(sections, cfg)
	sections = knownSections(sections, configSites(cfg))
	mu.Lock()
	ensureFreshHistory(sections)
	sections = applyConfigOrder(sections, configSites(cfg))
	applyLiveRatios(sections)
	statusV2.Sections = sections
	statusV2.Sites = packSites(sections, configSites(cfg))
	mu.Unlock()

	now := time.Now()
	var targets []probeTarget
	for _, site := range configSites(cfg) {
		if only != nil && !only[site.Name] {
			continue
		}
		for _, g := range site.Groups {
			for _, sg := range g.SubGroups {
				for _, m := range sg.Models {
					targets = append(targets, probeTarget{site.Name, g.Name, sg.Name, sg.Ratio, m})
				}
			}
		}
	}
	failed, totalLat, latCount := runProbes(targets, &sections, false)
	if len(failed) > 0 {
		time.Sleep(5 * time.Second)
		_, retryLat, retryCount := runProbes(failed, &sections, true)
		totalLat += retryLat
		latCount += retryCount
	}

	// 全部检测完毕，标记 checking = false
	mu.Lock()
	ensureFreshHistory(sections)
	sections = knownSections(sections, configSites(cfg))
	sections = applyConfigOrder(sections, configSites(cfg))
	applyLiveRatios(sections)
	overallStatus, overallLabel := overallFromRows(sections)
	statusV2 = StatusRespV2{
		Title:           cfg.Frontend.Title,
		Overall:         OverallStatus{Status: overallStatus, Label: overallLabel},
		LastCompletedAt: &now,
		Checking:        false,
		Sites:           packSites(sections, configSites(cfg)),
		Sections:        sections,
	}
	checking = false
	mu.Unlock()

	// push to Uptime Kuma
	// 1. 推送整体状态
	if cfg.Kuma.BaseURL != "" && cfg.Kuma.Slug != "" {
		avgPing := 0
		if latCount > 0 {
			avgPing = totalLat / latCount
		}
		pingKuma(cfg.Kuma.Slug, overallStatus != "down", avgPing)
	}
	// 2. 推送每个子组的独立状态
	if cfg.Kuma.BaseURL != "" {
		for _, site := range configSites(cfg) {
			for _, g := range site.Groups {
				for _, sg := range g.SubGroups {
					if sg.KumaSlug != "" {
						var sgLatSum, sgLatCount int
						sgUp := len(sg.Models) > 0
						for _, m := range sg.Models {
							found := false
							for _, sec := range sections {
								if sec.Site != site.Name || sec.Name != g.Name {
									continue
								}
								for _, r := range sec.Rows {
									if r.SubName == sg.Name && r.Name == m.ID {
										found = true
										if r.Status == "down" {
											sgUp = false
										}
									}
								}
							}
							if !found {
								sgUp = false
							}
							if _, lat, at := getLastCheck(site.Name, g.Name, sg.Name, m.ID); !at.IsZero() {
								sgLatSum += lat
								sgLatCount++
							}
						}
						sgAvgPing := 0
						if sgLatCount > 0 {
							sgAvgPing = sgLatSum / sgLatCount
						}
						pingKuma(sg.KumaSlug, sgUp, sgAvgPing)
					}
				}
			}
		}
	}

	mu.RLock()
	tg := cfg.Telegram
	tgToken := tg.BotToken
	tgChats := append([]string(nil), tg.ChatIDs...)
	tgEvery := time.Duration(tg.Interval) * time.Minute
	tgTitle := cfg.Frontend.Title
	mu.RUnlock()
	tgButtons, tgPerRow := telegramButtons(tg)
	tgMarkup := telegramKeyboard(tgButtons, tgPerRow)
	// 启动后的第一轮，以及清空记录后的第一轮，只当基线，不推送。
	nowPush := time.Now()
	historyNow := atomic.LoadUint64(&historyEpoch)
	if tgToken == "" || len(tgChats) == 0 {
		noteTelegram("未填写 Bot Token 或群组，不推送")
	} else if !telegramBaselined || historyNow != atomic.LoadUint64(&telegramSyncedEpoch) {
		telegramBaselined = true
		atomic.StoreUint64(&telegramSyncedEpoch, historyNow)
		resetTelegramQueue()
		markTelegramPushed(nowPush)
		noteTelegram("启动后或清空记录后的第一轮只记基线，不推送")
	} else if text, reason := takeTelegram(prevRows, prevOverall, sections, overallStatus, overallLabel, tgEvery, nowPush); text == "" {
		noteTelegram(reason)
	} else {
		noteTelegram(text)
		images, imgErr := renderStatusBoard(tgTitle, sections, nowPush)
		if imgErr != nil {
			noteTelegram("状态图生成失败，改为文字：" + imgErr.Error())
			texts := splitTelegram(formatStatusBoard(tgTitle, sections, nowPush))
			if err := sendTelegramAll(tgToken, tgChats, texts, tgMarkup); err != nil {
				noteTelegram("文字发送失败：" + err.Error())
			} else {
				noteTelegram(fmt.Sprintf("文字已发到 %d 个群", len(tgChats)))
			}
		} else if err := sendTelegramPhotos(tgToken, tgChats, images, "", tgMarkup); err != nil {
			noteTelegram("图片发送失败：" + err.Error())
			texts := splitTelegram(formatStatusBoard(tgTitle, sections, nowPush))
			if err := sendTelegramAll(tgToken, tgChats, texts, tgMarkup); err != nil {
				noteTelegram("文字发送失败：" + err.Error())
			} else {
				noteTelegram(fmt.Sprintf("已改用文字发到 %d 个群", len(tgChats)))
			}
		} else {
			noteTelegram(fmt.Sprintf("状态图已发到 %d 个群", len(tgChats)))
		}
	}

	cleanupOld()
	return true
}

func statusWord(status string) string {
	switch status {
	case "up":
		return "正常"
	case "degraded":
		return "降级"
	case "down":
		return "异常"
	default:
		return "无采样"
	}
}

func overallWord(status string) string {
	switch status {
	case "down":
		return "部分异常"
	case "degraded":
		return "降级"
	case "up":
		return "运行正常"
	default:
		return "采样中"
	}
}

type tgItem struct {
	group, sub, model, from, to string
}

type telegramQueue struct {
	sync.Mutex
	items                         map[string]tgItem
	overallFrom, overallTo, label string
	last                          time.Time
}

var tgQueue telegramQueue

func rowStatusKey(sec Section, row GroupResp) string {
	key := sec.Name + "\x1f" + row.SubName + "\x1f" + row.Name
	if sec.Site != "" {
		key = sec.Site + "\x1f" + key
	}
	return key
}

func telegramDigest(prev map[string]string, prevOverall string, sections []Section, overall, overallLabel string) string {
	items, of, ot, ol := collectTelegram(prev, prevOverall, sections, overall, overallLabel)
	return formatTelegram(items, of, ot, ol)
}

func collectTelegram(prev map[string]string, prevOverall string, sections []Section, overall, overallLabel string) ([]tgItem, string, string, string) {
	if prevOverall == "" && len(prev) == 0 {
		return nil, "", "", ""
	}
	var items []tgItem
	for _, sec := range sections {
		for _, r := range sec.Rows {
			old, ok := prev[rowStatusKey(sec, r)]
			if !ok || old == "" || old == r.Status {
				continue
			}
			items = append(items, tgItem{sec.Name, r.SubName, r.Name, old, r.Status})
		}
	}
	if prevOverall == "" || overall == "" || prevOverall == overall {
		return items, "", "", ""
	}
	label := overallLabel
	if label == "" {
		label = overallWord(overall)
	}
	return items, prevOverall, overall, label
}

func formatTelegram(items []tgItem, overallFrom, overallTo, overallLabel string) string {
	sort.Slice(items, func(i, j int) bool {
		if items[i].group != items[j].group {
			return items[i].group < items[j].group
		}
		if items[i].sub != items[j].sub {
			return items[i].sub < items[j].sub
		}
		return items[i].model < items[j].model
	})
	extra := 0
	if len(items) > 30 {
		extra = len(items) - 30
		items = items[:30]
	}
	var b strings.Builder
	if len(items) > 0 {
		b.WriteString("状态变化")
		for _, it := range items {
			fmt.Fprintf(&b, "\n\n%s / %s\n%s\n%s → %s", it.group, it.sub, it.model, statusWord(it.from), statusWord(it.to))
		}
		if extra > 0 {
			fmt.Fprintf(&b, "\n\n还有 %d 项变化", extra)
		}
	}
	if overallFrom != "" && overallTo != "" && overallFrom != overallTo {
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		label := overallLabel
		if label == "" {
			label = overallWord(overallTo)
		}
		fmt.Fprintf(&b, "整体：%s → %s", overallWord(overallFrom), label)
	}
	return b.String()
}

func markTelegramPushed(now time.Time) {
	tgQueue.Lock()
	tgQueue.last = now
	tgQueue.Unlock()
}

func resetTelegramQueue() {
	tgQueue.Lock()
	tgQueue.items = map[string]tgItem{}
	tgQueue.overallFrom, tgQueue.overallTo, tgQueue.label = "", "", ""
	tgQueue.last = time.Time{}
	tgQueue.Unlock()
}

// takeTelegram 把本轮变化并进待发送队列。间隔为 0 时立刻返回消息；否则等到间隔结束再汇总。
func takeTelegram(prev map[string]string, prevOverall string, sections []Section, overall, overallLabel string, every time.Duration, now time.Time) (string, string) {
	items, of, ot, ol := collectTelegram(prev, prevOverall, sections, overall, overallLabel)
	tgQueue.Lock()
	defer tgQueue.Unlock()
	if tgQueue.items == nil {
		tgQueue.items = map[string]tgItem{}
	}
	for _, it := range items {
		key := it.group + "\x1f" + it.sub + "\x1f" + it.model
		if prevItem, ok := tgQueue.items[key]; ok {
			it.from = prevItem.from
		}
		if it.from == it.to {
			delete(tgQueue.items, key)
			continue
		}
		tgQueue.items[key] = it
	}
	if of != "" && ot != "" && of != ot {
		if tgQueue.overallFrom == "" {
			tgQueue.overallFrom = of
		}
		tgQueue.overallTo = ot
		tgQueue.label = ol
		if tgQueue.overallFrom == tgQueue.overallTo {
			tgQueue.overallFrom, tgQueue.overallTo, tgQueue.label = "", "", ""
		}
	}
	if len(tgQueue.items) == 0 && tgQueue.overallFrom == "" {
		return "", "没有状态变化，不推送"
	}
	if every > 0 && !tgQueue.last.IsZero() && now.Sub(tgQueue.last) < every {
		left := every - now.Sub(tgQueue.last)
		mins := int((left + time.Minute - time.Nanosecond) / time.Minute)
		if mins < 1 {
			mins = 1
		}
		return "", fmt.Sprintf("有变化但未到推送间隔，约 %d 分钟后再发：%s", mins, telegramChangeSummary(tgQueue.items))
	}
	pending := make([]tgItem, 0, len(tgQueue.items))
	for _, it := range tgQueue.items {
		pending = append(pending, it)
	}
	text := formatTelegram(pending, tgQueue.overallFrom, tgQueue.overallTo, tgQueue.label)
	tgQueue.items = map[string]tgItem{}
	tgQueue.overallFrom, tgQueue.overallTo, tgQueue.label = "", "", ""
	tgQueue.last = now
	return text, ""
}

func telegramChangeSummary(items map[string]tgItem) string {
	if len(items) == 0 {
		return "无模型条目"
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		names = append(names, it.model+" "+statusWord(it.from)+"→"+statusWord(it.to))
	}
	sort.Strings(names)
	if len(names) > 8 {
		extra := len(names) - 8
		names = append(names[:8], fmt.Sprintf("另有 %d 个", extra))
	}
	return strings.Join(names, "，")
}

func formatStatusBoard(title string, sections []Section, now time.Time) string {
	title = strings.TrimSpace(title)
	if title == "" {
		title = "分组状态监控"
	}
	dup := map[string]int{}
	for _, sec := range sections {
		for _, row := range sec.Rows {
			dup[sec.Name+"\x1f"+row.SubName]++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n%s", title, now.Format("01/02 15:04"))
	for _, sec := range sections {
		if len(sec.Rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "\n\n▸ %s", sec.Name)
		for _, row := range sec.Rows {
			b.WriteByte('\n')
			b.WriteString(boardLine(sec.Name, row, dup))
		}
	}
	return b.String()
}

func boardLine(group string, row GroupResp, dup map[string]int) string {
	name := strings.TrimSpace(row.SubName)
	if name == "" {
		name = group
	}
	name = trimGroupPrefix(group, name)
	if dup[group+"\x1f"+row.SubName] > 1 && strings.TrimSpace(row.Name) != "" {
		name += " · " + strings.TrimSpace(row.Name)
	}
	rate, ok := boardRate(row.Timelines["hour"])
	line := fmt.Sprintf("%s %s  %s%s", statusMark(row.Status), formatBoardRate(rate, ok), name, boardRatio(row.Ratio))
	if marks := boardMarks(row.Timelines["hour"]); marks != "" {
		line += "  " + marks
	}
	return line
}

func statusMark(status string) string {
	switch status {
	case "up":
		return "🟢"
	case "degraded":
		return "🟡"
	case "down":
		return "🔴"
	default:
		return "⚪"
	}
}

func formatBoardRate(rate float64, ok bool) string {
	if !ok {
		return "--"
	}
	if rate < 0 {
		rate = 0
	}
	if rate > 100 {
		rate = 100
	}
	whole := int(rate + 0.5)
	if rate-float64(whole) < 0.05 && float64(whole)-rate < 0.05 {
		return fmt.Sprintf("%d%%", whole)
	}
	return fmt.Sprintf("%.1f%%", rate)
}

func trimGroupPrefix(group, name string) string {
	group = strings.TrimSpace(group)
	name = strings.TrimSpace(name)
	if group == "" || len(name) <= len(group) || !strings.EqualFold(name[:len(group)], group) {
		return name
	}
	rest := name[len(group):]
	r, _ := utf8.DecodeRuneInString(rest)
	if !isBoardSep(r) {
		return name
	}
	rest = strings.TrimLeftFunc(rest, isBoardSep)
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return name
	}
	return rest
}

func isBoardSep(r rune) bool {
	switch r {
	case ' ', '\t', '-', '—', '–', '·', '•', '|', '｜', ':', '：', '/':
		return true
	default:
		return false
	}
}

func telegramButtons(t TelegramConfig) ([]TelegramButton, int) {
	per := t.ButtonsPerRow
	if per < 1 || per > 4 {
		per = 1
	}
	if t.Buttons != nil {
		return t.Buttons, per
	}
	return legacyTelegramButtons(t), per
}

func telegramKeyboard(buttons []TelegramButton, perRow int) json.RawMessage {
	if perRow < 1 || perRow > 4 {
		perRow = 1
	}
	var rows [][]map[string]string
	var row []map[string]string
	for _, button := range buttons {
		name := strings.TrimSpace(button.Name)
		link := strings.TrimSpace(button.URL)
		if name == "" || link == "" {
			continue
		}
		row = append(row, map[string]string{"text": name, "url": link})
		if len(row) == perRow {
			rows = append(rows, row)
			row = nil
		}
	}
	if len(row) > 0 {
		rows = append(rows, row)
	}
	if len(rows) == 0 {
		return nil
	}
	raw, err := json.Marshal(map[string]any{"inline_keyboard": rows})
	if err != nil {
		return nil
	}
	return raw
}

func sameBoardLink(a, b string) bool {
	trim := func(v string) string {
		v = strings.TrimSpace(strings.ToLower(v))
		v = strings.TrimPrefix(v, "https://")
		v = strings.TrimPrefix(v, "http://")
		return strings.Trim(v, "/")
	}
	return trim(a) != "" && trim(a) == trim(b)
}

func boardRate(buckets []TimelineBucket) (float64, bool) {
	for i := len(buckets) - 1; i >= 0; i-- {
		if buckets[i].Samples > 0 {
			return buckets[i].Rate, true
		}
	}
	return 0, false
}

func boardMarks(buckets []TimelineBucket) string {
	if len(buckets) > 12 {
		buckets = buckets[len(buckets)-12:]
	}
	var sampled []TimelineBucket
	for _, bucket := range buckets {
		if bucket.Samples > 0 {
			sampled = append(sampled, bucket)
		}
	}
	if len(sampled) < 2 {
		return ""
	}
	if len(sampled) > 6 {
		sampled = sampled[len(sampled)-6:]
	}
	var b strings.Builder
	for _, bucket := range sampled {
		switch {
		case bucket.Rate >= 90:
			b.WriteString("🟩")
		case bucket.Rate >= 60:
			b.WriteString("🟨")
		default:
			b.WriteString("🟥")
		}
	}
	return b.String()
}

func boardRatio(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.Contains(v, "×") || strings.Contains(v, "倍") || strings.ContainsAny(v, "xX") {
		return " " + v
	}
	return " ×" + v
}

func splitTelegram(text string) []string {
	if len([]rune(text)) <= 3800 {
		return []string{text}
	}
	parts := strings.Split(text, "\n\n")
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		out = append(out, b.String())
		b.Reset()
	}
	for _, part := range parts {
		if b.Len() > 0 && len([]rune(b.String()))+len([]rune(part))+2 > 3800 {
			flush()
		}
		if b.Len() > 0 {
			b.WriteString("\n\n")
		}
		b.WriteString(part)
	}
	flush()
	if len(out) == 0 {
		return []string{text}
	}
	return out
}

var (
	telegramAPI    = "https://api.telegram.org"
	telegramClient = &http.Client{Timeout: 25 * time.Second}
)

type tgLogLine struct {
	Time string `json:"time"`
	Text string `json:"text"`
}

var tgLogs struct {
	sync.Mutex
	lines []tgLogLine
}

func noteTelegram(text string) {
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	if len([]rune(text)) > 300 {
		text = string([]rune(text)[:300])
	}
	log.Println("telegram:", text)
	tgLogs.Lock()
	tgLogs.lines = append(tgLogs.lines, tgLogLine{
		Time: time.Now().Format("01-02 15:04:05"),
		Text: text,
	})
	if len(tgLogs.lines) > 80 {
		tgLogs.lines = tgLogs.lines[len(tgLogs.lines)-80:]
	}
	tgLogs.Unlock()
}

func telegramLogs() []tgLogLine {
	tgLogs.Lock()
	defer tgLogs.Unlock()
	out := make([]tgLogLine, len(tgLogs.lines))
	for i := range tgLogs.lines {
		out[len(out)-1-i] = tgLogs.lines[i]
	}
	return out
}

func sendTelegramAll(token string, chats []string, texts []string, markup json.RawMessage) error {
	var failed []string
	for _, chat := range chats {
		for i, text := range texts {
			var mark json.RawMessage
			if i == len(texts)-1 {
				mark = markup
			}
			if err := sendTelegram(token, chat, text, mark); err != nil {
				failed = append(failed, chat+"："+err.Error())
				break
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	msg := strings.Join(failed, "；")
	if len([]rune(msg)) > 300 {
		msg = string([]rune(msg)[:300])
	}
	return errors.New(msg)
}

func sendTelegram(token, chatID, text string, markup json.RawMessage) error {
	_, err := sendTelegramMessage(token, chatID, text, markup)
	return err
}

// sendTelegramMessage sends a text message and returns its message_id.
func sendTelegramMessage(token, chatID, text string, markup json.RawMessage) (int, error) {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	text = strings.TrimSpace(text)
	if token == "" || chatID == "" || text == "" {
		return 0, nil
	}
	payloadMap := map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	}
	if len(markup) > 0 {
		payloadMap["reply_markup"] = markup
	}
	payload, err := json.Marshal(payloadMap)
	if err != nil {
		return 0, errors.New("无法编码 Telegram 消息")
	}
	var parsed struct {
		Result struct {
			MessageID int `json:"message_id"`
		} `json:"result"`
	}
	if err := telegramCall(token, "sendMessage", payload, &parsed); err != nil {
		return 0, err
	}
	return parsed.Result.MessageID, nil
}

// deleteTelegramMessage removes one chat message. Deleting other people's
// messages in a group needs the bot to be an admin with "delete messages".
func deleteTelegramMessage(token, chatID string, messageID int) error {
	payload, err := json.Marshal(map[string]any{"chat_id": strings.TrimSpace(chatID), "message_id": messageID})
	if err != nil {
		return errors.New("无法编码 Telegram 消息")
	}
	return telegramCall(strings.TrimSpace(token), "deleteMessage", payload, nil)
}

// telegramCall posts a JSON payload to a Bot API method and decodes the reply into out.
func telegramCall(token, method string, payload []byte, out any) error {
	req, err := http.NewRequest(http.MethodPost, telegramAPI+"/bot"+token+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return errors.New("无法创建 Telegram 请求")
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := telegramClient.Do(req)
	if err != nil {
		return errors.New("无法连接 Telegram")
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(body, &parsed)
	if resp.StatusCode == http.StatusOK && parsed.OK {
		if out != nil {
			_ = json.Unmarshal(body, out)
		}
		return nil
	}
	msg := strings.TrimSpace(parsed.Description)
	if msg == "" {
		msg = "推送失败"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return errors.New(msg)
}

func sendTelegramPhotos(token string, chats []string, images [][]byte, caption string, markup json.RawMessage) error {
	var failed []string
	for _, chat := range chats {
		for i, img := range images {
			text := ""
			var mark json.RawMessage
			if i == 0 {
				text = caption
				mark = markup
			}
			if err := sendTelegramPhoto(token, chat, img, text, mark); err != nil {
				failed = append(failed, chat+"："+err.Error())
				break
			}
		}
	}
	if len(failed) == 0 {
		return nil
	}
	msg := strings.Join(failed, "；")
	if len([]rune(msg)) > 300 {
		msg = string([]rune(msg)[:300])
	}
	return errors.New(msg)
}

func sendTelegramPhoto(token, chatID string, png []byte, caption string, markup json.RawMessage) error {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	caption = strings.TrimSpace(caption)
	if token == "" || chatID == "" || len(png) == 0 {
		return nil
	}
	var body bytes.Buffer
	w := multipart.NewWriter(&body)
	_ = w.WriteField("chat_id", chatID)
	if caption != "" {
		_ = w.WriteField("caption", caption)
	}
	if len(markup) > 0 {
		_ = w.WriteField("reply_markup", string(markup))
	}
	part, err := w.CreateFormFile("photo", "status.png")
	if err != nil {
		return errors.New("无法编码 Telegram 图片")
	}
	if _, err := part.Write(png); err != nil {
		return errors.New("无法编码 Telegram 图片")
	}
	if err := w.Close(); err != nil {
		return errors.New("无法编码 Telegram 图片")
	}
	req, err := http.NewRequest(http.MethodPost, telegramAPI+"/bot"+token+"/sendPhoto", &body)
	if err != nil {
		return errors.New("无法创建 Telegram 请求")
	}
	req.Header.Set("Content-Type", w.FormDataContentType())
	resp, err := telegramClient.Do(req)
	if err != nil {
		return errors.New("无法连接 Telegram")
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var parsed struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal(respBody, &parsed)
	if resp.StatusCode == http.StatusOK && parsed.OK {
		return nil
	}
	msg := strings.TrimSpace(parsed.Description)
	if msg == "" {
		msg = "推送失败"
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	return errors.New(msg)
}

func pingKuma(slug string, up bool, pingMs int) {
	if slug == "" {
		return
	}
	url := strings.TrimSuffix(cfg.Kuma.BaseURL, "/") + "/api/push/" + slug
	statusStr := "down"
	if up {
		statusStr = "up"
	}
	pingParam := ""
	if pingMs > 0 {
		pingParam = fmt.Sprintf("%d", pingMs)
	}
	resp, err := http.Get(url + "?status=" + statusStr + "&msg=ok&ping=" + pingParam)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// Kuma 公开状态页兼容结构
type KumaCategoryConfig struct {
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	Description string `json:"description,omitempty"`
}

type KumaCategoryMonitor struct {
	ID      int     `json:"id"`
	Name    string  `json:"name"`
	Type    string  `json:"type"`
	Status  int     `json:"status"` // 0=down 1=up 2=pending
	Latency int     `json:"latency"`
	Msg     string  `json:"msg"`
	Uptime  float64 `json:"uptime"`
}

type KumaCategoryGroup struct {
	ID          int                   `json:"id"`
	Name        string                `json:"name"`
	Weight      int                   `json:"weight"`
	MonitorList []KumaCategoryMonitor `json:"monitorList"`
}

type KumaCategoryResp struct {
	Config          KumaCategoryConfig  `json:"config"`
	PublicGroupList []KumaCategoryGroup `json:"publicGroupList"`
}

// ---- Heartbeat 兼容结构（NewAPI 需要 /api/status-page/heartbeat/:slug）----

type HeartbeatPublic struct {
	Status int    `json:"status"` // 0=down 1=up 2=pending
	Time   string `json:"time"`
	Msg    string `json:"msg"`
	Ping   int    `json:"ping"`
}

// getHeartbeats 查询最近 limit 条心跳记录，返回按时间正序排列
func getHeartbeats(siteName, groupName, subName, modelID string, limit int) []HeartbeatPublic {
	rows, err := db.Query(
		"SELECT up, latency, checked_at FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT ?",
		siteName, groupName, subName, modelID, limit,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var items []HeartbeatPublic
	for rows.Next() {
		var up, lat int
		var ts int64
		rows.Scan(&up, &lat, &ts)
		status := 0
		if up == 1 {
			status = 1
		}
		items = append(items, HeartbeatPublic{
			Status: status,
			Time:   time.Unix(ts, 0).UTC().Format(time.RFC3339),
			Msg:    "",
			Ping:   lat,
		})
	}
	// 反转为时间正序
	for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
		items[i], items[j] = items[j], items[i]
	}
	return items
}

// calcUptime24h 计算 24 小时内的 uptime（0~1）
func calcUptime24h(siteName, groupName, subName, modelID string) float64 {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	var total, upCount int
	rows, err := db.Query(
		"SELECT up FROM checks WHERE site_name = ? AND group_name = ? AND sub_name = ? AND model_id = ? AND checked_at >= ?",
		siteName, groupName, subName, modelID, cutoff,
	)
	if err != nil {
		return 1.0
	}
	defer rows.Close()
	for rows.Next() {
		var up int
		rows.Scan(&up)
		total++
		if up == 1 {
			upCount++
		}
	}
	if total == 0 {
		return 1.0
	}
	return float64(upCount) / float64(total)
}

// monitorRef 记录一个模型的分组上下文和分配的 monitor ID
type monitorRef struct {
	SiteName  string
	GroupName string
	SubName   string
	ModelID   string
	MonitorID int
}

// collectMonitorRefs 根据 slug 收集 monitor 引用列表
// overall slug 返回全部模型（全局唯一 ID），子组 slug 返回该子组的模型
func collectMonitorRefs(cfg Config, slug string) []monitorRef {
	var refs []monitorRef

	// 检查是否为 overall slug
	if slug == cfg.Kuma.Slug {
		id := 1
		for _, site := range configSites(cfg) {
			for _, g := range site.Groups {
				for _, sg := range g.SubGroups {
					for _, m := range sg.Models {
						refs = append(refs, monitorRef{site.Name, g.Name, sg.Name, m.ID, id})
						id++
					}
				}
			}
		}
		return refs
	}

	// 查找子组 slug
	for _, site := range configSites(cfg) {
		for _, g := range site.Groups {
			for _, sg := range g.SubGroups {
				if sg.KumaSlug == slug {
					for i, m := range sg.Models {
						refs = append(refs, monitorRef{site.Name, g.Name, sg.Name, m.ID, i + 1})
					}
					return refs
				}
			}
		}
	}
	return nil
}

func writeJSON(w http.ResponseWriter, code int, v interface{}) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.Header().Set("access-control-allow-origin", "*")
	w.Header().Set("access-control-allow-headers", "*")
	w.WriteHeader(code)
	b, _ := json.Marshal(v)
	w.Write(b)
}

func apiStatus(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	s := statusV2
	sites := configSites(cfg)
	s.Sections = knownSections(s.Sections, sites)
	s.Sites = packSites(s.Sections, sites)
	c := checking
	cooldown := cfg.ManualRefreshCooldown
	if cooldown == 0 {
		cooldown = 60
	}
	mu.RUnlock()
	s.Checking = c

	// 计算手动刷新剩余冷却时间
	manualRefreshMu.RLock()
	last := lastManualRefresh
	manualRefreshMu.RUnlock()
	if !last.IsZero() {
		elapsed := int(time.Since(last).Seconds())
		remaining := cooldown - elapsed
		if remaining < 0 {
			remaining = 0
		}
		s.ManualRefreshRemaining = remaining
	}

	writeJSON(w, 200, s)
}

func apiConfig(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	interval := cfg.Interval
	frontend := cfg.Frontend
	cooldown := cfg.ManualRefreshCooldown
	if cooldown == 0 {
		cooldown = 60
	}
	var groupSettings []map[string]interface{}
	for _, site := range configSites(cfg) {
		for _, g := range site.Groups {
			groupSettings = append(groupSettings, map[string]interface{}{
				"site":          site.Name,
				"name":          g.Name,
				"manualRefresh": g.ManualRefresh,
			})
		}
	}
	if groupSettings == nil {
		groupSettings = []map[string]interface{}{}
	}
	mu.RUnlock()
	writeJSON(w, 200, map[string]interface{}{
		"interval":              interval,
		"frontend":              frontend,
		"manualRefreshCooldown": cooldown,
		"groups":                groupSettings,
	})
}

// manualRefreshRemaining 计算手动刷新剩余冷却秒数
func manualRefreshRemaining() int {
	manualRefreshMu.RLock()
	last := lastManualRefresh
	manualRefreshMu.RUnlock()
	if last.IsZero() {
		return 0
	}
	mu.RLock()
	cooldown := cfg.ManualRefreshCooldown
	if cooldown == 0 {
		cooldown = 60
	}
	mu.RUnlock()
	elapsed := int(time.Since(last).Seconds())
	remaining := cooldown - elapsed
	if remaining < 0 {
		remaining = 0
	}
	return remaining
}

// apiRefresh 手动刷新检测接口
func apiRefresh(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}

	// 检查冷却时间
	if remaining := manualRefreshRemaining(); remaining > 0 {
		writeJSON(w, 429, map[string]interface{}{
			"error":     "cooldown",
			"remaining": remaining,
		})
		return
	}

	// 检查是否正在检测中
	mu.RLock()
	isChecking := checking
	mu.RUnlock()
	if isChecking {
		writeJSON(w, 409, map[string]interface{}{
			"error": "already checking",
		})
		return
	}

	// 记录冷却开始时间
	manualRefreshMu.Lock()
	lastManualRefresh = time.Now()
	manualRefreshMu.Unlock()

	// 异步触发刷新
	go refresh(nil)

	mu.RLock()
	cooldown := cfg.ManualRefreshCooldown
	if cooldown == 0 {
		cooldown = 60
	}
	mu.RUnlock()

	writeJSON(w, 200, map[string]interface{}{
		"ok":       true,
		"cooldown": cooldown,
	})
}

// /api/status-page/:slug — 原版 Uptime Kuma 公开状态页兼容
func apiStatusPage(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/api/status-page/")
	slug = strings.Trim(slug, "/")
	if slug == "" {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}

	mu.RLock()
	cfgSnapshot := cfg
	s := statusV2
	mu.RUnlock()

	// 优先匹配顶级 kuma.slug（"总体状况"）—— 返回全量 Kuma 兼容格式
	if slug == cfgSnapshot.Kuma.Slug {
		groups := make([]KumaCategoryGroup, 0)
		groupID := 1
		monitorID := 1 // 全局递增，与 heartbeat 端点一致
		for _, g := range cfgSnapshot.Groups {
			monitors := make([]KumaCategoryMonitor, 0)
			for _, sg := range g.SubGroups {
				// 在 sections 中查找该子组的状态行
				var rows []GroupResp
				for _, sec := range s.Sections {
					if sec.Name == g.Name {
						for _, row := range sec.Rows {
							if row.SubName == sg.Name {
								rows = append(rows, row)
							}
						}
						break
					}
				}
				for _, m := range sg.Models {
					status := 2
					lat := 0
					msg := "等待检测"
					uptime := 0.0
					for j := range rows {
						if rows[j].Name == m.ID {
							switch rows[j].Status {
							case "up":
								status = 1
								msg = "运行正常"
							case "down":
								status = 0
								msg = "服务异常"
							default:
								status = 2
								msg = "采样中"
							}
							lat = rows[j].LatencyMs
							uptime = rows[j].HealthScore
							break
						}
					}
					monitors = append(monitors, KumaCategoryMonitor{
						ID:      monitorID,
						Name:    sg.Name,
						Type:    "push",
						Status:  status,
						Latency: lat,
						Msg:     msg,
						Uptime:  uptime,
					})
					monitorID++
				}
			}
			if len(monitors) > 0 {
				groups = append(groups, KumaCategoryGroup{
					ID:          groupID,
					Name:        g.Name,
					Weight:      groupID,
					MonitorList: monitors,
				})
				groupID++
			}
		}
		writeJSON(w, 200, map[string]interface{}{
			"config": KumaCategoryConfig{
				Title:       "总体状况",
				Slug:        slug,
				Description: "全量模型状态",
			},
			"publicGroupList": groups,
		})
		return
	}

	// 找到 kumaSlug 匹配的子组
	var groupName, subName string
	var models []Model
	found := false
	for _, g := range cfgSnapshot.Groups {
		for _, sg := range g.SubGroups {
			if sg.KumaSlug == slug {
				groupName = g.Name
				subName = sg.Name
				models = sg.Models
				found = true
				break
			}
		}
		if found {
			break
		}
	}
	if !found {
		writeJSON(w, 404, map[string]string{"error": "category not found: " + slug})
		return
	}

	// 在 statusV2.Sections 里找该子组的行
	var rows []GroupResp
	for _, sec := range s.Sections {
		if sec.Name == groupName {
			for _, row := range sec.Rows {
				if row.SubName == subName {
					rows = append(rows, row)
				}
			}
			break
		}
	}

	// 转换为 Kuma 兼容格式（0=down 1=up 2=pending）
	monitors := make([]KumaCategoryMonitor, 0, len(models))
	for i, m := range models {
		status := 2
		lat := 0
		msg := "等待检测"
		uptime := 0.0
		for j := range rows {
			if rows[j].Name == m.ID {
				switch rows[j].Status {
				case "up":
					status = 1
					msg = "运行正常"
				case "down":
					status = 0
					msg = "服务异常"
				default:
					status = 2
					msg = "采样中"
				}
				lat = rows[j].LatencyMs
				uptime = rows[j].HealthScore
				break
			}
		}
		monitors = append(monitors, KumaCategoryMonitor{
			ID:      i + 1,
			Name:    subName,
			Type:    "push",
			Status:  status,
			Latency: lat,
			Msg:     msg,
			Uptime:  uptime,
		})
	}

	resp := KumaCategoryResp{
		Config: KumaCategoryConfig{
			Title:       subName,
			Slug:        slug,
			Description: groupName,
		},
		PublicGroupList: []KumaCategoryGroup{
			{
				ID:          1,
				Name:        subName,
				Weight:      1,
				MonitorList: monitors,
			},
		},
	}
	writeJSON(w, 200, resp)
}

// /api/status-page/heartbeat/:slug — NewAPI 需要的 heartbeat 轮询端点
func apiStatusPageHeartbeat(w http.ResponseWriter, r *http.Request) {
	slug := strings.TrimPrefix(r.URL.Path, "/api/status-page/heartbeat/")
	slug = strings.Trim(slug, "/")
	if slug == "" {
		writeJSON(w, 400, map[string]string{"error": "slug required"})
		return
	}

	mu.RLock()
	cfgSnapshot := cfg
	mu.RUnlock()

	refs := collectMonitorRefs(cfgSnapshot, slug)
	if refs == nil {
		writeJSON(w, 404, map[string]string{"error": "category not found: " + slug})
		return
	}

	heartbeatList := make(map[string][]HeartbeatPublic)
	uptimeList := make(map[string]float64)

	for _, ref := range refs {
		monitorIDStr := strconv.Itoa(ref.MonitorID)
		hbs := getHeartbeats(ref.SiteName, ref.GroupName, ref.SubName, ref.ModelID, 100)
		if hbs == nil {
			hbs = []HeartbeatPublic{}
		}
		heartbeatList[monitorIDStr] = hbs
		uptimeList[monitorIDStr+"_24"] = calcUptime24h(ref.SiteName, ref.GroupName, ref.SubName, ref.ModelID)
	}

	writeJSON(w, 200, map[string]interface{}{
		"heartbeatList": heartbeatList,
		"uptimeList":    uptimeList,
	})
}

func serveAsset(w http.ResponseWriter, r *http.Request, name string) {
	candidates := []string{name, filepath.Join("/opt/status", name)}
	if exe, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(exe), name))
	}
	for _, p := range candidates {
		st, err := os.Stat(p)
		if err == nil && !st.IsDir() {
			w.Header().Set("cache-control", "no-store")
			http.ServeFile(w, r, p)
			return
		}
	}
	http.NotFound(w, r)
}

func soonestRun(next map[string]time.Time) time.Time {
	var soon time.Time
	for _, at := range next {
		if soon.IsZero() || at.Before(soon) {
			soon = at
		}
	}
	if soon.IsZero() {
		return time.Now().Add(time.Second)
	}
	return soon
}

func dueSiteNames(next map[string]time.Time, now time.Time) []string {
	var names []string
	for name, at := range next {
		if !at.After(now) {
			names = append(names, name)
		}
	}
	return names
}

func syncSiteSchedule(next map[string]time.Time, now time.Time, reset bool) {
	mu.RLock()
	sites := configSites(cfg)
	global := cfg.Interval
	mu.RUnlock()
	seen := map[string]bool{}
	for _, site := range sites {
		seen[site.Name] = true
		if reset {
			next[site.Name] = now.Add(time.Duration(effectiveInterval(site, global)) * time.Second)
			continue
		}
		if _, ok := next[site.Name]; !ok {
			next[site.Name] = now.Add(time.Duration(effectiveInterval(site, global)) * time.Second)
		}
	}
	for name := range next {
		if !seen[name] {
			delete(next, name)
		}
	}
}

var scheduleNotify = make(chan struct{}, 1)

func noteScheduleChange() {
	select {
	case scheduleNotify <- struct{}{}:
	default:
	}
}

func main() {
	loadConfig()
	initDB()
	migrateGroups()
	hydrateStatus()
	startCommunity()

	go func() {
		refresh(nil)
		nextRun := map[string]time.Time{}
		syncSiteSchedule(nextRun, time.Now(), true)
		for {
			timer := time.NewTimer(time.Until(soonestRun(nextRun)))
			select {
			case <-scheduleNotify:
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				syncSiteSchedule(nextRun, time.Now(), true)
			case <-timer.C:
				due := dueSiteNames(nextRun, time.Now())
				if len(due) == 0 {
					syncSiteSchedule(nextRun, time.Now(), false)
					continue
				}
				only := map[string]bool{}
				for _, name := range due {
					only[name] = true
				}
				if !refresh(only) {
					time.Sleep(time.Second)
					continue
				}
				now := time.Now()
				mu.RLock()
				global := cfg.Interval
				sites := configSites(cfg)
				mu.RUnlock()
				byName := map[string]Site{}
				for _, site := range sites {
					byName[site.Name] = site
				}
				for _, name := range due {
					nextRun[name] = now.Add(time.Duration(effectiveInterval(byName[name], global)) * time.Second)
				}
				syncSiteSchedule(nextRun, now, false)
			}
		}
	}()

	http.HandleFunc("/api/status", apiStatus)
	http.HandleFunc("/api/config", apiConfig)
	http.HandleFunc("/api/refresh", apiRefresh)
	http.HandleFunc("/api/status-page/heartbeat/", apiStatusPageHeartbeat)
	http.HandleFunc("/api/status-page/", apiStatusPage)
	if base, err := mountAdmin(http.DefaultServeMux, cfg.AdminPath); err != nil {
		log.Println("admin disabled:", err)
	} else if base == "" {
		log.Println("admin disabled: set adminPath in config.json")
	} else {
		log.Printf("admin at http://127.0.0.1:%d%s", cfg.Port, base)
	}
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		serveAsset(w, r, "status.html")
	})

	addr := fmt.Sprintf(":%d", cfg.Port)
	log.Println("status monitor on", addr)
	log.Fatal(http.ListenAndServe(addr, nil))
}
