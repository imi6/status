package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

var shanghai = time.FixedZone("UTC+8", 8*3600)

type CommunityConfig struct {
	Enabled    bool   `json:"enabled"`
	ChatID     string `json:"chatID"`
	RatePoints int    `json:"ratePoints"`
	RateYuan   string `json:"rateYuan"`
	RateCents  int    `json:"rateCents"`
	MinPoints  int    `json:"minPoints"`
	SpeechNeed int    `json:"speechNeed"`
	Winners    int    `json:"winners"`
	CleanLines bool   `json:"cleanLines"`
}

func defaultCommunityConfig() CommunityConfig {
	return CommunityConfig{
		RatePoints: 10, RateYuan: "1", RateCents: 100, MinPoints: 10,
		SpeechNeed: 5, Winners: 1, CleanLines: true,
	}
}

func ensureCommunityTables() error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS reward_codes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			code TEXT NOT NULL UNIQUE,
			tier TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'available',
			used_by TEXT NOT NULL DEFAULT '',
			used_name TEXT NOT NULL DEFAULT '',
			activity TEXT NOT NULL DEFAULT '',
			used_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS reward_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			activity TEXT NOT NULL,
			tier TEXT NOT NULL,
			user_id INTEGER NOT NULL,
			user_name TEXT NOT NULL,
			text TEXT NOT NULL,
			created_at INTEGER NOT NULL,
			code_id INTEGER NOT NULL DEFAULT 0,
			points INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS point_accounts (
			user_id INTEGER PRIMARY KEY,
			user_name TEXT NOT NULL DEFAULT '',
			balance INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS community_state (
			k TEXT PRIMARY KEY,
			v TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS community_members (
			user_id INTEGER PRIMARY KEY,
			user_name TEXT NOT NULL,
			joined_at INTEGER NOT NULL
		);
		CREATE TABLE IF NOT EXISTS daily_speech (
			day TEXT NOT NULL,
			user_id INTEGER NOT NULL,
			user_name TEXT NOT NULL,
			messages INTEGER NOT NULL DEFAULT 0,
			rewarded INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (day, user_id)
		);
		CREATE TABLE IF NOT EXISTS daily_speech_lines (
			day TEXT NOT NULL,
			user_id INTEGER NOT NULL,
			body TEXT NOT NULL,
			PRIMARY KEY (day, user_id, body)
		);
		CREATE TABLE IF NOT EXISTS lottery_prizes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			code TEXT NOT NULL UNIQUE,
			status TEXT NOT NULL DEFAULT 'available',
			winner_id INTEGER NOT NULL DEFAULT 0,
			winner_name TEXT NOT NULL DEFAULT '',
			day TEXT NOT NULL DEFAULT '',
			used_at INTEGER NOT NULL DEFAULT 0
		);
		CREATE TABLE IF NOT EXISTS daily_settle (
			day TEXT PRIMARY KEY,
			participants INTEGER NOT NULL,
			rewarded INTEGER NOT NULL,
			settled_at INTEGER NOT NULL
		);
	`)
	if err != nil {
		return err
	}
	_, _ = db.Exec(`ALTER TABLE reward_logs ADD COLUMN points INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE reward_codes ADD COLUMN amount_cents INTEGER NOT NULL DEFAULT 0`)
	_, _ = db.Exec(`ALTER TABLE daily_speech_lines ADD COLUMN created_at INTEGER NOT NULL DEFAULT 0`)
	return nil
}

func loadCommunityConfig() CommunityConfig {
	cfg := defaultCommunityConfig()
	var raw string
	err := db.QueryRow(`SELECT v FROM community_state WHERE k = ?`, "config").Scan(&raw)
	if err != nil {
		return cfg
	}
	_ = json.Unmarshal([]byte(raw), &cfg)
	def := defaultCommunityConfig()
	if cfg.RatePoints <= 0 {
		cfg.RatePoints = def.RatePoints
	}
	if cfg.RateCents <= 0 {
		cfg.RateCents = def.RateCents
	}
	if strings.TrimSpace(cfg.RateYuan) == "" {
		cfg.RateYuan = formatYuan(cfg.RateCents)
	}
	if cfg.MinPoints <= 0 {
		cfg.MinPoints = def.MinPoints
	}
	if cfg.SpeechNeed <= 0 {
		cfg.SpeechNeed = def.SpeechNeed
	}
	if cfg.Winners <= 0 {
		cfg.Winners = def.Winners
	}
	return cfg
}

func saveCommunityConfig(c CommunityConfig) error {
	if c.RatePoints < 1 || c.RatePoints > 10000 {
		return errors.New("兑换积分需在 1 到 10000")
	}
	if c.MinPoints < 1 || c.MinPoints > 10000 {
		return errors.New("最低兑换积分需在 1 到 10000")
	}
	cents, err := parseYuan(c.RateYuan)
	if err != nil {
		return errors.New("兑换额度需在 0.01 到 10000 元")
	}
	c.RateCents = cents
	c.RateYuan = formatYuan(cents)
	probe := c
	if costForCents(cents, probe) < c.MinPoints {
		return errors.New("按这个比例，这份额度不到最低兑换积分")
	}
	if c.SpeechNeed < 1 || c.SpeechNeed > 1000 {
		return errors.New("每日发言条数需在 1 到 1000")
	}
	if c.Winners < 1 || c.Winners > 1000 {
		return errors.New("每天中奖人数需在 1 到 1000")
	}
	c.ChatID = strings.TrimSpace(c.ChatID)
	raw, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT INTO community_state (k, v) VALUES ('config', ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, string(raw))
	return err
}

func communityChatID(c CommunityConfig) string {
	if strings.TrimSpace(c.ChatID) != "" {
		return strings.TrimSpace(c.ChatID)
	}
	mu.RLock()
	defer mu.RUnlock()
	if len(cfg.Telegram.ChatIDs) > 0 {
		return cfg.Telegram.ChatIDs[0]
	}
	return strings.TrimSpace(cfg.Telegram.ChatID)
}

type communityMessage struct {
	ChatID         int64
	UserID         int64
	UserName       string
	Text           string
	MessageID      int
	ReplyUserID    int64
	ReplyUserName  string
	ReplyText      string
	ReplyMessageID int
}

func (m communityMessage) displayName() string {
	if strings.TrimSpace(m.UserName) != "" {
		return "@" + strings.TrimPrefix(m.UserName, "@")
	}
	return strconv.FormatInt(m.UserID, 10)
}

var communityMu sync.Mutex

func applyCommunity(msg communityMessage, now time.Time) (string, string) {
	if db == nil {
		return "", ""
	}
	name, rest := splitCommand(msg.Text)
	if msg.ChatID == msg.UserID {
		return applyPrivate(msg, name, rest, now)
	}
	conf := loadCommunityConfig()
	if !conf.Enabled {
		return "", ""
	}
	want := communityChatID(conf)
	if want == "" || strconv.FormatInt(msg.ChatID, 10) != want {
		return "", ""
	}
	switch name {
	case "help", "start":
		return communityHelp(), ""
	case "points":
		return pointsReply(msg, conf), ""
	case "redeem":
		return "兑换只能私聊机器人进行。请先私聊机器人发送 /start，再发送 /redeem 1。", ""
	case "code":
		return resendOwnCode(msg)
	case "join":
		return joinSpeech(msg, now)
	case "leave":
		return leaveSpeech(msg)
	case "pool":
		return poolReply(msg, now), ""
	default:
		return "", ""
	}
}

func applyPrivate(msg communityMessage, name, rest string, now time.Time) (string, string) {
	conf := loadCommunityConfig()
	switch name {
	case "start", "help":
		return communityHelp(), ""
	case "points":
		return pointsReply(msg, conf), ""
	case "redeem":
		if !conf.Enabled {
			return "群活动还没开启。", ""
		}
		return redeemPoints(msg, rest, conf, now)
	case "code":
		return resendOwnCode(msg)
	case "join":
		if !conf.Enabled {
			return "群活动还没开启。", ""
		}
		return joinSpeech(msg, now)
	case "leave":
		return leaveSpeech(msg)
	case "pool":
		return poolReply(msg, now), ""
	default:
		return "发送 /join 参加每日抽奖。积分兑换请发送 /redeem 1。", ""
	}
}

func communityHelp() string {
	conf := loadCommunityConfig()
	return strings.Join([]string{
		"每日抽奖要先 /join，没报名的发言不计数。报名一次长期有效。",
		fmt.Sprintf("每天发言满 %d 句进入当天抽奖池，次日随机抽 %d 人，私信发奖。", conf.SpeechNeed, conf.Winners),
		"请先私聊机器人 /start，否则兑换码发不出去。",
		"/join   报名参加",
		"/leave   退出，之后不再计数",
		"/pool   查看今天奖池和奖品",
		"/points   查看积分",
		"/redeem 1   只能私聊用积分兑换",
		"/code   没收到兑换码时再要一次",
	}, "\n")
}

func splitCommand(text string) (string, string) {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return "", text
	}
	parts := strings.SplitN(text, " ", 2)
	name := strings.ToLower(strings.TrimPrefix(parts[0], "/"))
	if i := strings.IndexByte(name, '@'); i >= 0 {
		name = name[:i]
	}
	rest := ""
	if len(parts) == 2 {
		rest = strings.TrimSpace(parts[1])
	}
	return name, rest
}

func redeemPoints(msg communityMessage, rest string, conf CommunityConfig, now time.Time) (string, string) {
	if strings.TrimSpace(rest) == "" {
		return redeemMenu(conf), ""
	}
	cents, err := parseYuan(rest)
	if err != nil {
		return "格式：/redeem 1。发送 /redeem 可以查看现有额度和所需积分。", ""
	}
	cost := costForCents(cents, conf)
	if cost < conf.MinPoints {
		return fmt.Sprintf("%s元需要 %d 积分，低于最低 %d 积分，不能兑换。", formatYuan(cents), cost, conf.MinPoints), ""
	}
	label := formatYuan(cents) + "元"
	communityMu.Lock()
	defer communityMu.Unlock()
	res, err := db.Exec(`UPDATE point_accounts SET balance = balance - ?, user_name = ? WHERE user_id = ? AND balance >= ?`,
		cost, msg.displayName(), msg.UserID, cost)
	if err != nil {
		return "兑换暂时失败，请稍后再试。", ""
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Sprintf("积分不够。当前 %d，%s需要 %d。最低 %d 积分起兑。", pointBalance(msg.UserID), label, cost, conf.MinPoints), ""
	}
	code, codeID, err := takeCode(cents)
	if err != nil {
		_ = addPoints(msg.UserID, msg.displayName(), cost)
		return "这个额度的兑换码暂时没有了，积分已退回。发送 /redeem 可查看现有额度。", ""
	}
	mark, err := db.Exec(`UPDATE reward_codes SET status = 'used', used_by = ?, used_name = ?, activity = 'redeem', used_at = ? WHERE id = ? AND status = 'available'`,
		strconv.FormatInt(msg.UserID, 10), msg.displayName(), now.Unix(), codeID)
	if err != nil || rowsOf(mark) == 0 {
		_ = addPoints(msg.UserID, msg.displayName(), cost)
		return "这个额度的兑换码暂时没有了，积分已退回。", ""
	}
	_, _ = db.Exec(`INSERT INTO reward_logs (activity, tier, user_id, user_name, text, created_at, code_id, points) VALUES ('redeem', ?, ?, ?, ?, ?, ?, ?)`,
		label, msg.UserID, msg.displayName(), "/redeem "+label, now.Unix(), codeID, -cost)
	notifyGroup(fmt.Sprintf("%s 用 %d 积分兑换了 %s CDK。", msg.displayName(), cost, label))
	return fmt.Sprintf("已用 %d 积分兑换 %s CDK，兑换码在下一条消息。当前积分 %d。", cost, label, pointBalance(msg.UserID)), code
}

func rowsOf(res interface{ RowsAffected() (int64, error) }) int64 {
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
}

func redeemMenu(conf CommunityConfig) string {
	lines := []string{rateText(conf)}
	items := codeStock()
	if len(items) == 0 {
		lines = append(lines, "当前没有可兑换的 CDK。")
	} else {
		lines = append(lines, "可兑换：")
		for _, item := range items {
			lines = append(lines, fmt.Sprintf("%s元  %d积分  剩余%d", item.Yuan, item.Points, item.Count))
		}
	}
	lines = append(lines, "例如发送 /redeem 1")
	return strings.Join(lines, "\n")
}

func pointsReply(msg communityMessage, conf CommunityConfig) string {
	return fmt.Sprintf("%s 当前积分 %d。\n%s\n发送 /redeem 查看可换的额度。",
		msg.displayName(), pointBalance(msg.UserID), rateText(conf))
}

func rateText(conf CommunityConfig) string {
	return fmt.Sprintf("兑换比例：%d 积分 = %s 元。最低 %d 积分起兑。", conf.RatePoints, formatYuan(conf.RateCents), conf.MinPoints)
}

func costForCents(cents int, conf CommunityConfig) int {
	if cents <= 0 || conf.RatePoints <= 0 || conf.RateCents <= 0 {
		return 0
	}
	return (cents*conf.RatePoints + conf.RateCents - 1) / conf.RateCents
}

func parseYuan(raw string) (int, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimSuffix(s, "元")
	s = strings.TrimSuffix(strings.ToLower(s), "r")
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		return 0, errors.New("额度格式不正确")
	}
	parts := strings.Split(s, ".")
	if len(parts) > 2 || parts[0] == "" {
		return 0, errors.New("额度格式不正确")
	}
	yuan, err := strconv.Atoi(parts[0])
	if err != nil || yuan < 0 || yuan > 10000 {
		return 0, errors.New("额度格式不正确")
	}
	frac := 0
	if len(parts) == 2 {
		digits := parts[1]
		if digits == "" || len(digits) > 2 {
			return 0, errors.New("额度最多两位小数")
		}
		if len(digits) == 1 {
			digits += "0"
		}
		frac, err = strconv.Atoi(digits)
		if err != nil {
			return 0, errors.New("额度格式不正确")
		}
	}
	total := yuan*100 + frac
	if total <= 0 || total > 10000*100 {
		return 0, errors.New("额度需在 0.01 到 10000 元")
	}
	return total, nil
}

func formatYuan(cents int) string {
	if cents < 0 {
		cents = 0
	}
	yuan := cents / 100
	frac := cents % 100
	if frac == 0 {
		return strconv.Itoa(yuan)
	}
	if frac%10 == 0 {
		return fmt.Sprintf("%d.%d", yuan, frac/10)
	}
	return fmt.Sprintf("%d.%02d", yuan, frac)
}

func addPoints(userID int64, name string, delta int) error {
	_, err := db.Exec(`INSERT INTO point_accounts (user_id, user_name, balance) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET balance = balance + excluded.balance, user_name = excluded.user_name`,
		userID, name, delta)
	return err
}

func pointBalance(userID int64) int {
	var n int
	_ = db.QueryRow(`SELECT balance FROM point_accounts WHERE user_id = ?`, userID).Scan(&n)
	return n
}

func takeCode(cents int) (string, int64, error) {
	var id int64
	var code string
	err := db.QueryRow(`SELECT id, code FROM reward_codes WHERE amount_cents = ? AND status = 'available' ORDER BY id LIMIT 1`, cents).Scan(&id, &code)
	if err != nil {
		return "", 0, err
	}
	return code, id, nil
}

func resendOwnCode(msg communityMessage) (string, string) {
	var code string
	err := db.QueryRow(`SELECT code FROM (
		SELECT code, used_at FROM reward_codes WHERE used_by = ?
		UNION ALL SELECT code, used_at FROM lottery_prizes WHERE status = 'used' AND winner_id = ?
	) ORDER BY used_at DESC LIMIT 1`, strconv.FormatInt(msg.UserID, 10), msg.UserID).Scan(&code)
	if err != nil {
		return "还没有可发送的兑换码。", ""
	}
	return "兑换码已再私聊发送一次。如果仍收不到，请先发送 /start。", code
}

func activityLabel(activity string) string {
	switch activity {
	case "checkin":
		return "打卡"
	case "recipe":
		return "配方"
	case "fail":
		return "翻车"
	case "correct":
		return "纠错"
	case "pair":
		return "组队"
	case "relay":
		return "接力"
	case "vote":
		return "站队"
	case "ask":
		return "提问"
	case "answer":
		return "回答"
	case "redeem":
		return "兑换"
	case "speech":
		return "每日发言"
	case "lottery":
		return "每日抽奖"
	default:
		return activity
	}
}

func stateGet(key string) string {
	var v string
	_ = db.QueryRow(`SELECT v FROM community_state WHERE k = ?`, key).Scan(&v)
	return v
}

func stateSet(key, value string) error {
	_, err := db.Exec(`INSERT INTO community_state (k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v = excluded.v`, key, value)
	return err
}

func botToken() string {
	mu.RLock()
	defer mu.RUnlock()
	return strings.TrimSpace(cfg.Telegram.BotToken)
}

var communitySend = sendTelegram

func deliverCommunity(msg communityMessage, groupReply, code string) {
	token := botToken()
	if token == "" {
		return
	}
	if code != "" {
		dm := "你的兑换码：\n" + code + "\n请妥善保存，群里不会再公布。"
		if err := communitySend(token, strconv.FormatInt(msg.UserID, 10), dm, nil); err != nil {
			groupReply = strings.TrimSpace(groupReply + "\n私聊发送失败，请先私聊机器人发送 /start，再发送 /code。")
		}
	}
	if strings.TrimSpace(groupReply) != "" {
		_ = communitySend(token, strconv.FormatInt(msg.ChatID, 10), groupReply, nil)
	}
}

func notifyGroup(text string) {
	conf := loadCommunityConfig()
	chat := communityChatID(conf)
	token := botToken()
	if token == "" || chat == "" || strings.TrimSpace(text) == "" {
		return
	}
	_ = communitySend(token, chat, text, nil)
}

func importCodes(cents int, raw string) (int, error) {
	if cents <= 0 {
		return 0, errors.New("额度不正确")
	}
	conf := loadCommunityConfig()
	cost := costForCents(cents, conf)
	if cost < conf.MinPoints {
		return 0, fmt.Errorf("%s元按当前比例需要 %d 积分，低于最低 %d 积分", formatYuan(cents), cost, conf.MinPoints)
	}
	added := 0
	for _, line := range strings.Split(raw, "\n") {
		code := strings.TrimSpace(line)
		if code == "" {
			continue
		}
		if utf8.RuneCountInString(code) < 4 || utf8.RuneCountInString(code) > 80 {
			continue
		}
		res, err := db.Exec(`INSERT INTO reward_codes (code, tier, amount_cents, status) VALUES (?, ?, ?, 'available')`, code, formatYuan(cents)+"元", cents)
		if err != nil {
			continue
		}
		if n, _ := res.RowsAffected(); n > 0 {
			added++
		}
	}
	if added == 0 {
		return 0, errors.New("没有导入新的兑换码")
	}
	return added, nil
}

func listAvailableCodes(limit int) []availableCode {
	if limit <= 0 {
		limit = 300
	}
	rows, err := db.Query(`SELECT id, code, amount_cents FROM reward_codes WHERE status = 'available' ORDER BY amount_cents, id LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []availableCode
	for rows.Next() {
		var item availableCode
		var cents int
		if rows.Scan(&item.ID, &item.Code, &cents) != nil {
			continue
		}
		item.Yuan = formatYuan(cents)
		out = append(out, item)
	}
	return out
}

func deleteAvailableCodes(ids []int64, raw string, cents int, allOfAmount bool) (int, error) {
	removed := 0
	drop := func(res interface{ RowsAffected() (int64, error) }, err error) {
		if err != nil {
			return
		}
		if n, _ := res.RowsAffected(); n > 0 {
			removed += int(n)
		}
	}
	if allOfAmount {
		if cents <= 0 {
			return 0, errors.New("请先填写额度")
		}
		res, err := db.Exec(`DELETE FROM reward_codes WHERE status = 'available' AND amount_cents = ?`, cents)
		drop(res, err)
	}
	for _, id := range ids {
		if id <= 0 {
			continue
		}
		res, err := db.Exec(`DELETE FROM reward_codes WHERE id = ? AND status = 'available'`, id)
		drop(res, err)
	}
	for _, line := range strings.Split(raw, "\n") {
		code := strings.TrimSpace(line)
		if code == "" {
			continue
		}
		res, err := db.Exec(`DELETE FROM reward_codes WHERE code = ? AND status = 'available'`, code)
		drop(res, err)
	}
	if removed == 0 {
		return 0, errors.New("没有可删除的未使用兑换码")
	}
	return removed, nil
}

type availableCode struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
	Yuan string `json:"yuan"`
}

