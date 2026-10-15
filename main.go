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
	Groups                []Group        `json:"groups"`
}

type TelegramConfig struct {
	BotToken  string   `json:"botToken"`
	ChatID    string   `json:"chatID"`
	ChatIDs   []string `json:"chatIDs,omitempty"`
	Interval  int      `json:"interval"`  // 群推送间隔（分钟）。0 表示状态一变化就推送
	StatusURL string   `json:"statusURL"` // 推送消息底部的监控页地址，留空则使用 zzshu.si，不补协议
	Website   string   `json:"website"`   // 推送消息底部的官网，留空不显示
	Channel   string   `json:"channel"`   // 推送消息底部的通知频道，留空不显示
}

type KumaConfig struct {
	BaseURL string `json:"baseURL"`
	Slug    string `json:"slug"`
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
}

// 按外层group.name分组，返回分组列表和该分组下的所有行
type Section struct {
	Name string      `json:"name"`
	Rows []GroupResp `json:"rows"`
}

type StatusRespV2 struct {
	Title                  string        `json:"title"`
	Overall                OverallStatus `json:"overall"`
	LastCompletedAt        *time.Time    `json:"last_completed_at,omitempty"`
	Checking               bool          `json:"checking"`
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
			groups, err := loadMonitorGroups()
			if err != nil {
				log.Println("load groups:", err)
			} else {
				newCfg.Groups = groups
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
	for i := range c.Groups {
		if c.Groups[i].SubGroups == nil {
			c.Groups[i].SubGroups = []SubGroup{}
		}
		for j := range c.Groups[i].SubGroups {
			if c.Groups[i].SubGroups[j].Models == nil {
				c.Groups[i].SubGroups[j].Models = []Model{}
			}
			for k := range c.Groups[i].SubGroups[j].Models {
				m := &c.Groups[i].SubGroups[j].Models[k]
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

func initDB() {
	var err error
	db, err = sql.Open("sqlite", "status.db")
	if err != nil {
		log.Fatal(err)
	}
	if err := ensureSchema(); err != nil {
		log.Fatal(err)
	}
}

func ensureSchema() error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS checks (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			group_name TEXT NOT NULL,
			sub_name TEXT NOT NULL,
			model_id TEXT NOT NULL,
			up INTEGER NOT NULL,
			latency INTEGER NOT NULL,
			checked_at INTEGER NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_checks_time ON checks(checked_at);
		CREATE INDEX IF NOT EXISTS idx_checks_group ON checks(group_name, sub_name, model_id, checked_at);
		CREATE TABLE IF NOT EXISTS monitor_groups (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL UNIQUE,
			manual_refresh INTEGER NOT NULL DEFAULT 0,
			sort_order INTEGER NOT NULL
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
	return ensureSubgroupRatio()
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

func loadMonitorGroups() ([]Group, error) {
	rows, err := db.Query(`SELECT id, name, manual_refresh FROM monitor_groups ORDER BY sort_order, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var groups []Group
	for rows.Next() {
		var id, manual int
		var name string
		if err := rows.Scan(&id, &name, &manual); err != nil {
			return nil, err
		}
		subs, err := loadMonitorSubgroups(id)
		if err != nil {
			return nil, err
		}
		groups = append(groups, Group{Name: name, ManualRefresh: manual == 1, SubGroups: subs})
	}
	if groups == nil {
		groups = []Group{}
	}
	return groups, rows.Err()
}

func loadMonitorSubgroups(groupID int) ([]SubGroup, error) {
	rows, err := db.Query(`SELECT id, name, kuma_slug, ratio FROM monitor_subgroups WHERE group_id = ? ORDER BY sort_order, id`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var subs []SubGroup
	for rows.Next() {
		var id int
		var name, slug, ratio string
		if err := rows.Scan(&id, &name, &slug, &ratio); err != nil {
			return nil, err
		}
		models, err := loadMonitorModels(id)
		if err != nil {
			return nil, err
		}
		subs = append(subs, SubGroup{Name: name, KumaSlug: slug, Ratio: strings.TrimSpace(ratio), Models: models})
	}
	if subs == nil {
		subs = []SubGroup{}
	}
	return subs, rows.Err()
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

func saveMonitorGroups(groups []Group) error {
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
	for i, g := range groups {
		manual := 0
		if g.ManualRefresh {
			manual = 1
		}
		res, err := tx.Exec(`INSERT INTO monitor_groups (name, manual_refresh, sort_order) VALUES (?, ?, ?)`, g.Name, manual, i)
		if err != nil {
			return err
		}
		gid, err := res.LastInsertId()
		if err != nil {
			return err
		}
		for j, sg := range g.SubGroups {
			res, err = tx.Exec(`INSERT INTO monitor_subgroups (group_id, name, kuma_slug, ratio, sort_order) VALUES (?, ?, ?, ?, ?)`, gid, sg.Name, sg.KumaSlug, strings.TrimSpace(sg.Ratio), j)
			if err != nil {
				return err
			}
			sid, err := res.LastInsertId()
			if err != nil {
				return err
			}
			for k, m := range sg.Models {
				if _, err := tx.Exec(`INSERT INTO monitor_models (subgroup_id, model_id, provider, base_url, api_key, timeout, sort_order) VALUES (?, ?, ?, ?, ?, ?, ?)`, sid, m.ID, m.Provider, m.BaseURL, m.Key, m.Timeout, k); err != nil {
					return err
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
	ready, err := groupsReady()
	if err != nil {
		log.Fatal(err)
	}
	if !ready {
		mu.RLock()
		groups := append([]Group(nil), cfg.Groups...)
		mu.RUnlock()
		if err := saveMonitorGroups(groups); err != nil {
			log.Fatal(err)
		}
		mu.Lock()
		stored := cfg
		stored.Groups = []Group{}
		mu.Unlock()
		if err := writeConfigFile(stored); err != nil {
			log.Println("config strip groups:", err)
		}
	}
	groups, err := loadMonitorGroups()
	if err != nil {
		log.Fatal(err)
	}
	mu.Lock()
	cfg.Groups = groups
	mu.Unlock()
}

func saveCheck(groupName, subName, modelID string, up bool, latency int) {
	v := 0
	if up {
		v = 1
	}
	_, err := db.Exec(
		"INSERT INTO checks (group_name, sub_name, model_id, up, latency, checked_at) VALUES (?, ?, ?, ?, ?, ?)",
		groupName, subName, modelID, v, latency, time.Now().Unix(),
	)
	if err != nil {
		log.Println("db insert error:", err)
	}
}

func getHistory(groupName, subName, modelID string, limit int) []HistoryItem {
	rows, err := db.Query(
		"SELECT up, latency, checked_at FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT ?",
		groupName, subName, modelID, limit,
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
	up  bool
	lat int
	ts  int64
}

func loadProbePoints(groupName, subName, modelID string, since int64) []probePoint {
	if db == nil {
		return nil
	}
	rows, err := db.Query(
		`SELECT up, latency, checked_at FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? AND checked_at >= ? ORDER BY checked_at`,
		groupName, subName, modelID, since,
	)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var points []probePoint
	for rows.Next() {
		var up, lat int
		var ts int64
		if rows.Scan(&up, &lat, &ts) != nil {
			continue
		}
		points = append(points, probePoint{up: up == 1, lat: lat, ts: ts})
	}
	return points
}

func buildTimelines(groupName, subName, modelID string, now time.Time) map[string][]TimelineBucket {
	since := now.Add(-84 * 24 * time.Hour).Unix()
	points := loadProbePoints(groupName, subName, modelID, since)
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

func calcHealthScore(groupName, subName, modelID string) float64 {
	rows, err := db.Query(
		"SELECT up FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT 100",
		groupName, subName, modelID,
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

func getLastCheck(groupName, subName, modelID string) (bool, int, time.Time) {
	var up int
	var lat int
	var ts int64
	err := db.QueryRow(
		"SELECT up, latency, checked_at FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT 1",
		groupName, subName, modelID,
	).Scan(&up, &lat, &ts)
	if err != nil {
		return false, 0, time.Time{}
	}
	return up == 1, lat, time.Unix(ts, 0)
}

// 最近一小时里只要有成功，偶发失败只算降级。连续失败满一小时才算异常。
func classifyProbeStatus(groupName, subName, modelID string, now time.Time) (string, string) {
	if db == nil {
		return "unknown", "无采样"
	}
	cutoff := now.Add(-time.Hour).Unix()
	rows, err := db.Query(
		`SELECT up, checked_at FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? AND checked_at >= ? ORDER BY checked_at`,
		groupName, subName, modelID, cutoff,
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
			`SELECT up, checked_at FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? AND checked_at < ? ORDER BY checked_at DESC LIMIT 1`,
			groupName, subName, modelID, cutoff,
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

func refreshRowFromHistory(group string, row *GroupResp, now time.Time) {
	statusStr, label := classifyProbeStatus(group, row.SubName, row.Name, now)
	_, lat, lastAt := getLastCheck(group, row.SubName, row.Name)
	row.Status = statusStr
	row.Label = label
	row.HealthScore = calcHealthScore(group, row.SubName, row.Name)
	row.LatencyMs = lat
	row.History = getHistory(group, row.SubName, row.Name, 120)
	row.Timelines = buildTimelines(group, row.SubName, row.Name, now)
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
			refreshRowFromHistory(sections[i].Name, &sections[i].Rows[j], now)
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

func checkOpenAICompat(m Model) (bool, int) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0
	}
	body, err := probeBody("openai-compat", m.ID)
	if err != nil {
		return false, 0
	}
	code, resp, lat := doPost(base+"/chat/completions", m.Key, body, m.Timeout)
	return code > 0 && code < 300 && openaiCompatHasReply(resp), lat
}

func checkOpenAIResponse(m Model) (bool, int) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0
	}
	body, err := probeBody("openai-response", m.ID)
	if err != nil {
		return false, 0
	}
	code, resp, lat := doPost(base+"/responses", m.Key, body, m.Timeout)
	return code > 0 && code < 300 && openaiResponseHasReply(resp), lat
}

func checkGoogle(m Model) (bool, int) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0
	}
	url := fmt.Sprintf("%s/models/%s:generateContent?key=%s", base, m.ID, m.Key)
	body, err := probeBody("google", m.ID)
	if err != nil {
		return false, 0
	}
	code, resp, lat := doPost(url, "", body, m.Timeout)
	return code > 0 && code < 300 && googleHasReply(resp), lat
}

func readyBase(m Model) (string, bool) {
	base, err := normalizeBaseURL(m.Provider, m.BaseURL)
	return base, err == nil
}

func checkAnthropic(m Model) (bool, int) {
	base, ok := readyBase(m)
	if !ok {
		return false, 0
	}
	body, err := probeBody("anthropic", m.ID)
	if err != nil {
		return false, 0
	}
	req, err := http.NewRequest("POST", base+"/messages", bytes.NewReader(body))
	if err != nil {
		return false, 0
	}
	req.Header.Set("x-api-key", m.Key)
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("content-type", "application/json")
	code, resp, lat := doReq(req, m.Timeout)
	return code > 0 && code < 300 && anthropicHasReply(resp), lat
}

func doPost(url, key string, body []byte, timeout int) (int, []byte, int) {
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return 0, nil, 0
	}
	req.Header.Set("content-type", "application/json")
	if key != "" {
		req.Header.Set("authorization", "Bearer "+key)
	}
	return doReq(req, timeout)
}

func doReq(req *http.Request, timeout int) (int, []byte, int) {
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
		return 0, nil, lat
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, lat
	}
	return resp.StatusCode, b, lat
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

func checkOne(m Model) (bool, int) {
	switch m.Provider {
	case "openai-compat":
		return checkOpenAICompat(m)
	case "openai-response":
		return checkOpenAIResponse(m)
	case "google":
		return checkGoogle(m)
	case "anthropic":
		return checkAnthropic(m)
	default:
		return checkOpenAICompat(m)
	}
}

func providerFamily(p string) string {
	switch p {
	case "openai-compat", "openai-response":
		return "openai"
	case "anthropic":
		return "claude"
	case "google":
		return "gemini"
	default:
		return "other"
	}
}

func snapshotRow(gName string, sg SubGroup, m Model, lat int, lastAt, now time.Time) GroupResp {
	statusStr, label := classifyProbeStatus(gName, sg.Name, m.ID, now)
	row := GroupResp{
		Name:        m.ID,
		SubName:     sg.Name,
		Ratio:       strings.TrimSpace(sg.Ratio),
		Family:      providerFamily(m.Provider),
		Status:      statusStr,
		Label:       label,
		HealthScore: calcHealthScore(gName, sg.Name, m.ID),
		LatencyMs:   lat,
		History:     getHistory(gName, sg.Name, m.ID, 120),
		Timelines:   buildTimelines(gName, sg.Name, m.ID, now),
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
	groups := append([]Group(nil), cfg.Groups...)
	title := cfg.Frontend.Title
	mu.RUnlock()
	now := time.Now()
	sections := make([]Section, 0, len(groups))
	for _, g := range groups {
		var rows []GroupResp
		for _, sg := range g.SubGroups {
			for _, m := range sg.Models {
				_, lat, lastAt := getLastCheck(g.Name, sg.Name, m.ID)
				rows = append(rows, snapshotRow(g.Name, sg, m, lat, lastAt, now))
			}
		}
		if len(rows) > 0 {
			sections = append(sections, Section{Name: g.Name, Rows: rows})
		}
	}
	sections = applyConfigOrder(sections, groups)
	overallStatus, overallLabel := overallFromRows(sections)
	mu.Lock()
	statusV2 = StatusRespV2{
		Title:    title,
		Overall:  OverallStatus{Status: overallStatus, Label: overallLabel},
		Checking: false,
		Sections: sections,
	}
	mu.Unlock()
}

// updateRow 检测完一个模型后立即更新共享状态，前端轮询可见
func updateRow(gName string, sg SubGroup, m Model, sections *[]Section, up bool, lat int) {
	_ = up
	_, _, lastAt := getLastCheck(gName, sg.Name, m.ID)
	row := snapshotRow(gName, sg, m, lat, lastAt, time.Now())

	mu.Lock()
	ensureFreshHistory(*sections)
	*sections = upsertSectionRow(*sections, gName, row)
	*sections = applyConfigOrder(*sections, cfg.Groups)
	applyLiveRatios(*sections)

	overallStatus, overallLabel := overallFromRows(*sections)

	statusV2 = StatusRespV2{
		Title:    cfg.Frontend.Title,
		Overall:  OverallStatus{Status: overallStatus, Label: overallLabel},
		Checking: true,
		Sections: *sections,
	}
	mu.Unlock()
}

// upsertSectionRow 在 sections 中插入或更新一行，用于逐个检测时实时更新前端
func upsertSectionRow(sections []Section, gName string, row GroupResp) []Section {
	idx := -1
	for i, sec := range sections {
		if sec.Name == gName {
			idx = i
			break
		}
	}
	if idx == -1 {
		return append(sections, Section{Name: gName, Rows: []GroupResp{row}})
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
func applyConfigOrder(sections []Section, groups []Group) []Section {
	groupIndex := make(map[string]int, len(groups))
	type rank struct{ sub, model int }
	rowIndex := make(map[string]rank)
	for i, g := range groups {
		groupIndex[g.Name] = i
		for j, sg := range g.SubGroups {
			for k, m := range sg.Models {
				rowIndex[g.Name+"\x1f"+sg.Name+"\x1f"+m.ID] = rank{j, k}
			}
		}
	}
	sort.SliceStable(sections, func(a, b int) bool {
		ia, oka := groupIndex[sections[a].Name]
		ib, okb := groupIndex[sections[b].Name]
		if !oka {
			ia = len(groups)
		}
		if !okb {
			ib = len(groups)
		}
		return ia < ib
	})
	for i := range sections {
		rows := sections[i].Rows
		gName := sections[i].Name
		sort.SliceStable(rows, func(a, b int) bool {
			ka, oka := rowIndex[gName+"\x1f"+rows[a].SubName+"\x1f"+rows[a].Name]
			kb, okb := rowIndex[gName+"\x1f"+rows[b].SubName+"\x1f"+rows[b].Name]
			if !oka {
				ka = rank{len(groups) + 1, len(groups) + 1}
			}
			if !okb {
				kb = rank{len(groups) + 1, len(groups) + 1}
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
	for _, g := range cfg.Groups {
		for _, sg := range g.SubGroups {
			ratios[g.Name+"\x1f"+sg.Name] = strings.TrimSpace(sg.Ratio)
		}
	}
	for i := range sections {
		for j := range sections[i].Rows {
			key := sections[i].Name + "\x1f" + sections[i].Rows[j].SubName
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

func renameLiveSubgroup(group, oldName, newName string) {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == "" || oldName == newName {
		return
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range statusV2.Sections {
		if statusV2.Sections[i].Name != group {
			continue
		}
		for j := range statusV2.Sections[i].Rows {
			if statusV2.Sections[i].Rows[j].SubName == oldName {
				statusV2.Sections[i].Rows[j].SubName = newName
			}
		}
	}
}

func reorderSections(names []string) {
	index := make(map[string]int, len(names))
	for i, name := range names {
		index[strings.TrimSpace(name)] = i
	}
	mu.Lock()
	defer mu.Unlock()
	sections := statusV2.Sections
	sort.SliceStable(sections, func(a, b int) bool {
		ia, oka := index[sections[a].Name]
		ib, okb := index[sections[b].Name]
		if !oka {
			ia = len(names)
		}
		if !okb {
			ib = len(names)
		}
		return ia < ib
	})
	statusV2.Sections = sections
}

func reorderSectionRows(group string, names []string) {
	index := make(map[string]int, len(names))
	for i, name := range names {
		index[strings.TrimSpace(name)] = i
	}
	mu.Lock()
	defer mu.Unlock()
	for i := range statusV2.Sections {
		if statusV2.Sections[i].Name != group {
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
}

// pruneStaleSections 根据 config 当前条目移除 sections 中已不存在的 group/subGroup/model 行
func pruneStaleSections(sections []Section, cfg Config) []Section {
	// 建立 config 中有效条目的集合: "groupName|subName|modelID"
	valid := make(map[string]bool)
	for _, g := range cfg.Groups {
		for _, sg := range g.SubGroups {
			for _, m := range sg.Models {
				valid[g.Name+"|"+sg.Name+"|"+m.ID] = true
			}
		}
	}

	var result []Section
	for _, sec := range sections {
		var keptRows []GroupResp
		for _, row := range sec.Rows {
			key := sec.Name + "|" + row.SubName + "|" + row.Name
			if valid[key] {
				keptRows = append(keptRows, row)
			}
		}
		if len(keptRows) > 0 {
			result = append(result, Section{Name: sec.Name, Rows: keptRows})
		}
	}
	return result
}

func refresh() {
	// 防止并发执行：如果已有一次检测正在运行，则跳过
	mu.Lock()
	if checking {
		mu.Unlock()
		return
	}
	checking = true
	mu.Unlock()

	loadConfig()

	prevRows := map[string]string{}
	mu.RLock()
	prevOverall := statusV2.Overall.Status
	for _, sec := range statusV2.Sections {
		for _, r := range sec.Rows {
			prevRows[sec.Name+"\x1f"+r.SubName+"\x1f"+r.Name] = r.Status
		}
	}
	mu.RUnlock()

	// 以当前已有状态为起点，逐个检测并实时更新
	mu.RLock()
	sections := statusV2.Sections
	if sections == nil {
		sections = []Section{}
	}
	mu.RUnlock()

	// 根据 config 清理已不存在的条目，并立刻按后台顺序发布
	sections = pruneStaleSections(sections, cfg)
	mu.Lock()
	ensureFreshHistory(sections)
	sections = applyConfigOrder(sections, cfg.Groups)
	applyLiveRatios(sections)
	statusV2.Sections = sections
	mu.Unlock()

	now := time.Now()
	totalLat := 0
	latCount := 0

	// failRecords 记录首次失败的模型，等全部测完再统一重试
	type failRecord struct {
		gName, sgName, ratio string
		m                    Model
		lat                  int
	}
	var failRecords []failRecord

	// 逐个检测所有模型，失败的暂不记录，先收集
	for _, g := range cfg.Groups {
		for _, sg := range g.SubGroups {
			for _, m := range sg.Models {
				up, lat := checkOne(m)
				if up {
					saveCheck(g.Name, sg.Name, m.ID, true, lat)
					totalLat += lat
					latCount++
					updateRow(g.Name, sg, m, &sections, true, lat)
				} else {
					failRecords = append(failRecords, failRecord{gName: g.Name, sgName: sg.Name, ratio: sg.Ratio, m: m, lat: lat})
				}
			}
		}
	}

	// 等待5秒后重试所有失败的模型
	if len(failRecords) > 0 {
		time.Sleep(5 * time.Second)
		for _, fr := range failRecords {
			up, lat := checkOne(fr.m)
			saveCheck(fr.gName, fr.sgName, fr.m.ID, up, lat)
			if up {
				totalLat += lat
				latCount++
			}
			updateRow(fr.gName, SubGroup{Name: fr.sgName, Ratio: fr.ratio}, fr.m, &sections, up, lat)
		}
	}

	// 全部检测完毕，标记 checking = false
	mu.Lock()
	ensureFreshHistory(sections)
	sections = applyConfigOrder(sections, cfg.Groups)
	applyLiveRatios(sections)
	overallStatus, overallLabel := overallFromRows(sections)
	statusV2 = StatusRespV2{
		Title:           cfg.Frontend.Title,
		Overall:         OverallStatus{Status: overallStatus, Label: overallLabel},
		LastCompletedAt: &now,
		Checking:        false,
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
		for _, g := range cfg.Groups {
			for _, sg := range g.SubGroups {
				if sg.KumaSlug != "" {
					var sgLatSum, sgLatCount int
					sgUp := len(sg.Models) > 0
					for _, m := range sg.Models {
						found := false
						for _, sec := range sections {
							if sec.Name != g.Name {
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
						if _, lat, at := getLastCheck(g.Name, sg.Name, m.ID); !at.IsZero() {
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

	mu.RLock()
	tgToken := cfg.Telegram.BotToken
	tgChats := append([]string(nil), cfg.Telegram.ChatIDs...)
	tgEvery := time.Duration(cfg.Telegram.Interval) * time.Minute
	tgTitle := cfg.Frontend.Title
	tgStatus, tgWebsite, tgChannel := cfg.Telegram.StatusURL, cfg.Telegram.Website, cfg.Telegram.Channel
	mu.RUnlock()
	// 启动后的第一轮，以及清空记录后的第一轮，只当基线，不推送。
	nowPush := time.Now()
	historyNow := atomic.LoadUint64(&historyEpoch)
	if !telegramBaselined || historyNow != atomic.LoadUint64(&telegramSyncedEpoch) {
		telegramBaselined = true
		atomic.StoreUint64(&telegramSyncedEpoch, historyNow)
		resetTelegramQueue()
		markTelegramPushed(nowPush)
	} else if takeTelegram(prevRows, prevOverall, sections, overallStatus, overallLabel, tgEvery, nowPush) != "" {
		texts := splitTelegram(formatStatusBoard(tgTitle, sections, nowPush, tgStatus, tgWebsite, tgChannel))
		if err := sendTelegramAll(tgToken, tgChats, texts); err != nil {
			log.Println("telegram:", err)
		}
	}

	cleanupOld()
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
			old, ok := prev[sec.Name+"\x1f"+r.SubName+"\x1f"+r.Name]
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
func takeTelegram(prev map[string]string, prevOverall string, sections []Section, overall, overallLabel string, every time.Duration, now time.Time) string {
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
		return ""
	}
	if every > 0 && !tgQueue.last.IsZero() && now.Sub(tgQueue.last) < every {
		return ""
	}
	pending := make([]tgItem, 0, len(tgQueue.items))
	for _, it := range tgQueue.items {
		pending = append(pending, it)
	}
	text := formatTelegram(pending, tgQueue.overallFrom, tgQueue.overallTo, tgQueue.label)
	tgQueue.items = map[string]tgItem{}
	tgQueue.overallFrom, tgQueue.overallTo, tgQueue.label = "", "", ""
	tgQueue.last = now
	return text
}

func formatStatusBoard(title string, sections []Section, now time.Time, statusURL, website, channel string) string {
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
	if footer := boardFooter(statusURL, website, channel); footer != "" {
		b.WriteString("\n\n")
		b.WriteString(footer)
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

func boardFooter(statusURL, website, channel string) string {
	cleaned, err := cleanStatusURL(statusURL)
	if err != nil || cleaned == "" {
		cleaned = "zzshu.si"
	}
	statusURL = cleaned
	website = strings.TrimSpace(website)
	channel = strings.TrimSpace(channel)
	parts := []string{"监控 " + statusURL}
	if website != "" && !sameBoardLink(website, statusURL) {
		parts = append(parts, "官网 "+website)
	}
	if channel != "" {
		parts = append(parts, "通知频道 "+channel)
	}
	return strings.Join(parts, "\n")
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
	telegramClient = &http.Client{Timeout: 12 * time.Second}
)

func sendTelegramAll(token string, chats []string, texts []string) error {
	var failed []string
	for _, chat := range chats {
		for _, text := range texts {
			if err := sendTelegram(token, chat, text); err != nil {
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

func sendTelegram(token, chatID, text string) error {
	token = strings.TrimSpace(token)
	chatID = strings.TrimSpace(chatID)
	text = strings.TrimSpace(text)
	if token == "" || chatID == "" || text == "" {
		return nil
	}
	payload, err := json.Marshal(map[string]any{
		"chat_id":                  chatID,
		"text":                     text,
		"disable_web_page_preview": true,
	})
	if err != nil {
		return errors.New("无法编码 Telegram 消息")
	}
	req, err := http.NewRequest(http.MethodPost, telegramAPI+"/bot"+token+"/sendMessage", bytes.NewReader(payload))
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
func getHeartbeats(groupName, subName, modelID string, limit int) []HeartbeatPublic {
	rows, err := db.Query(
		"SELECT up, latency, checked_at FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? ORDER BY checked_at DESC LIMIT ?",
		groupName, subName, modelID, limit,
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
func calcUptime24h(groupName, subName, modelID string) float64 {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	var total, upCount int
	rows, err := db.Query(
		"SELECT up FROM checks WHERE group_name = ? AND sub_name = ? AND model_id = ? AND checked_at >= ?",
		groupName, subName, modelID, cutoff,
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
		for _, g := range cfg.Groups {
			for _, sg := range g.SubGroups {
				for _, m := range sg.Models {
					refs = append(refs, monitorRef{g.Name, sg.Name, m.ID, id})
					id++
				}
			}
		}
		return refs
	}

	// 查找子组 slug
	for _, g := range cfg.Groups {
		for _, sg := range g.SubGroups {
			if sg.KumaSlug == slug {
				for i, m := range sg.Models {
					refs = append(refs, monitorRef{g.Name, sg.Name, m.ID, i + 1})
				}
				return refs
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
	groupSettings := make([]map[string]interface{}, 0, len(cfg.Groups))
	for _, g := range cfg.Groups {
		groupSettings = append(groupSettings, map[string]interface{}{
			"name":          g.Name,
			"manualRefresh": g.ManualRefresh,
		})
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
	go refresh()

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
		hbs := getHeartbeats(ref.GroupName, ref.SubName, ref.ModelID, 100)
		if hbs == nil {
			hbs = []HeartbeatPublic{}
		}
		heartbeatList[monitorIDStr] = hbs
		uptimeList[monitorIDStr+"_24"] = calcUptime24h(ref.GroupName, ref.SubName, ref.ModelID)
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

func main() {
	loadConfig()
	initDB()
	migrateGroups()
	hydrateStatus()

	go func() {
		refresh()
		for {
			mu.RLock()
			sec := cfg.Interval
			mu.RUnlock()
			time.Sleep(time.Duration(sec) * time.Second)
			refresh()
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
