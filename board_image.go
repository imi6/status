package main

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

type boardCard struct {
	name, ratio, status, rate string
	dots                      []int
}

type boardSection struct {
	name  string
	cards []boardCard
}

func renderStatusBoard(title string, sections []Section, now time.Time) ([][]byte, error) {
	titleFace, nameFace, metaFace, err := boardFaces()
	if err != nil {
		return nil, err
	}
	defer closeFace(titleFace)
	defer closeFace(nameFace)
	defer closeFace(metaFace)

	title = strings.TrimSpace(title)
	if title == "" {
		title = "分组状态"
	}
	groups := buildBoardSections(sections)
	pngBytes, err := drawBoardPage(titleFace, nameFace, metaFace, title, groups, now, columnsFor(groups))
	if err != nil {
		return nil, err
	}
	out := [][]byte{pngBytes}
	return out, nil
}

func buildBoardCards(sections []Section) []boardCard {
	var cards []boardCard
	for _, group := range buildBoardSections(sections) {
		cards = append(cards, group.cards...)
	}
	return cards
}

func buildBoardSections(sections []Section) []boardSection {
	dup := map[string]int{}
	for _, sec := range sections {
		for _, row := range sec.Rows {
			dup[sec.Name+"\x1f"+row.SubName]++
		}
	}
	var groups []boardSection
	for _, sec := range sections {
		var cards []boardCard
		for _, row := range sec.Rows {
			name := strings.TrimSpace(row.SubName)
			if name == "" {
				name = sec.Name
			}
			if dup[sec.Name+"\x1f"+row.SubName] > 1 && strings.TrimSpace(row.Name) != "" {
				name += " · " + strings.TrimSpace(row.Name)
			}
			cards = append(cards, boardCard{
				name:   name,
				ratio:  ratioTag(row.Ratio),
				status: row.Status,
				rate:   imageRate(row),
				dots:   boardDotLevels(row),
			})
		}
		if len(cards) == 0 {
			continue
		}
		groups = append(groups, boardSection{name: strings.TrimSpace(sec.Name), cards: cards})
	}
	return groups
}

func ratioTag(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if strings.Contains(v, "×") || strings.Contains(v, "倍") || strings.ContainsAny(v, "xX") {
		return v
	}
	return v + "x"
}

func imageRate(row GroupResp) string {
	buckets := row.Timelines["probe"]
	if len(buckets) == 0 {
		rate, ok := boardRate(row.Timelines["hour"])
		return formatBoardRate(rate, ok)
	}
	var samples, up int
	for _, b := range buckets {
		samples += b.Samples
		up += b.Up
	}
	if samples == 0 {
		return "--"
	}
	return formatBoardRate(float64(up)/float64(samples)*100, true)
}

func boardDotLevels(row GroupResp) []int {
	buckets := row.Timelines["probe"]
	if len(buckets) == 0 {
		buckets = row.Timelines["hour"]
	}
	if len(buckets) > 12 {
		buckets = buckets[len(buckets)-12:]
	}
	out := make([]int, 12)
	for i := range out {
		out[i] = -1
	}
	start := 12 - len(buckets)
	for i, b := range buckets {
		out[start+i] = dotLevel(b)
	}
	return out
}

func dotLevel(b TimelineBucket) int {
	if b.Samples == 0 {
		return -1
	}
	if b.Rate >= 90 {
		return 2
	}
	if b.Rate >= 60 {
		return 1
	}
	return 0
}

const (
	boardMargin = 28
	boardGap    = 16
	boardCardW  = 524
	boardCardH  = 148
	boardHeadH    = 108
	boardSectionH = 72
	boardBottom   = 28
)

func boardSize(n, cols int) (int, int) {
	return boardExtent([]boardSection{{cards: make([]boardCard, n)}}, cols)
}

