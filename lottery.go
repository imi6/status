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
	return fmt.Sprintf("%s 已报名，当前共 %d 人报名。今天 %d/%d 句，今日奖池 %d 人。每天满 %d 句进入抽奖池，次日 0 点随机开奖、私信发奖。%s请先私聊机器人 /start。",
		msg.displayName(), memberCount(), speechCount(msg.UserID, day), conf.SpeechNeed, poolSize(day, conf), conf.SpeechNeed, tierText(day, conf)), ""
}

// tierText describes how many people each pool size draws, highest tier first.
func tierText(day string, conf CommunityConfig) string {
	planned := plannedCount(day) > 0
	tiers := dayTiers(day, planned, conf)
	if len(tiers) == 0 {
		if conf.MinPool <= 1 {
			return ""
		}
		return fmt.Sprintf("奖池满 %d 人才开奖。", conf.MinPool)
	}
	low := tiers[len(tiers)-1].Need
	if len(tiers) == 1 {
		if low <= 1 {
			return fmt.Sprintf("奖池有人就开奖，抽 %d 人。", tierDraw(tiers[0], planned, conf))
		}
		return fmt.Sprintf("奖池满 %d 人才开奖，抽 %d 人。", low, tierDraw(tiers[0], planned, conf))
	}
	parts := make([]string, 0, len(tiers))
	for i, t := range tiers {
		part := fmt.Sprintf("满 %d 人抽 %d 人", t.Need, tierDraw(t, planned, conf))
		if i > 0 {
			part += "（兜底）"
		}
		parts = append(parts, part)
	}
	return fmt.Sprintf("奖池%s，不满 %d 人当天不开奖。", strings.Join(parts, "，"), low)
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
	size := poolSize(day, conf)
	planned := plannedCount(day) > 0
	tiers := dayTiers(day, planned, conf)
	reach := ""
	if t, ok := pickTier(tiers, size); ok {
		reach = fmt.Sprintf("按现在人数次日抽 %d 人。", min(tierDraw(t, planned, conf), size))
	} else if len(tiers) > 0 {
		reach = fmt.Sprintf("还差 %d 人才能开奖。", tiers[len(tiers)-1].Need-size)
	}
	lines := []string{fmt.Sprintf("当前报名 %d 人，今日奖池 %d 人。%s%s", memberCount(), size, reach, tierText(day, conf))}
	if speechMember(msg.UserID) {
		lines = append(lines, fmt.Sprintf("%s 今天 %d/%d 句。", msg.displayName(), speechCount(msg.UserID, day), conf.SpeechNeed))
	} else {
		lines = append(lines, msg.displayName()+" 还没报名，发送 /join 参加。")
	}
	if items := prizeStockFor(day, planned, conf); len(items) == 0 {
		lines = append(lines, "奖品还没准备好。")
	} else {
		parts := make([]string, 0, len(items))
		for _, item := range items {
			part := fmt.Sprintf("%s × %d", item.Name, item.Count)
			if len(tiers) > 1 {
				part += fmt.Sprintf("（满 %d 人）", item.Need)
			}
			parts = append(parts, part)
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
	planned := plannedCount(day) > 0
	tiers := dayTiers(day, planned, conf)
	if len(tiers) == 0 {
		if stateGet("speech_wait:"+day) != "1" {
			notifyGroup(fmt.Sprintf("%s 奖池 %d 人，但奖品库存为空，补货后再开奖。", day, len(pool)))
			_ = stateSet("speech_wait:"+day, "1")
		}
		return
	}
	tier, ok := pickTier(tiers, len(pool))
	if !ok {
		_, _ = db.Exec(`INSERT INTO daily_settle (day, participants, rewarded, settled_at) VALUES (?, ?, 0, ?)`, day, len(pool), now.Unix())
		_ = stateSet("speech_wait:"+day, "")
		notifyGroup(fmt.Sprintf("%s 奖池 %d 人，未满 %d 人，今日不开奖。", day, len(pool), tiers[len(tiers)-1].Need))
		return
	}
	want := min(tierDraw(tier, planned, conf), len(pool))
	var names []string
	failed := 0
	for _, item := range drawWinners(pool, want) {
		prize, code, err := grantPrize(item.id, item.name, day, planned, tier.Need, conf, now)
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
	if len(tiers) > 1 && tier.Need < tiers[0].Need {
		notice = fmt.Sprintf("%s 每日抽奖：奖池 %d 人，未满 %d 人，按 %d 人档兜底开奖，抽中 %d 人：%s。", day, len(pool), tiers[0].Need, tier.Need, len(names), strings.Join(names, "、"))
	}
	if failed > 0 {
		notice += " 私信失败的请先私聊机器人 /start，再发送 /code。"
	}
	notifyGroup(notice)
}

// A prize's need_pool is the pool size it needs; 0 means "use 最少奖池人数".
// needExpr resolves that per row and takes conf.MinPool as its parameter.
const needExpr = `(CASE WHEN need_pool > 0 THEN need_pool ELSE ? END)`

// dayScope picks the prizes a day may hand out and takes the day as its
// parameter. A day with a plan uses only its own prizes. Otherwise it uses
// unplanned prizes plus leftovers from past plans, and prizes planned for other
// days stay reserved.
func dayScope(planned bool) string {
	if planned {
		return `plan_day = ?`
	}
	return `(plan_day = '' OR plan_day < ?)`
}

func plannedCount(day string) int {
	var n int
	_ = db.QueryRow(`SELECT COUNT(*) FROM lottery_prizes WHERE status = 'available' AND plan_day = ?`, day).Scan(&n)
	return n
}

// prizeTier is one pool-size threshold and how many prizes sit at exactly it.
type prizeTier struct {
	Need  int
	Count int
}

// dayTiers lists the day's thresholds, highest first.
func dayTiers(day string, planned bool, conf CommunityConfig) []prizeTier {
	rows, err := db.Query(`SELECT `+needExpr+` AS need, COUNT(*) FROM lottery_prizes
		WHERE status = 'available' AND `+dayScope(planned)+` GROUP BY need ORDER BY need DESC`, conf.MinPool, day)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []prizeTier
	for rows.Next() {
		var t prizeTier
		if rows.Scan(&t.Need, &t.Count) == nil {
			out = append(out, t)
		}
	}
	// Each tier can also hand out the prizes of every lower tier.
	for i := len(out) - 2; i >= 0; i-- {
		out[i].Count += out[i+1].Count
	}
	return out
}

// pickTier returns the highest tier the pool size reaches.
func pickTier(tiers []prizeTier, size int) (prizeTier, bool) {
	for _, t := range tiers {
		if size >= t.Need {
			return t, true
		}
	}
	return prizeTier{}, false
}

// drawNow is how many people the draw would pick at the current pool size.
func drawNow(day string, size int, conf CommunityConfig) int {
	planned := plannedCount(day) > 0
	t, ok := pickTier(dayTiers(day, planned, conf), size)
	if !ok {
		return 0
	}
	return min(tierDraw(t, planned, conf), size)
}

// tierDraw is how many people a tier draws: every prize it unlocks when the day
// is planned, otherwise 每天中奖人数 capped by stock.
func tierDraw(t prizeTier, planned bool, conf CommunityConfig) int {
	if planned {
		return t.Count
	}
	return min(conf.Winners, t.Count)
}

// grantPrize hands out the highest-threshold prize the tier unlocks, so big
// prizes go on big days and low-threshold ones stay as the fallback.
func grantPrize(userID int64, name, day string, planned bool, reach int, conf CommunityConfig, now time.Time) (string, string, error) {
	communityMu.Lock()
	defer communityMu.Unlock()
	var id int64
	var prize, code string
	err := db.QueryRow(`SELECT id, name, code FROM lottery_prizes WHERE status = 'available' AND `+dayScope(planned)+` AND `+needExpr+` <= ?
		ORDER BY `+needExpr+` DESC, plan_day, id LIMIT 1`, day, conf.MinPool, reach, conf.MinPool).Scan(&id, &prize, &code)
	if err != nil {
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
	Need  int    `json:"need"`
	Count int    `json:"count"`
}

func prizeStockFor(day string, planned bool, conf CommunityConfig) []prizeStockItem {
	rows, err := db.Query(`SELECT name, `+needExpr+` AS need, COUNT(*) FROM lottery_prizes WHERE status = 'available' AND `+dayScope(planned)+`
		GROUP BY name, need ORDER BY need DESC, MIN(id)`, conf.MinPool, day)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []prizeStockItem
	for rows.Next() {
		var item prizeStockItem
		if rows.Scan(&item.Name, &item.Need, &item.Count) == nil {
			out = append(out, item)
		}
	}
	return out
}

type prizeRow struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Code    string `json:"code"`
	Status  string `json:"status"`
	Winner  string `json:"winner"`
	Day     string `json:"day"`
	PlanDay string `json:"planDay"`
	Need    int    `json:"need"`
}

// listPrizes puts unsent prizes first in the order they go out: planned days
// earliest first, higher thresholds first, then import order. Sent prizes
// follow, newest first.
func listPrizes(limit int) []prizeRow {
	out := []prizeRow{}
	rows, err := db.Query(`SELECT id, name, code, status, winner_name, day, plan_day, need_pool FROM lottery_prizes
		ORDER BY status = 'used', plan_day = '', plan_day, need_pool DESC,
		CASE WHEN status = 'used' THEN -used_at ELSE id END LIMIT ?`, limit)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var item prizeRow
		if rows.Scan(&item.ID, &item.Name, &item.Code, &item.Status, &item.Winner, &item.Day, &item.PlanDay, &item.Need) == nil {
			out = append(out, item)
		}
	}
	return out
}

func checkNeed(need int) error {
	if need < 0 || need > 100000 {
		return errors.New("开奖人数门槛需在 0 到 100000，0 表示用最少奖池人数")
	}
	return nil
}

func importPrizes(name, raw string, need int) (int, error) {
	name = strings.TrimSpace(name)
	if name == "" || utf8.RuneCountInString(name) > 40 {
		return 0, errors.New("奖品名称需在 1 到 40 字")
	}
	if err := checkNeed(need); err != nil {
		return 0, err
	}
	added := 0
	for _, line := range strings.Split(raw, "\n") {
		code := strings.TrimSpace(line)
		if utf8.RuneCountInString(code) < 4 || utf8.RuneCountInString(code) > 200 {
			continue
		}
		res, err := db.Exec(`INSERT INTO lottery_prizes (name, code, need_pool) VALUES (?, ?, ?) ON CONFLICT(code) DO NOTHING`, name, code, need)
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

// planPrizes reserves unsent prizes for one speech day; an empty day clears the plan.
func planPrizes(ids []int64, day string) (int, error) {
	day = strings.TrimSpace(day)
	if day != "" {
		if _, err := time.ParseInLocation("2006-01-02", day, shanghai); err != nil {
			return 0, errors.New("日期格式应为 2026-10-23")
		}
	}
	return updatePrizes(ids, `plan_day = ?`, day)
}

// setPrizeNeed changes the pool-size threshold of unsent prizes.
func setPrizeNeed(ids []int64, need int) (int, error) {
	if err := checkNeed(need); err != nil {
		return 0, err
	}
	return updatePrizes(ids, `need_pool = ?`, need)
}

func updatePrizes(ids []int64, set string, value any) (int, error) {
	changed := 0
	for _, id := range ids {
		res, err := db.Exec(`UPDATE lottery_prizes SET `+set+` WHERE id = ? AND status = 'available'`, value, id)
		if err == nil {
			changed += int(rowsOf(res))
		}
	}
	if changed == 0 {
		return 0, errors.New("请先勾选未发出的奖品")
	}
	return changed, nil
}

func adminCommunityPrizes(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPut:
		var body struct {
			IDs  []int64 `json:"ids"`
			Day  *string `json:"day"`
			Need *int    `json:"need"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		var changed int
		var err error
		switch {
		case body.Need != nil:
			changed, err = setPrizeNeed(body.IDs, *body.Need)
		case body.Day != nil:
			changed, err = planPrizes(body.IDs, *body.Day)
		default:
			err = errors.New("没有要修改的内容")
		}
		if err != nil {
			writeAdminJSON(w, 400, map[string]string{"error": err.Error()})
			return
		}
		writeAdminJSON(w, 200, map[string]any{"ok": true, "changed": changed})
	case http.MethodPost:
		var body struct {
			Name  string `json:"name"`
			Codes string `json:"codes"`
			Need  int    `json:"need"`
		}
		if !decodeBody(w, r, &body) {
			return
		}
		added, err := importPrizes(body.Name, body.Codes, body.Need)
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