type codeStockItem struct {
	Yuan   string `json:"yuan"`
	Cents  int    `json:"cents"`
	Count  int    `json:"count"`
	Points int    `json:"points"`
}

func codeStock() []codeStockItem {
	conf := loadCommunityConfig()
	rows, err := db.Query(`SELECT amount_cents, COUNT(*) FROM reward_codes WHERE status = 'available' AND amount_cents > 0 GROUP BY amount_cents ORDER BY amount_cents`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []codeStockItem
	for rows.Next() {
		var cents, n int
		if rows.Scan(&cents, &n) != nil {
			continue
		}
		out = append(out, codeStockItem{
			Yuan:   formatYuan(cents),
			Cents:  cents,
			Count:  n,
			Points: costForCents(cents, conf),
		})
	}
	return out
}

func recentRewards(limit int) []tgLogLine {
	rows, err := db.Query(`SELECT created_at, user_name, activity, tier, points FROM reward_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []tgLogLine
	for rows.Next() {
		var ts int64
		var name, activity, tier string
		var points int
		if rows.Scan(&ts, &name, &activity, &tier, &points) != nil {
			continue
		}
		text := name + " · " + activityLabel(activity) + " · +" + strconv.Itoa(points) + " 积分"
		if activity == "redeem" {
			text = name + " · 兑换" + tier + " · " + strconv.Itoa(points) + " 积分"
		}
		if activity == "speech" || activity == "lottery" {
			text = name + " · " + activityLabel(activity) + " · " + tier
		}
		out = append(out, tgLogLine{
			Time: time.Unix(ts, 0).In(shanghai).Format("01-02 15:04"),
			Text: text,
		})
	}
	return out
}

func adminCommunity(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAdminJSON(w, 200, map[string]any{
			"config":    loadCommunityConfig(),
			"stock":     codeStock(),
			"codes":     listAvailableCodes(300),
			"logs":      recentRewards(40),
			"speech":    speechToday(time.Now()),
			"dashboard": speechDashboard(time.Now()),
			"prizes":    listPrizes(300),
			"prizeLeft": prizeCount(),
			"lineCount": speechLineCount(),
		})
	case http.MethodPut:
		var body CommunityConfig
		if !decodeBody(w, r, &body) {
			return
		}
		if err := saveCommunityConfig(body); err != nil {
			writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeAdminJSON(w, 200, map[string]bool{"ok": true})
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func adminCommunityCodes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Amount string `json:"amount"`
			Codes  string `json:"codes"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		cents, err := parseYuan(body.Amount)
		if err != nil {
			writeAdminJSON(w, 400, map[string]string{"error": "额度格式不正确，例如 1 或 5"})
			return
		}
		added, err := importCodes(cents, body.Codes)
		if err != nil {
			writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"ok": true, "added": added})
	case http.MethodDelete:
		var body struct {
			IDs    []int64 `json:"ids"`
			Codes  string  `json:"codes"`
			Amount string  `json:"amount"`
			All    bool    `json:"all"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		cents := 0
		if body.All {
			var err error
			cents, err = parseYuan(body.Amount)
			if err != nil {
				writeAdminJSON(w, 400, map[string]string{"error": "额度格式不正确，例如 1 或 5"})
				return
			}
		}
		removed, err := deleteAvailableCodes(body.IDs, body.Codes, cents, body.All)
		if err != nil {
			writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"ok": true, "removed": removed})
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}

func leaveSpeech(msg communityMessage) (string, string) {
	_, _ = db.Exec(`DELETE FROM community_members WHERE user_id = ?`, msg.UserID)
	return msg.displayName() + " 已退出每日发言，之后的发言不再计数。", ""
}

func speechDay(now time.Time) string {
	return now.In(shanghai).Format("2006-01-02")
}

func speechMember(userID int64) bool {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM community_members WHERE user_id = ?`, userID).Scan(&n)
	return n > 0
}

func speechCount(userID int64, day string) int {
	var n int
	_ = db.QueryRow(`SELECT messages FROM daily_speech WHERE day = ? AND user_id = ?`, day, userID).Scan(&n)
	return n
}

func recordSpeech(msg communityMessage, now time.Time) {
	if db == nil || msg.UserID == 0 || msg.ChatID == msg.UserID {
		return
	}
	text := strings.TrimSpace(msg.Text)
	if text == "" || strings.HasPrefix(text, "/") {
		return
	}
	conf := loadCommunityConfig()
	if !conf.Enabled {
		return
	}
	want := communityChatID(conf)
	if want == "" || strconv.FormatInt(msg.ChatID, 10) != want {
		return
	}
	if !speechMember(msg.UserID) {
		return
	}
	text = strings.Join(strings.Fields(text), " ")
	day := speechDay(now)
	res, err := db.Exec(`INSERT INTO daily_speech_lines (day, user_id, body, created_at) VALUES (?, ?, ?, ?) ON CONFLICT(day, user_id, body) DO NOTHING`, day, msg.UserID, text, now.Unix())
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return
	}
	_, _ = db.Exec(`INSERT INTO daily_speech (day, user_id, user_name, messages, rewarded) VALUES (?, ?, ?, 1, 0)
		ON CONFLICT(day, user_id) DO UPDATE SET messages = messages + 1, user_name = excluded.user_name`,
		day, msg.UserID, msg.displayName())
}

