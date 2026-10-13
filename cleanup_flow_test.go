package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCleanupOnlyAdminsCanDeleteWhileDownloadsStayPublic(t *testing.T) {
	r := setup(t)
	r.bot.cfg.Allowed = map[int64]bool{}
	r.bot.cfg.CleanupAllowed = map[int64]bool{7: true}
	if err := r.bot.handle(Update{Message: &Message{From: User{8}, Chat: Chat{80, "private"}, Text: "Film"}}); err != nil {
		t.Fatal(err)
	}
	if len(r.bot.pending) == 0 {
		t.Fatal("public download search was denied")
	}
	for _, v := range r.bot.pending {
		if v.User != 8 {
			t.Fatal("wrong requester")
		}
	}

	c := newCleanupRig(t)
	c.b.cfg.Allowed = map[int64]bool{}
	c.b.cfg.CleanupAllowed = map[int64]bool{7: true}
	if err := c.b.browseCleanup(80, 8, "Film"); err != nil {
		t.Fatal(err)
	}
	key := c.pickKey(t, "movie", -1)
	if err := c.click(key, "cleanpick", 8, 80); err != nil {
		t.Fatal(err)
	}
	if c.b.pending[key].Confirmed {
		t.Fatal("nonadmin got delete confirmation")
	}
	// Even a forged/replayed confirmation must be checked at the destructive boundary.
	c.b.pending[key].Confirmed = true
	if err := c.click(key, "cleanremove", 8, 80); err != nil {
		t.Fatal(err)
	}
	c.assertNoDeletion(t)
	delete(c.b.pending, key)
	if err := c.message("/cleanup Film"); err != nil {
		t.Fatal(err)
	}
	key = c.pickKey(t, "movie", -1)
	if err := c.click(key, "cleanpick", 7, 70); err != nil {
		t.Fatal(err)
	}
	if err := c.click(key, "cleanremove", 7, 70); err != nil {
		t.Fatal(err)
	}
	if c.movie["hasFile"] != false {
		t.Fatal("cleanup admin was denied")
	}
}

func TestCleanupPaginationAndUnknownDates(t *testing.T) {
	r := newCleanupRig(t)
	scopes := []CleanupScope{}
	for i := 0; i < 10; i++ {
		scopes = append(scopes, CleanupScope{Kind: "movie", ItemID: 81, Season: -1, Label: "Film"})
	}
	key, v, err := r.b.cleanupSelection(70, 7)
	if err != nil {
		t.Fatal(err)
	}
	v.CleanupList = scopes
	if err := r.b.cleanupPage(key, v); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(field(r.sent[len(r.sent)-1], "text"), "page 1/2") {
		t.Fatal(r.sent)
	}
	if err := r.click(key, "cleannext", 7, 70); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(field(r.sent[len(r.sent)-1], "text"), "page 2/2") {
		t.Fatal(r.sent)
	}
	if err := r.click(key, "cleannext", 7, 70); err != nil {
		t.Fatal(err)
	}
	if v.CleanupPage != 1 {
		t.Fatal("paged beyond end")
	}
	if err := r.click(key, "cleanprev", 7, 70); err != nil {
		t.Fatal(err)
	}
	if v.CleanupPage != 0 {
		t.Fatal("previous page ignored")
	}
	delete(r.movie["movieFile"].(map[string]any), "dateAdded")
	if err := r.message("/list"); err != nil {
		t.Fatal(err)
	}
	text := field(r.sent[len(r.sent)-1], "text")
	if !strings.Contains(text, "unknown date") || strings.Index(text, "Show — S02") > strings.Index(text, "Film") {
		t.Fatal("unknown date not last: " + text)
	}
	r.assertNoDeletion(t)
}

func TestCleanupDirectPathAndWholeSeriesPackProof(t *testing.T) {
	t.Run("torrent points to imported file without history", func(t *testing.T) {
		r := newCleanupRig(t)
		r.historyMovie = nil
		r.torrents[0]["downloadDir"] = "/downloads/films"
		r.torrents[0]["files"] = []map[string]any{{"name": "Film/Film.mkv"}}
		p, err := r.b.cleanupPlan(CleanupScope{Kind: "movie", ItemID: 81, Season: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Torrents) != 1 {
			t.Fatal("missed imported-path torrent")
		}
	})
	t.Run("whole series can remove a proven mixed season pack", func(t *testing.T) {
		r := newCleanupRig(t)
		r.historySeries[1]["downloadId"] = "hash-s1"
		r.torrents = r.torrents[:2]
		r.torrents[1]["files"] = []map[string]any{{"name": "S01E01.mkv"}, {"name": "S02E01.mkv"}}
		p, err := r.b.cleanupPlan(CleanupScope{Kind: "series", ItemID: 91, Season: -1})
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Torrents) != 1 {
			t.Fatal("pack proof failed")
		}
	})
}

func TestCleanupPendingBlocksReRequest(t *testing.T) {
	r := newCleanupRig(t)
	r.b.cfg.PreparationDir = t.TempDir()
	_ = prepareDirs(r.b.cfg.PreparationDir)
	r.movie["tmdbId"] = 21
	r.b.state.Cursor = map[string]int{"movie": 0, "series": 0}
	r.b.state.Cleanups = []CleanupTask{{ID: "0123456789abcdef", Scope: CleanupScope{Kind: "movie", ItemID: 81}, Output: "films/Film"}}
	if err := r.b.add(&Selection{Kind: "movie", Item: map[string]any{"tmdbId": 21}, Chat: 70, Season: 1, Expires: time.Now().Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(field(r.sent[len(r.sent)-1], "text"), "still pending") {
		t.Fatal("pending cleanup not respected")
	}
	r.assertNoDeletion(t)
}

func TestWorkerCleanupRefreshFailureIsRetryable(t *testing.T) {
	queue, output := t.TempDir(), t.TempDir()
	_ = prepareDirs(queue)
	task := CleanupTask{ID: "0123456789abcdef", Scope: CleanupScope{Kind: "movie", ItemID: 81}, Output: "films/Film"}
	_ = writeJSON(filepath.Join(queue, "cleanup", task.ID+".json"), task)
	_ = os.MkdirAll(filepath.Join(output, task.Output), 0755)
	fail := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if fail {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(204)
		}
	}))
	defer server.Close()
	w := &Worker{Queue: queue, Output: output, JellyfinURL: server.URL, Client: server.Client()}
	if err := w.cleanupTick(); err == nil {
		t.Fatal("refresh failure ignored")
	}
	if _, err := os.Stat(filepath.Join(queue, "cleanup-done", task.ID)); !os.IsNotExist(err) {
		t.Fatal("marked complete without refresh")
	}
	fail = false
	if err := w.cleanupTick(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(queue, "cleanup-done", task.ID)); err != nil {
		t.Fatal(err)
	}
	if _, err := cleanupDestination(filepath.Join(output, "missing-mount"), task.Output); err == nil {
		t.Fatal("accepted missing output mount")
	}
}