func boardExtent(groups []boardSection, cols int) (int, int) {
	if cols < 1 {
		cols = 2
	}
	width := boardMargin*2 + cols*boardCardW + (cols-1)*boardGap
	y := boardHeadH
	any := false
	for _, group := range groups {
		if len(group.cards) == 0 {
			continue
		}
		any = true
		y += boardSectionH
		rows := (len(group.cards) + cols - 1) / cols
		y += rows*boardCardH + (rows-1)*boardGap + boardGap
	}
	if !any {
		return width, boardHeadH + 80
	}
	return width, y - boardGap + boardBottom
}

// boardColumns 卡片多时加列，让整张图不会太长，Telegram 压缩后仍能看清。
func boardColumns(n int) int {
	return columnsFor([]boardSection{{cards: make([]boardCard, n)}})
}

func columnsFor(groups []boardSection) int {
	for cols := 2; cols < 6; cols++ {
		w, h := boardExtent(groups, cols)
		if float64(h) <= float64(w)*1.4 {
			return cols
		}
	}
	return 6
}

func drawBoardPage(titleFace, nameFace, metaFace font.Face, title string, groups []boardSection, now time.Time, cols int) ([]byte, error) {
	const (
		margin = boardMargin
		gap    = boardGap
		cardH  = boardCardH
		headH  = boardHeadH
		radius = 18
	)
	if cols < 1 {
		cols = 2
	}
	width, height := boardExtent(groups, cols)
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	bg := color.RGBA{243, 246, 251, 255}
	draw.Draw(img, img.Bounds(), image.NewUniform(bg), image.Point{}, draw.Src)

	ink := color.RGBA{28, 36, 48, 255}
	muted := color.RGBA{120, 130, 145, 255}
	titleBaseline := 30 + titleFace.Metrics().Ascent.Ceil()
	drawString(img, titleFace, margin, titleBaseline, fitText(titleFace, title, width-240), ink)
	clock := now.Format("01/02 15:04")
	drawString(img, metaFace, width-margin-textWidth(metaFace, clock), titleBaseline-4, clock, muted)
	cardCount := 0
	for _, group := range groups {
		cardCount += len(group.cards)
	}
	sub := strconv.Itoa(len(groups)) + " 个分组 · " + strconv.Itoa(cardCount) + " 项"
	drawString(img, metaFace, margin, titleBaseline+12+metaFace.Metrics().Ascent.Ceil(), sub, muted)

	cardW := boardCardW
	contentW := width - margin*2
	y := headH
	for _, group := range groups {
		if len(group.cards) == 0 {
			continue
		}
		drawSectionHead(img, titleFace, margin, y, contentW, group.name)
		y += boardSectionH
		for i, card := range group.cards {
			col := i % cols
			row := i / cols
			x := margin + col*(cardW+gap)
			drawBoardCard(img, nameFace, metaFace, x, y+row*(cardH+gap), cardW, cardH, radius, card)
		}
		rows := (len(group.cards) + cols - 1) / cols
		y += rows*cardH + (rows-1)*gap + gap
	}
	if cardCount == 0 {
		drawString(img, nameFace, margin, headH+36, "暂无分组", muted)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func drawSectionHead(img *image.RGBA, titleFace font.Face, x, y, width int, name string) {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名分组"
	}
	ink := color.RGBA{28, 36, 48, 255}
	barH := 36
	barY := y + (boardSectionH-barH)/2
	fillRoundRect(img, image.Rect(x, barY, x+8, barY+barH), 4, ink)
	ascent := titleFace.Metrics().Ascent.Ceil()
	textH := ascent + titleFace.Metrics().Descent.Ceil()
	baseline := barY + (barH-textH)/2 + ascent
	drawString(img, titleFace, x+22, baseline, fitText(titleFace, name, width-40), ink)
	fillRoundRect(img, image.Rect(x, y+boardSectionH-8, x+width, y+boardSectionH-5), 1, color.RGBA{210, 218, 230, 255})
}

func drawBoardCard(img *image.RGBA, nameFace, metaFace font.Face, x, y, w, h, radius int, card boardCard) {
	fillRoundRect(img, image.Rect(x, y, x+w, y+h), radius, color.RGBA{255, 255, 255, 255})
	strokeRoundRect(img, image.Rect(x, y, x+w, y+h), radius, color.RGBA{226, 232, 240, 255})

	label, bg, fg := badgeStyle(card.status)
	badgeW := textWidth(metaFace, label) + 28
	badgeH := 32
	badgeX := x + w - 16 - badgeW
	badgeY := y + 18
	fillRoundRect(img, image.Rect(badgeX, badgeY, badgeX+badgeW, badgeY+badgeH), 16, bg)
	drawString(img, metaFace, badgeX+14, badgeY+6+metaFace.Metrics().Ascent.Ceil(), label, fg)

	ratio := ""
	if card.ratio != "" {
		ratio = "[" + card.ratio + "]"
	}
	ratioW := 0
	if ratio != "" {
		ratioW = textWidth(metaFace, ratio) + 10
	}
	nameMax := badgeX - (x + 18) - ratioW - 8
	if nameMax < 40 {
		nameMax = 40
	}
	nameY := y + 22 + nameFace.Metrics().Ascent.Ceil()
	drawString(img, nameFace, x+18, nameY, fitText(nameFace, card.name, nameMax), color.RGBA{28, 36, 48, 255})
	if ratio != "" {
		nameShown := fitText(nameFace, card.name, nameMax)
		drawString(img, metaFace, x+18+textWidth(nameFace, nameShown)+10, y+24+metaFace.Metrics().Ascent.Ceil(), ratio, color.RGBA{120, 130, 145, 255})
	}

	rateLabel := "成功率 "
	rateY := y + 72 + metaFace.Metrics().Ascent.Ceil()
	drawString(img, metaFace, x+18, rateY, rateLabel, color.RGBA{120, 130, 145, 255})
	drawString(img, metaFace, x+18+textWidth(metaFace, rateLabel), rateY, card.rate, color.RGBA{28, 36, 48, 255})

	drawDots(img, x+18, y+h-34, card.dots)
}

func badgeStyle(status string) (string, color.RGBA, color.RGBA) {
	switch status {
	case "up":
		return "正常", color.RGBA{220, 252, 231, 255}, color.RGBA{21, 128, 61, 255}
	case "degraded":
		return "降级", color.RGBA{254, 243, 199, 255}, color.RGBA{180, 83, 9, 255}
	case "down":
		return "异常", color.RGBA{254, 226, 226, 255}, color.RGBA{185, 28, 28, 255}
	default:
		return "无采样", color.RGBA{241, 245, 249, 255}, color.RGBA{100, 116, 139, 255}
	}
}

func drawDots(img *image.RGBA, x, y int, dots []int) {
	colors := []color.RGBA{
		{239, 68, 68, 255},
		{245, 185, 66, 255},
		{34, 197, 94, 255},
	}
	gray := color.RGBA{226, 232, 240, 255}
	for i, level := range dots {
		col := gray
		if level >= 0 && level < len(colors) {
			col = colors[level]
		}
		fillCircle(img, x+8+i*22, y+8, 7, col)
	}
}

func drawString(img *image.RGBA, face font.Face, x, y int, s string, col color.Color) {
	d := font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(col),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func textWidth(face font.Face, s string) int {
	return font.MeasureString(face, s).Ceil()
}

func fitText(face font.Face, s string, maxWidth int) string {
	if s == "" || textWidth(face, s) <= maxWidth {
		return s
	}
	runes := []rune(s)
	for len(runes) > 0 {
		runes = runes[:len(runes)-1]
		candidate := string(runes) + "…"
		if textWidth(face, candidate) <= maxWidth {
			return candidate
		}
	}
	return "…"
}

func fillRoundRect(img *image.RGBA, rect image.Rectangle, radius int, col color.RGBA) {
	if radius < 1 {
		radius = 1
	}
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			if roundCover(x, y, rect, radius) > 0.5 {
				blend(img, x, y, col, 1)
			}
		}
	}
}