type speechSnapshot struct {
	Members   int `json:"members"`
	Speakers  int `json:"speakers"`
	Qualified int `json:"qualified"`
}

func speechToday(now time.Time) speechSnapshot {
	conf := loadCommunityConfig()
	day := speechDay(now)
	var out speechSnapshot
	_ = db.QueryRow(`SELECT COUNT(*) FROM community_members`).Scan(&out.Members)
	_ = db.QueryRow(`SELECT COUNT(*) FROM daily_speech s JOIN community_members m ON m.user_id = s.user_id WHERE s.day = ? AND s.messages > 0`, day).Scan(&out.Speakers)
	_ = db.QueryRow(`SELECT COUNT(*) FROM daily_speech s JOIN community_members m ON m.user_id = s.user_id WHERE s.day = ? AND s.messages >= ?`, day, conf.SpeechNeed).Scan(&out.Qualified)
	return out
}

type dashDay struct {
	Day      string `json:"day"`
	Speakers int    `json:"speakers"`
	Messages int    `json:"messages"`
	Rewarded int    `json:"rewarded"`
}

type speechDash struct {
	Members   int       `json:"members"`
	Speakers  int       `json:"speakers"`
	Qualified int       `json:"qualified"`
	Messages  int       `json:"messages"`
	Need      int       `json:"need"`
	Winners   int       `json:"winners"`
	Days      []dashDay `json:"days"`
}

