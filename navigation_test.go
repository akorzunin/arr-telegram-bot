package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testMagnet = "magnet:?xt=urn:btih:045948F960A646EECF3F5321A2CB0B7D2AF1AE23&dn=Nobody%202021%201080p"

func requestFixture(r *rig) {
	r.bot.state.Requests = []RequestRecord{{Kind: "movie", ItemID: 81, Chat: 70, Label: "Film"}}
	r.existingMovie = []map[string]any{{"id": 81, "title": "Film", "hasFile": false}}
}

func TestRequestIDLanguageBeforeDownload(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	requestFixture(r)
	if err := r.bot.handle(msg("/language 1")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "profile-russian")
	r.action(t, "apply")
	if len(r.added) != 1 || number(r.added[0]["qualityProfileId"]) != r.bot.cfg.Movie.RussianProfile || r.bot.state.Requests[0].Language != "russian" {
		t.Fatal(r.added, r.messages)
	}
	if hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal("unexpected search")
	}
}

func TestIDsCannotCrossChatsOrFallBackToTitle(t *testing.T) {
	r := setup(t)
	requestFixture(r)
	r.bot.state.Requests[0].Chat = 999
	for _, q := range []string{"1", "#1", "81"} {
		if r.bot.itemMatches(70, q, "movie", r.existingMovie[0]) {
			t.Fatal("numeric query fell back", q)
		}
	}
	if !r.bot.itemMatches(70, "movie:81", "movie", r.existingMovie[0]) || r.bot.itemMatches(70, "series:81", "movie", r.existingMovie[0]) {
		t.Fatal("explicit ARR selector")
	}
	if err := r.bot.handle(msg("/set 1 " + testMagnet)); err != nil {
		t.Fatal(err)
	}
	if len(r.bot.pending) != 0 || len(r.transmission) > 0 {
		t.Fatal("foreign request accessed")
	}
}

func TestMenuAndDurableItemButtons(t *testing.T) {
	r := setup(t)
	requestFixture(r)
	if err := r.bot.handle(msg("/start")); err != nil {
		t.Fatal(err)
	}
	if r.telegramBodies[0]["reply_markup"] == nil {
		t.Fatal("no keyboard")
	}
	if err := r.bot.handle(msg("My requests")); err != nil {
		t.Fatal(err)
	}
	u := Update{Callback: &Callback{ID: "x", From: User{ID: 7}, Message: Message{Chat: Chat{ID: 70, Type: "private"}}, Data: "item:1"}}
	if err := r.bot.handle(u); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "#1 Film") {
		t.Fatal(r.messages)
	}
	u.Callback.Data = "magnet:1"
	if err := r.bot.handle(u); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.handle(msg(testMagnet)); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "bypasses indexer") {
		t.Fatal(r.messages)
	}
	if len(r.transmission) > 0 {
		t.Fatal("added without confirmation")
	}
}

func TestMagnetValidation(t *testing.T) {
	for _, link := range []string{testMagnet, "magnet:?xt=urn:btih:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"} {
		if _, err := parseMagnet(link); err != nil {
			t.Fatal(err)
		}
	}
	for _, link := range []string{"https://example.com/torrent", "magnet:?xt=urn:btih:bad", "magnet:?dn=film", "magnet:?xt=urn:btih:" + strings.Repeat("z", 40), "magnet:?xt=urn:btih:" + strings.Repeat("0", 40) + "&dn=%zz"} {
		if _, err := parseMagnet(link); err == nil {
			t.Fatal(link)
		}
	}
}