func strokeRoundRect(img *image.RGBA, rect image.Rectangle, radius int, col color.RGBA) {
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		for x := rect.Min.X; x < rect.Max.X; x++ {
			cover := roundCover(x, y, rect, radius)
			inner := roundCover(x, y, image.Rect(rect.Min.X+1, rect.Min.Y+1, rect.Max.X-1, rect.Max.Y-1), radius-1)
			edge := cover - inner
			if edge > 0 {
				blend(img, x, y, col, edge)
			}
		}
	}
}

func roundCover(x, y int, rect image.Rectangle, radius int) float64 {
	if radius < 0 {
		radius = 0
	}
	dx, dy := 0, 0
	if x < rect.Min.X+radius {
		dx = rect.Min.X + radius - x
	} else if x >= rect.Max.X-radius {
		dx = x - (rect.Max.X - radius - 1)
	}
	if y < rect.Min.Y+radius {
		dy = rect.Min.Y + radius - y
	} else if y >= rect.Max.Y-radius {
		dy = y - (rect.Max.Y - radius - 1)
	}
	if dx == 0 && dy == 0 {
		if image.Pt(x, y).In(rect) {
			return 1
		}
		return 0
	}
	cover := float64(radius) + 0.5 - math.Hypot(float64(dx), float64(dy))
	if cover <= 0 {
		return 0
	}
	if cover > 1 {
		return 1
	}
	return cover
}