func speechDashboard(now time.Time) speechDash {
	conf := loadCommunityConfig()
	today := speechToday(now)
	out := speechDash{
		Members: today.Members, Speakers: today.Speakers, Qualified: today.Qualified,
		Need: conf.SpeechNeed, Winners: conf.Winners,
		Days: []dashDay{},
	}
	day := speechDay(now)
	_ = db.QueryRow(`SELECT COALESCE(SUM(messages), 0) FROM daily_speech WHERE day = ?`, day).Scan(&out.Messages)
	rows, err := db.Query(`SELECT day, COUNT(*), COALESCE(SUM(messages), 0), COALESCE(SUM(rewarded), 0)
		FROM daily_speech GROUP BY day ORDER BY day DESC LIMIT 30`)
	if err == nil {
		for rows.Next() {
			var item dashDay
			if rows.Scan(&item.Day, &item.Speakers, &item.Messages, &item.Rewarded) == nil {
				out.Days = append(out.Days, item)
			}
		}
		rows.Close()
	}
	return out
}

func settleDue(now time.Time) {
	if db == nil {
		return
	}
	today := speechDay(now)
	rows, err := db.Query(`SELECT DISTINCT day FROM daily_speech WHERE day < ? AND day NOT IN (SELECT day FROM daily_settle) ORDER BY day`, today)
	if err != nil {
		return
	}
	var days []string
	for rows.Next() {
		var day string
		if rows.Scan(&day) == nil && day != "" {
			days = append(days, day)
		}
	}
	rows.Close()
	for _, day := range days {
		settleSpeechDay(day, now)
	}
}