func TestManualMagnetConfirmationAndImport(t *testing.T) {
	r := setup(t)
	requestFixture(r)
	hash, _ := parseMagnet(testMagnet)
	r.transmissionReply = map[string]any{"torrent-added": map[string]any{"hashString": hash}}
	if err := r.bot.handle(msg("/set 1 " + testMagnet)); err != nil {
		t.Fatal(err)
	}
	if len(r.transmission) > 0 {
		t.Fatal("premature download")
	}
	r.action(t, "magnetadd")
	if len(r.transmission) != 1 || r.bot.state.Requests[0].ManualHash != hash {
		t.Fatal(r.transmission, r.messages)
	}
	restarted := newBot(r.bot.cfg, r.server.Client())
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	r.bot = restarted
	r.transmissionReply = map[string]any{"torrents": []map[string]any{{"name": "Unparseable title", "downloadDir": "/downloads/complete", "percentDone": 1}}}
	r.manualFiles = []map[string]any{{"path": "/downloads/complete/Unparseable title/video.mkv", "quality": map[string]any{"quality": map[string]any{"id": 7}}, "languages": []map[string]any{{"id": 1}}, "rejections": []any{}}}
	if err := r.bot.pollMagnets(); err != nil {
		t.Fatal(err)
	}
	if !r.bot.state.Requests[0].ManualImport || len(r.added) != 1 || r.added[0]["name"] != "ManualImport" || r.added[0]["importMode"] != "copy" {
		t.Fatal(r.added, r.messages)
	}
	if len(r.manualReprocess) != 1 || number(r.manualReprocess[0]["movieId"]) != 81 || r.manualReprocess[0]["downloadId"] != nil {
		t.Fatal(r.manualReprocess)
	}
	for _, call := range r.calls {
		if strings.Contains(call, "/manualimport?") && strings.HasPrefix(call, "GET") && (strings.Contains(call, "downloadId") || strings.Contains(call, "movieId")) {
			t.Fatal(call)
		}
	}
	files := maps(r.added[0]["files"])
	if len(files) != 1 || number(files[0]["movieId"]) != 81 || files[0]["downloadId"] != hash {
		t.Fatal(files)
	}
	if err := r.bot.pollMagnets(); err != nil {
		t.Fatal(err)
	}
	if len(r.added) != 1 {
		t.Fatal("duplicate import")
	}
}

func TestManualMagnetRejectsDuplicateAndAmbiguousPack(t *testing.T) {
	r := setup(t)
	requestFixture(r)
	r.transmissionReply = map[string]any{"torrent-duplicate": map[string]any{"hashString": "existing"}}
	if err := r.bot.handle(msg("/set 1 " + testMagnet)); err != nil {
		t.Fatal(err)
	}
	r.action(t, "magnetadd")
	if r.bot.state.Requests[0].ManualHash != "" {
		t.Fatal("claimed existing torrent")
	}
	r.bot.state.Requests[0].ManualHash = strings.Repeat("a", 40)
	r.transmissionReply = map[string]any{"torrents": []map[string]any{{"name": "Pack", "downloadDir": "/downloads/complete", "percentDone": 1}}}
	r.manualFiles = []map[string]any{{"path": "a.mkv"}, {"path": "b.mkv"}}
	if err := r.bot.pollMagnets(); err != nil {
		t.Fatal(err)
	}
	if len(r.added) > 0 || !strings.Contains(r.messages[len(r.messages)-1], "ambiguous") {
		t.Fatal(r.added, r.messages)
	}
}

func TestFFmpegGPUFailureIsSafeAndActionable(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\necho '/private/title.mkv CUDA_ERROR_NO_DEVICE: no CUDA-capable device is detected' >&2\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	w := &Worker{}
	err := w.runFFmpeg(context.Background(), PrepareJob{}, nil, 1, &PrepareResult{})
	if err == nil || !strings.Contains(err.Error(), "recreate") || strings.Contains(err.Error(), "private") {
		t.Fatal(err)
	}
	var tail ffmpegErrors
	_, _ = tail.Write([]byte(strings.Repeat("a", 20000)))
	_, _ = tail.Write([]byte("end"))
	if len(tail) > 16384 || !strings.HasSuffix(string(tail), "end") {
		t.Fatal(len(tail))
	}
}