func fillCircle(img *image.RGBA, cx, cy, r int, col color.RGBA) {
	for y := cy - r - 1; y <= cy+r+1; y++ {
		for x := cx - r - 1; x <= cx+r+1; x++ {
			cover := float64(r) + 0.5 - math.Hypot(float64(x-cx), float64(y-cy))
			if cover <= 0 {
				continue
			}
			if cover > 1 {
				cover = 1
			}
			blend(img, x, y, col, cover)
		}
	}
}

func blend(img *image.RGBA, x, y int, src color.RGBA, cover float64) {
	if cover <= 0 || !image.Pt(x, y).In(img.Bounds()) {
		return
	}
	i := img.PixOffset(x, y)
	dstR := float64(img.Pix[i])
	dstG := float64(img.Pix[i+1])
	dstB := float64(img.Pix[i+2])
	a := cover
	img.Pix[i] = uint8(dstR*(1-a) + float64(src.R)*a)
	img.Pix[i+1] = uint8(dstG*(1-a) + float64(src.G)*a)
	img.Pix[i+2] = uint8(dstB*(1-a) + float64(src.B)*a)
	img.Pix[i+3] = 255
}

var (
	boardFontOnce sync.Once
	boardFont     *opentype.Font
	boardFontErr  error
)

func boardFaces() (font.Face, font.Face, font.Face, error) {
	f, err := loadBoardFont()
	if err != nil {
		return nil, nil, nil, err
	}
	title, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 34, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		return nil, nil, nil, err
	}
	name, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 24, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		closeFace(title)
		return nil, nil, nil, err
	}
	meta, err := opentype.NewFace(f, &opentype.FaceOptions{Size: 18, DPI: 72, Hinting: font.HintingNone})
	if err != nil {
		closeFace(title)
		closeFace(name)
		return nil, nil, nil, err
	}
	return title, name, meta, nil
}

func closeFace(face font.Face) {
	if c, ok := face.(interface{ Close() error }); ok {
		_ = c.Close()
	}
}

func loadBoardFont() (*opentype.Font, error) {
	boardFontOnce.Do(func() {
		path, err := boardFontPath()
		if err != nil {
			boardFontErr = err
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			boardFontErr = err
			return
		}
		boardFont, boardFontErr = parseBoardFont(data)
	})
	return boardFont, boardFontErr
}