func notifyUser(userID int64, text string) error {
	token := botToken()
	if token == "" || strings.TrimSpace(text) == "" {
		return errors.New("发不出去")
	}
	return communitySend(token, strconv.FormatInt(userID, 10), text, nil)
}

var communityClient = &http.Client{Timeout: 70 * time.Second}

func redactTelegramErr(err error) string {
	if err == nil {
		return ""
	}
	s := err.Error()
	if i := strings.Index(s, "/bot"); i >= 0 {
		rest := s[i+4:]
		if j := strings.Index(rest, "/"); j >= 0 {
			s = s[:i+4] + "***" + rest[j:]
		}
	}
	return s
}

func startCommunity() {
	go communityPollLoop()
	go communitySettleLoop()
}

func communitySettleLoop() {
	var lastPrune time.Time
	for {
		time.Sleep(30 * time.Second)
		if db == nil {
			continue
		}
		conf := loadCommunityConfig()
		if conf.CleanLines && time.Since(lastPrune) >= 10*time.Minute {
			pruneSpeechLines(time.Now())
			lastPrune = time.Now()
		}
		if !conf.Enabled {
			continue
		}
		settleDue(time.Now())
	}
}

// Chat lines only exist to drop same-day duplicates, so 48 hours is plenty.
const speechLineKeep = 48 * time.Hour

