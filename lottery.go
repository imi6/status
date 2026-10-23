package main

import (
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

func joinSpeech(msg communityMessage, now time.Time) (string, string) {
	conf := loadCommunityConfig()
	_, err := db.Exec(`INSERT INTO community_members (user_id, user_name, joined_at) VALUES (?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET user_name = excluded.user_name`,
		msg.UserID, msg.displayName(), now.Unix())
	if err != nil {
		return "报名失败，请稍后再试。", ""
	}
	day := speechDay(now)
	return fmt.Sprintf("%s 已报名，当前共 %d 人报名。今天 %d/%d 句，今日奖池 %d 人。每天满 %d 句进入抽奖池，次日随机抽 %d 人私信发奖。%s请先私聊机器人 /start。",
		msg.displayName(), memberCount(), speechCount(msg.UserID, day), conf.SpeechNeed, poolSize(day, conf), conf.SpeechNeed, conf.Winners, minPoolText(conf)), ""
}

// minPoolText is empty when any pool size draws, so the default reads as before.
func minPoolText(conf CommunityConfig) string {
	if conf.MinPool <= 1 {
		return ""
	}
	return fmt.Sprintf("奖池满 %d 人才开奖，不满则当天不开奖。", conf.MinPool)
}

func memberCount() int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM community_members`).Scan(&n)
	return n
}

func poolSize(day string, conf CommunityConfig) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM daily_speech s JOIN community_members m ON m.user_id = s.user_id WHERE s.day = ? AND s.messages >= ?`, day, conf.SpeechNeed).Scan(&n)
	return n
}

func poolReply(msg communityMessage, now time.Time) string {
	conf := loadCommunityConfig()
	day := speechDay(now)
	lines := []string{fmt.Sprintf("当前报名 %d 人，今日奖池 %d 人，次日抽 %d 人。%s", memberCount(), poolSize(day, conf), conf.Winners, minPoolText(conf))}
	if speechMember(msg.UserID) {
		lines = append(lines, fmt.Sprintf("%s 今天 %d/%d 句。", msg.displayName(), speechCount(msg.UserID, day), conf.SpeechNeed))
	} else {
		lines = append(lines, msg.displayName()+" 还没报名，发送 /join 参加。")
	}
	if items := prizeStock(); len(items) == 0 {
		lines = append(lines, "奖品还没准备好。")
	} else {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			parts = append(parts, fmt.Sprintf("%s × %d", item.Name, item.Count))
		}
		lines = append(lines, "奖品："+strings.Join(parts, "，"))
	}
	return strings.Join(lines, "\n")
}

type poolEntry struct {
	id   int64
	name string
}

// lotteryShuffle is swapped in tests for a deterministic draw.
var lotteryShuffle = rand.Shuffle

func drawWinners(pool []poolEntry, n int) []poolEntry {
	picked := append([]poolEntry(nil), pool...)
	lotteryShuffle(len(picked), func(i, j int) { picked[i], picked[j] = picked[j], picked[i] })
	return picked[:min(n, len(picked))]
}

func settleSpeechDay(day string, now time.Time) {
	conf := loadCommunityConfig()
	var settled int
	_ = db.QueryRow(`SELECT COUNT(*) FROM daily_settle WHERE day = ?`, day).Scan(&settled)
	if settled > 0 {
		return
	}
	rows, err := db.Query(`SELECT s.user_id, s.user_name FROM daily_speech s JOIN community_members m ON m.user_id = s.user_id WHERE s.day = ? AND s.messages >= ? AND s.rewarded = 0 ORDER BY s.user_id`, day, conf.SpeechNeed)
	if err != nil {
		return
	}
	var pool []poolEntry
	for rows.Next() {
		var item poolEntry
		if rows.Scan(&item.id, &item.name) == nil {
			pool = append(pool, item)
		}
	}
	rows.Close()
	if len(pool) == 0 {
		_, _ = db.Exec(`INSERT INTO daily_settle (day, participants, rewarded, settled_at) VALUES (?, 0, 0, ?)`, day, now.Unix())
		return
	}
	if len(pool) < conf.MinPool {
		_, _ = db.Exec(`INSERT INTO daily_settle (day, participants, rewarded, settled_at) VALUES (?, ?, 0, ?)`, day, len(pool), now.Unix())
		notifyGroup(fmt.Sprintf("%s 奖池 %d 人，未满 %d 人，今日不开奖。", day, len(pool), conf.MinPool))
		return
	}
	want := min(conf.Winners, len(pool))
	if left := prizeCount(); left < want {
		if left == 0 {
			if stateGet("speech_wait:"+day) != "1" {
				notifyGroup(fmt.Sprintf("%s 奖池 %d 人，但奖品库存为空，补货后再开奖。", day, len(pool)))
				_ = stateSet("speech_wait:"+day, "1")
			}
			return
		}
		want = left
	}
	var names []string
	failed := 0
	for _, item := range drawWinners(pool, want) {
		prize, code, err := grantPrize(item.id, item.name, day, now)
		if err != nil {
			break
		}
		_, _ = db.Exec(`UPDATE daily_speech SET rewarded = 1 WHERE day = ? AND user_id = ?`, day, item.id)
		names = append(names, item.name+"（"+prize+"）")
		if notifyUser(item.id, "恭喜你在 "+day+" 的每日抽奖中奖："+prize+"\n"+code+"\n请妥善保存，群里不会公布。") != nil {
			failed++
		}
	}
	_, _ = db.Exec(`INSERT INTO daily_settle (day, participants, rewarded, settled_at) VALUES (?, ?, ?, ?)`, day, len(pool), len(names), now.Unix())
	_ = stateSet("speech_wait:"+day, "")
	notice := fmt.Sprintf("%s 每日抽奖：奖池 %d 人，抽中 %d 人：%s。", day, len(pool), len(names), strings.Join(names, "、"))
	if failed > 0 {
		notice += " 私信失败的请先私聊机器人 /start，再发送 /code。"
	}
	notifyGroup(notice)
}

