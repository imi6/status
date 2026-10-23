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
)

var shanghai = time.FixedZone("UTC+8", 8*3600)

type CommunityConfig struct {
	Enabled    bool   `json:"enabled"`
	ChatID     string `json:"chatID"`
	SpeechNeed int    `json:"speechNeed"`
	Winners    int    `json:"winners"`
	MinPool    int    `json:"minPool"`
	CleanLines bool   `json:"cleanLines"`
}

func defaultCommunityConfig() CommunityConfig {
	return CommunityConfig{
		SpeechNeed: 5, Winners: 1, MinPool: 100, CleanLines: true,
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
	_, _ = db.Exec(`ALTER TABLE lottery_prizes ADD COLUMN plan_day TEXT NOT NULL DEFAULT ''`)
	// Unsent codes from the retired points and fixed-reward flows join the lottery prizes.
	_, _ = db.Exec(`INSERT OR IGNORE INTO lottery_prizes (name, code) SELECT tier, code FROM reward_codes WHERE status = 'available'`)
	_, _ = db.Exec(`DELETE FROM reward_codes WHERE status = 'available' AND code IN (SELECT code FROM lottery_prizes)`)
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
	if cfg.SpeechNeed <= 0 {
		cfg.SpeechNeed = def.SpeechNeed
	}
	if cfg.Winners <= 0 {
		cfg.Winners = def.Winners
	}
	if cfg.MinPool <= 0 {
		cfg.MinPool = def.MinPool
	}
	return cfg
}

func saveCommunityConfig(c CommunityConfig) error {
	if c.SpeechNeed < 1 || c.SpeechNeed > 1000 {
		return errors.New("每日发言条数需在 1 到 1000")
	}
	if c.Winners < 1 || c.Winners > 1000 {
		return errors.New("每天中奖人数需在 1 到 1000")
	}
	if c.MinPool < 1 || c.MinPool > 100000 {
		return errors.New("最少奖池人数需在 1 到 100000")
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
		return "发送 /join 参加每日抽奖，/pool 查看今天奖池。", ""
	}
}

func communityHelp() string {
	conf := loadCommunityConfig()
	return strings.Join([]string{
		"每日抽奖要先 /join，没报名的发言不计数。报名一次长期有效。",
		fmt.Sprintf("每天发言满 %d 句进入当天抽奖池，次日随机抽 %d 人，私信发奖。", conf.SpeechNeed, conf.Winners) + minPoolText(conf),
		"请先私聊机器人 /start，否则兑换码发不出去。",
		"/join   报名参加",
		"/leave   退出，之后不再计数",
		"/pool   查看今天奖池和奖品",
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

func rowsOf(res interface{ RowsAffected() (int64, error) }) int64 {
	if res == nil {
		return 0
	}
	n, _ := res.RowsAffected()
	return n
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

func recentRewards(limit int) []tgLogLine {
	rows, err := db.Query(`SELECT created_at, user_name, activity, tier FROM reward_logs WHERE activity IN ('lottery', 'speech') ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []tgLogLine
	for rows.Next() {
		var ts int64
		var name, activity, tier string
		if rows.Scan(&ts, &name, &activity, &tier) != nil {
			continue
		}
		label := "每日抽奖"
		if activity == "speech" {
			label = "每日发言"
		}
		out = append(out, tgLogLine{
			Time: time.Unix(ts, 0).In(shanghai).Format("01-02 15:04"),
			Text: name + " · " + label + " · " + tier,
		})
	}
	return out
}

func adminCommunity(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeAdminJSON(w, 200, map[string]any{
			"config":    loadCommunityConfig(),
			"logs":      recentRewards(40),
			"speech":    speechToday(time.Now()),
			"dashboard": speechDashboard(time.Now()),
			"prizes":    listPrizes(300),
			"prizeLeft": prizeCount(),
			"lineCount": speechLineCount(),
			"today":     speechDay(time.Now()),
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
	MinPool   int       `json:"minPool"`
	Days      []dashDay `json:"days"`
}

func speechDashboard(now time.Time) speechDash {
	conf := loadCommunityConfig()
	today := speechToday(now)
	out := speechDash{
		Members: today.Members, Speakers: today.Speakers, Qualified: today.Qualified,
		Need: conf.SpeechNeed, Winners: dayWinners(speechDay(now), conf), MinPool: conf.MinPool,
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