func parseBoardFont(data []byte) (*opentype.Font, error) {
	if col, err := opentype.ParseCollection(data); err == nil && col.NumFonts() > 0 {
		if f, err := col.Font(0); err == nil {
			return f, nil
		}
	}
	if f, err := opentype.Parse(data); err == nil {
		return f, nil
	}
	fixed, err := alignedSFNT(data)
	if err != nil {
		return nil, err
	}
	return opentype.Parse(fixed)
}

// alignedSFNT 取出字体集里的第一个字体，按 4 字节对齐重排各张表。
// wqy-zenhei 等字体的表偏移没有对齐，x/image 会拒绝解析。
func alignedSFNT(data []byte) ([]byte, error) {
	bad := errors.New("字体文件格式不支持")
	u16 := func(at int) int { return int(data[at])<<8 | int(data[at+1]) }
	u32 := func(at int) int {
		return int(data[at])<<24 | int(data[at+1])<<16 | int(data[at+2])<<8 | int(data[at+3])
	}
	if len(data) < 12 {
		return nil, bad
	}
	base := 0
	if string(data[:4]) == "ttcf" {
		if len(data) < 16 || u32(8) < 1 {
			return nil, bad
		}
		base = u32(12)
	}
	if base < 0 || base+12 > len(data) {
		return nil, bad
	}
	numTables := u16(base + 4)
	dirEnd := base + 12 + numTables*16
	if numTables == 0 || numTables > 200 || dirEnd > len(data) {
		return nil, bad
	}
	type table struct {
		rec       []byte
		off, size int
	}
	tables := make([]table, 0, numTables)
	for i := 0; i < numTables; i++ {
		at := base + 12 + i*16
		off, size := u32(at+8), u32(at+12)
		if off < 0 || size < 0 || off+size > len(data) {
			return nil, bad
		}
		tables = append(tables, table{rec: data[at : at+16], off: off, size: size})
	}
	sort.Slice(tables, func(i, j int) bool { return string(tables[i].rec[:4]) < string(tables[j].rec[:4]) })

	headLen := 12 + numTables*16
	total := headLen
	for _, t := range tables {
		total += (t.size + 3) &^ 3
	}
	out := make([]byte, total)
	copy(out, data[base:base+12])
	pos := headLen
	for i, t := range tables {
		rec := out[12+i*16 : 12+i*16+16]
		copy(rec, t.rec[:8])
		rec[8], rec[9], rec[10], rec[11] = byte(pos>>24), byte(pos>>16), byte(pos>>8), byte(pos)
		copy(rec[12:], t.rec[12:16])
		copy(out[pos:], data[t.off:t.off+t.size])
		pos += (t.size + 3) &^ 3
	}
	return out, nil
}

func boardFontPath() (string, error) {
	var candidates []string
	if windir := os.Getenv("WINDIR"); windir != "" {
		candidates = append(candidates, filepath.Join(windir, "Fonts", "msyh.ttc"))
	}
	candidates = append(candidates,
		`C:\Windows\Fonts\msyh.ttc`,
		`C:\Windows\Fonts\simhei.ttf`,
		"/usr/share/fonts/wenquanyi/wqy-zenhei/wqy-zenhei.ttc",
		"/usr/share/fonts/wqy-zenhei/wqy-zenhei.ttc",
		"/usr/share/fonts/truetype/wqy/wqy-zenhei.ttc",
	)
	for _, p := range candidates {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
	}
	found := ""
	_ = filepath.Walk("/usr/share/fonts", func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || found != "" {
			return nil
		}
		low := strings.ToLower(info.Name())
		if strings.Contains(low, "wqy") || strings.Contains(low, "noto") || strings.Contains(low, "cjk") {
			if strings.HasSuffix(low, ".ttc") || strings.HasSuffix(low, ".ttf") || strings.HasSuffix(low, ".otf") {
				found = path
				return filepath.SkipAll
			}
		}
		return nil
	})
	if found != "" {
		return found, nil
	}
	return "", errors.New("缺少中文字体，无法生成状态图")
}