func pruneSpeechLines(now time.Time) int64 {
	var removed int64
	if res, err := db.Exec(`DELETE FROM daily_speech_lines WHERE created_at < ?`, now.Add(-speechLineKeep).Unix()); err == nil {
		removed += rowsOf(res)
	}
	removed += dropSpeechCounts(speechDay(now.Add(-speechLineKeep)))
	return removed
}

// dropSpeechCounts removes per-user daily counts before day, but only for days
// already drawn: an unsettled day still needs its pool.
func dropSpeechCounts(before string) int64 {
	res, err := db.Exec(`DELETE FROM daily_speech WHERE day < ? AND day IN (SELECT day FROM daily_settle)`, before)
	if err != nil {
		return 0
	}
	return rowsOf(res)
}

// clearSpeechLines keeps today's lines and counts so repeated messages still count once.
func clearSpeechLines(now time.Time) (int64, error) {
	res, err := db.Exec(`DELETE FROM daily_speech_lines WHERE day < ?`, speechDay(now))
	if err != nil {
		return 0, err
	}
	removed := rowsOf(res) + dropSpeechCounts(speechDay(now))
	_, _ = db.Exec(`VACUUM`)
	return removed, nil
}

func speechLineCount() int {
	var n int
	_ = db.QueryRow(`SELECT (SELECT COUNT(*) FROM daily_speech_lines) + (SELECT COUNT(*) FROM daily_speech)`).Scan(&n)
	return n
}

