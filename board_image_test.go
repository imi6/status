package main

import (
	"bytes"
	"image/png"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestChunkBoardCards(t *testing.T) {
	if pages := chunkBoardCards(nil, 10); len(pages) != 1 || len(pages[0]) != 0 {
		t.Fatalf("empty %#v", pages)
	}
	cards := make([]boardCard, 11)
	pages := chunkBoardCards(cards, 10)
	if len(pages) != 2 || len(pages[0]) != 10 || len(pages[1]) != 1 {
		t.Fatalf("pages %d %d", len(pages), len(pages[0]))
	}
	if ratioTag("0.08") != "0.08x" || ratioTag("1.5倍") != "1.5倍" || ratioTag("  ") != "" {
		t.Fatal(ratioTag("0.08"))
	}
}

func TestRenderStatusBoard(t *testing.T) {
	now := time.Date(2026, 10, 15, 23, 24, 0, 0, time.Local)
	hour := make([]TimelineBucket, 24)
	hour[23] = TimelineBucket{Samples: 10, Up: 10, Rate: 100}
	sections := []Section{{Name: "GPT", Rows: []GroupResp{
		{Name: "plus", SubName: "GPT Plus", Status: "up", Ratio: "0.09", Timelines: map[string][]TimelineBucket{
			"probe": {
				{Samples: 1, Up: 1, Rate: 100},
				{Samples: 1, Up: 0, Rate: 0},
			},
		}},
		{Name: "pro", SubName: "GPT Pro", Status: "degraded", Timelines: map[string][]TimelineBucket{"hour": hour}},
	}}}
	pngs, err := renderStatusBoard("吱吱鼠AI监控", sections, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pngs) != 1 {
		t.Fatalf("pages %d", len(pngs))
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(pngs[0]))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Width != 1120 || cfg.Height < 200 {
		t.Fatalf("size %dx%d", cfg.Width, cfg.Height)
	}
	cards := buildBoardCards(sections)
	if cards[0].ratio != "0.09x" || cards[0].rate != "50%" || cards[0].dots[10] != 2 || cards[0].dots[11] != 0 {
		t.Fatalf("card %+v", cards[0])
	}
}

func TestSendTelegramPhoto(t *testing.T) {
	var gotChat, gotCaption, filename string
	var gotPNG bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/sendPhoto") || strings.Contains(r.URL.Path, "123:abc") == false {
			t.Fatalf("path %s", r.URL.Path)
		}
		mediaType, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || !strings.HasPrefix(mediaType, "multipart/") {
			t.Fatal(err)
		}
		mr := multipart.NewReader(r.Body, params["boundary"])
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Fatal(err)
			}
			body, _ := io.ReadAll(part)
			switch part.FormName() {
			case "chat_id":
				gotChat = string(body)
			case "caption":
				gotCaption = string(body)
			case "photo":
				filename = part.FileName()
				gotPNG = len(body) > 8 && string(body[:8]) == "\x89PNG\r\n\x1a\n"
			}
		}
		w.Header().Set("content-type", "application/json")
		io.WriteString(w, `{"ok":true}`)
	}))
	defer srv.Close()
	old := telegramAPI
	telegramAPI = srv.URL
	t.Cleanup(func() { telegramAPI = old })
	pngBytes := []byte("\x89PNG\r\n\x1a\nrest")
	if err := sendTelegramPhoto("123:abc", "-1001", pngBytes, "监控 zzshu.si"); err != nil {
		t.Fatal(err)
	}
	if gotChat != "-1001" || gotCaption != "监控 zzshu.si" || filename != "status.png" || !gotPNG {
		t.Fatalf("chat %q caption %q file %q png %v", gotChat, gotCaption, filename, gotPNG)
	}
}