func grantPrize(userID int64, name, day string, now time.Time) (string, string, error) {
	communityMu.Lock()
	defer communityMu.Unlock()
	var id int64
	var prize, code string
	if err := db.QueryRow(`SELECT id, name, code FROM lottery_prizes WHERE status = 'available' ORDER BY id LIMIT 1`).Scan(&id, &prize, &code); err != nil {
		return "", "", err
	}
	mark, err := db.Exec(`UPDATE lottery_prizes SET status = 'used', winner_id = ?, winner_name = ?, day = ?, used_at = ? WHERE id = ? AND status = 'available'`,
		userID, name, day, now.Unix(), id)
	if err != nil || rowsOf(mark) == 0 {
		return "", "", errors.New("奖品已被领取")
	}
	_, _ = db.Exec(`INSERT INTO reward_logs (activity, tier, user_id, user_name, text, created_at, code_id, points) VALUES ('lottery', ?, ?, ?, ?, ?, ?, 0)`,
		prize, userID, name, "每日抽奖 "+day, now.Unix(), id)
	return prize, code, nil
}

func prizeCount() int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lottery_prizes WHERE status = 'available'`).Scan(&n)
	return n
}

type prizeStockItem struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

func prizeStock() []prizeStockItem {
	rows, err := db.Query(`SELECT name, COUNT(*) FROM lottery_prizes WHERE status = 'available' GROUP BY name ORDER BY MIN(id)`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []prizeStockItem
	for rows.Next() {
		var item prizeStockItem
		if rows.Scan(&item.Name, &item.Count) == nil {
			out = append(out, item)
		}
	}
	return out
}

type prizeRow struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Code   string `json:"code"`
	Status string `json:"status"`
	Winner string `json:"winner"`
	Day    string `json:"day"`
}

func listPrizes(limit int) []prizeRow {
	out := []prizeRow{}
	rows, err := db.Query(`SELECT id, name, code, status, winner_name, day FROM lottery_prizes ORDER BY status = 'used', id DESC LIMIT ?`, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var item prizeRow
		if rows.Scan(&item.ID, &item.Name, &item.Code, &item.Status, &item.Winner, &item.Day) == nil {
			out = append(out, item)
		}
	}
	return out
}

func importPrizes(name, raw string) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		return 0, errors.New("奖品名称需在 1 到 40 字")
	}
	added := 0
	for _, line := range strings.Split(raw, "\n") {
		code := strings.TrimSpace(line)
		if utf8.RuneCountInString(code) < 4 || utf8.RuneCountInString(code) > 200 {
			continue
		}
		res, err := db.Exec(`INSERT INTO lottery_prizes (name, code) VALUES (?, ?) ON CONFLICT(code) DO NOTHING`, name, code)
		if err == nil {
			added += int(rowsOf(res))
		}
	}
	if added == 0 {
		return 0, errors.New("没有导入新的 CDK（每行一个，4 到 200 字，不能重复）")
	}
	return added, nil
}

func deletePrizes(ids []int64) int {
	removed := 0
	for _, id := range ids {
		res, err := db.Exec(`DELETE FROM lottery_prizes WHERE id = ? AND status = 'available'`, id)
		if err == nil {
			removed += int(rowsOf(res))
		}
	}
	return removed
}

func adminCommunityPrizes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Codes string `json:"codes"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		added, err := importPrizes(body.Name, body.Codes)
		if err != nil {
			writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"ok": true, "added": added})
	case http.MethodDelete:
		var body struct {
			IDs []int64 `json:"ids"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		removed := deletePrizes(body.IDs)
		if removed == 0 {
			writeAdminJSON(w, 400, map[string]string{"error": "没有可删除的未发出奖品"})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"ok": true, "removed": removed})
	default:
		writeAdminJSON(w, 405, map[string]string{"error": "method not allowed"})
	}
}