func adminCommunityLines(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
		return
	}
	n, err := clearSpeechLines(time.Now())
	if err != nil {
		writeAdminJSON(w, 500, map[string]string{"error": "清理失败"})
		return
	}
	writeAdminJSON(w, 200, map[string]any{"ok": true, "removed": n})
}

func communityPollLoop() {
	for {
		if db == nil || botToken() == "" || !loadCommunityConfig().Enabled {
			time.Sleep(5 * time.Second)
			continue
		}
		if err := pollCommunityOnce(); err != nil {
			log.Println("community poll:", redactTelegramErr(err))
			time.Sleep(5 * time.Second)
		}
	}
}

func maybeDeleteWebhook() {
	last, _ := strconv.ParseInt(stateGet("webhook_cleared_at"), 10, 64)
	if time.Now().Unix()-last < 600 {
		return
	}
	token := botToken()
	if token == "" {
		return
	}
	req, err := http.NewRequest(http.MethodPost, telegramAPI+"/bot"+token+"/deleteWebhook", strings.NewReader(`{"drop_pending_updates":false}`))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := communityClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
	_ = stateSet("webhook_cleared_at", strconv.FormatInt(time.Now().Unix(), 10))
}

func pollCommunityOnce() error {
	maybeDeleteWebhook()
	offset := 0
	if raw := stateGet("update_offset"); raw != "" {
		offset, _ = strconv.Atoi(raw)
	}
	payload, _ := json.Marshal(map[string]any{
		"offset":          offset,
		"timeout":         20,
		"allowed_updates": []string{"message"},
	})
	req, err := http.NewRequest(http.MethodPost, telegramAPI+"/bot"+botToken()+"/getUpdates", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := communityClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var parsed struct {
		OK     bool `json:"ok"`
		Result []struct {
			UpdateID int `json:"update_id"`
			Message  *struct {
				MessageID int    `json:"message_id"`
				Text      string `json:"text"`
				Chat      struct {
					ID int64 `json:"id"`
				} `json:"chat"`
				From struct {
					ID        int64  `json:"id"`
					Username  string `json:"username"`
					FirstName string `json:"first_name"`
				} `json:"from"`
				Reply *struct {
					MessageID int    `json:"message_id"`
					Text      string `json:"text"`
					From      struct {
						ID       int64  `json:"id"`
						Username string `json:"username"`
					} `json:"from"`
				} `json:"reply_to_message"`
			} `json:"message"`
		} `json:"result"`
	}
	if json.Unmarshal(body, &parsed) != nil || !parsed.OK {
		return errors.New("读取群消息失败")
	}
	next := offset
	for _, item := range parsed.Result {
		if item.UpdateID+1 > next {
			next = item.UpdateID + 1
		}
		if item.Message == nil || strings.TrimSpace(item.Message.Text) == "" {
			continue
		}
		msg := communityMessage{
			ChatID:    item.Message.Chat.ID,
			UserID:    item.Message.From.ID,
			UserName:  item.Message.From.Username,
			Text:      item.Message.Text,
			MessageID: item.Message.MessageID,
		}
		if item.Message.Reply != nil {
			msg.ReplyUserID = item.Message.Reply.From.ID
			msg.ReplyUserName = item.Message.Reply.From.Username
			msg.ReplyText = item.Message.Reply.Text
			msg.ReplyMessageID = item.Message.Reply.MessageID
		}
		groupReply, code := applyCommunity(msg, time.Now())
		recordSpeech(msg, time.Now())
		deliverCommunity(msg, groupReply, code)
	}
	if next != offset {
		_ = stateSet("update_offset", strconv.Itoa(next))
	}
	return nil
}
