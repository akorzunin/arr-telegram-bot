package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type cleanupRig struct {
	b                                                             *Bot
	movie, series                                                 map[string]any
	files, episodes, historyMovie, historySeries, torrents, queue []map[string]any
	calls                                                         []string
	sent                                                          []map[string]any
	rpc                                                           []map[string]any
	failDelete                                                    bool
}

func newCleanupRig(t *testing.T) *cleanupRig {
	t.Helper()
	r := &cleanupRig{}
	r.movie = map[string]any{"id": 81, "title": "Film", "path": "/downloads/films/Film", "hasFile": true, "monitored": true, "movieFile": map[string]any{"id": 9, "relativePath": "Film.mkv", "path": "/downloads/films/Film/Film.mkv", "dateAdded": "2024-02-01T00:00:00Z", "size": float64(2 << 30)}}
	r.series = map[string]any{"id": 91, "title": "Show", "path": "/downloads/shows/Show", "monitored": true, "seasons": []map[string]any{{"seasonNumber": 1, "monitored": true}, {"seasonNumber": 2, "monitored": true}}}
	r.files = []map[string]any{{"id": 11, "seasonNumber": 1, "path": "/downloads/shows/Show/Season 1/S01E01.mkv", "dateAdded": "2023-01-01T00:00:00Z", "size": float64(1 << 30)}, {"id": 12, "seasonNumber": 2, "path": "/downloads/shows/Show/Season 2/S02E01.mkv", "dateAdded": "2025-01-01T00:00:00Z", "size": float64(3 << 30)}}
	r.episodes = []map[string]any{{"id": 41, "seasonNumber": 1, "episodeFileId": 11}, {"id": 42, "seasonNumber": 2, "episodeFileId": 12}}
	r.historyMovie = []map[string]any{{"movieId": 81, "eventType": "downloadFolderImported", "downloadId": "hash-film", "data": map[string]any{"droppedPath": "/downloads/incoming/Film/Film.mkv"}}}
	r.historySeries = []map[string]any{{"seriesId": 91, "episodeId": 41, "eventType": "downloadFolderImported", "downloadId": "hash-s1", "data": map[string]any{"droppedPath": "/downloads/incoming/S01E01.mkv"}}, {"seriesId": 91, "episodeId": 42, "eventType": "downloadFolderImported", "downloadId": "hash-s2", "data": map[string]any{"droppedPath": "/downloads/incoming/S02E01.mkv"}}}
	r.torrents = []map[string]any{{"hashString": "hash-film", "downloadDir": "/downloads/incoming", "files": []map[string]any{{"name": "Film/Film.mkv"}, {"name": "Film/Film.nfo"}}}, {"hashString": "hash-s1", "downloadDir": "/downloads/incoming", "files": []map[string]any{{"name": "S01E01.mkv"}}}, {"hashString": "hash-s2", "downloadDir": "/downloads/incoming", "files": []map[string]any{{"name": "S02E01.mkv"}}}}
	server := httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(server.Close)
	r.b = newBot(Config{Token: "test", Allowed: map[int64]bool{7: true}, StateDir: t.TempDir(), TelegramURL: server.URL, TransmissionURL: server.URL + "/rpc", Movie: AppConfig{URL: server.URL, Key: "movie", Root: "/downloads/films"}, Series: AppConfig{URL: server.URL, Key: "series", Root: "/downloads/shows"}, MediaRoot: "/downloads"}, server.Client())
	return r
}
func (r *cleanupRig) serve(w http.ResponseWriter, q *http.Request) {
	r.calls = append(r.calls, q.Method+" "+q.URL.Path)
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	_ = json.NewDecoder(q.Body).Decode(&body)
	var out any = map[string]any{}
	switch {
	case q.URL.Path == "/bottest/sendMessage":
		r.sent = append(r.sent, body)
		out = map[string]any{"ok": true}
	case q.URL.Path == "/bottest/answerCallbackQuery":
		out = map[string]any{"ok": true}
	case q.URL.Path == "/api/v3/movie":
		out = []map[string]any{r.movie}
	case q.URL.Path == "/api/v3/series":
		out = []map[string]any{r.series}
	case q.URL.Path == "/api/v3/movie/81":
		if q.Method == "PUT" {
			r.movie = body
		}
		out = r.movie
	case q.URL.Path == "/api/v3/series/91":
		if q.Method == "PUT" {
			r.series = body
		}
		out = r.series
	case q.URL.Path == "/api/v3/episode":
		out = r.episodes
	case q.URL.Path == "/api/v3/episodefile":
		out = r.files
	case q.URL.Path == "/api/v3/episode/monitor":
		for _, ep := range r.episodes {
			if containsInts(body["episodeIds"], number(ep["id"])) {
				ep["monitored"] = body["monitored"]
			}
		}
	case q.URL.Path == "/api/v3/moviefile/9" && q.Method == "DELETE":
		if r.failDelete {
			w.WriteHeader(500)
			return
		}
		delete(r.movie, "movieFile")
		r.movie["hasFile"] = false
	case strings.HasPrefix(q.URL.Path, "/api/v3/episodefile/") && q.Method == "DELETE":
		var id int
		fmt.Sscanf(q.URL.Path, "/api/v3/episodefile/%d", &id)
		next := []map[string]any{}
		for _, f := range r.files {
			if number(f["id"]) != id {
				next = append(next, f)
			}
		}
		r.files = next
	case q.URL.Path == "/api/v3/queue":
		out = map[string]any{"records": r.queue, "totalRecords": len(r.queue)}
	case q.URL.Path == "/api/v3/history":
		records := r.historyMovie
		if q.Header.Get("X-Api-Key") == "series" {
			records = r.historySeries
		}
		out = map[string]any{"records": records}
	case q.URL.Path == "/rpc":
		if q.Header.Get("X-Transmission-Session-Id") != "session" {
			w.Header().Set("X-Transmission-Session-Id", "session")
			w.WriteHeader(409)
			return
		}
		r.rpc = append(r.rpc, body)
		out = map[string]any{"result": "success", "arguments": map[string]any{"torrents": r.torrents}}
		if body["method"] == "torrent-remove" {
			args, _ := body["arguments"].(map[string]any)
			ids, _ := args["ids"].([]any)
			next := []map[string]any{}
			for _, torrent := range r.torrents {
				found := false
				for _, id := range ids {
					if id == torrent["hashString"] {
						found = true
					}
				}
				if !found {
					next = append(next, torrent)
				}
			}
			r.torrents = next
		}
	default:
		w.WriteHeader(404)
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}
func containsInts(v any, n int) bool {
	a, _ := v.([]any)
	for _, x := range a {
		if number(x) == n {
			return true
		}
	}
	return false
}
func (r *cleanupRig) message(text string) error {
	return r.b.handle(Update{Message: &Message{From: User{7}, Chat: Chat{70, "private"}, Text: text}})
}
func (r *cleanupRig) click(key, action string, user, chat int64) error {
	return r.b.handle(Update{Callback: &Callback{ID: "cb", From: User{user}, Message: Message{Chat: Chat{chat, "private"}}, Data: action + ":" + key}})
}
func (r *cleanupRig) pickKey(t *testing.T, kind string, season int) string {
	t.Helper()
	for k, v := range r.b.pending {
		if v.Cleanup != nil && v.Cleanup.Scope.Kind == kind && v.Cleanup.Scope.Season == season {
			return k
		}
	}
	t.Fatal("missing selection")
	return ""
}
func (r *cleanupRig) assertNoDeletion(t *testing.T) {
	t.Helper()
	for _, c := range r.calls {
		if strings.HasPrefix(c, "DELETE ") || strings.HasPrefix(c, "PUT ") {
			t.Fatalf("mutated ARR: %s", c)
		}
	}
	for _, call := range r.rpc {
		if call["method"] == "torrent-remove" {
			t.Fatal("removed torrent")
		}
	}
}

func TestListSortedAndNameSeasonFilter(t *testing.T) {
	r := newCleanupRig(t)
	if err := r.message("/list"); err != nil {
		t.Fatal(err)
	}
	text := field(r.sent[len(r.sent)-1], "text")
	s1, movie, s2 := strings.Index(text, "Show — S01"), strings.Index(text, "Film"), strings.Index(text, "Show — S02")
	if s1 < 0 || !(s1 < movie && movie < s2) {
		t.Fatal(text)
	}
	if !strings.Contains(text, "2.00 GiB") {
		t.Fatal(text)
	}
	if err := r.message("/cleanup show S02"); err != nil {
		t.Fatal(err)
	}
	r.pickKey(t, "series", 2)
	for _, v := range r.b.pending {
		if v.Cleanup != nil && v.Cleanup.Scope.Season != 2 {
			t.Fatal("filter ignored")
		}
	}
	r.assertNoDeletion(t)
}
func TestCleanupRequiresBoundConfirmation(t *testing.T) {
	r := newCleanupRig(t)
	if err := r.message("/cleanup Film"); err != nil {
		t.Fatal(err)
	}
	key := r.pickKey(t, "movie", -1)
	if err := r.click(key, "cleanremove", 7, 70); err != nil {
		t.Fatal(err)
	}
	r.assertNoDeletion(t)
	if err := r.click(key, "cleanpick", 7, 70); err != nil {
		t.Fatal(err)
	}
	if !r.b.pending[key].Confirmed {
		t.Fatal("not confirmed")
	}
	for _, pair := range [][2]int64{{8, 70}, {7, 71}} {
		_ = r.click(key, "cleanremove", pair[0], pair[1])
	}
	r.assertNoDeletion(t)
	r.b.pending[key].Expires = time.Now().Add(-time.Second)
	_ = r.click(key, "cleanremove", 7, 70)
	r.assertNoDeletion(t)
}
func TestCleanupMovieDeletesAllKnownCopies(t *testing.T) {
	r := newCleanupRig(t)
	queue := t.TempDir()
	if err := prepareDirs(queue); err != nil {
		t.Fatal(err)
	}
	r.b.cfg.PreparationDir = queue
	job := PrepareJob{ID: strings.Repeat("a", 64), Output: "films/Film/Film.mp4"}
	if err := writeJSON(filepath.Join(queue, "requests", job.ID+".json"), job); err != nil {
		t.Fatal(err)
	}
	r.b.state.Requests = []RequestRecord{{Kind: "movie", ItemID: 81, Chat: 70}, {Kind: "movie", ItemID: 81, Chat: 71}, {Kind: "series", ItemID: 91, Season: 1, Chat: 70}}
	if err := r.message("/cleanup Film"); err != nil {
		t.Fatal(err)
	}
	key := r.pickKey(t, "movie", -1)
	if err := r.click(key, "cleanpick", 7, 70); err != nil {
		t.Fatal(err)
	}
	if err := r.click(key, "cleanremove", 7, 70); err != nil {
		t.Fatal(err)
	}
	if r.movie["monitored"] != false || r.movie["hasFile"] != false {
		t.Fatal(r.movie)
	}
	if !r.b.state.Requests[0].Cancelled || !r.b.state.Requests[1].Cancelled || r.b.state.Requests[2].Cancelled {
		t.Fatal("wrong scope cancelled")
	}
	removed := false
	for _, call := range r.rpc {
		if call["method"] == "torrent-remove" {
			args := call["arguments"].(map[string]any)
			if args["delete-local-data"] != true {
				t.Fatal("kept data")
			}
			removed = true
		}
	}
	if !removed || len(r.torrents) != 2 {
		t.Fatal("wrong torrents")
	}
	if _, err := os.Stat(filepath.Join(queue, "cancel", job.ID)); err != nil {
		t.Fatal(err)
	}
	if len(r.b.state.Cleanups) != 1 || !r.b.cleanupPending("movie", 81, 1) {
		t.Fatal("cleanup not tracked")
	}
	task := r.b.state.Cleanups[0]
	if task.Output != "films/Film" {
		t.Fatal(task)
	}
	if _, err := os.Stat(filepath.Join(queue, "cleanup", task.ID+".json")); err != nil {
		t.Fatal(err)
	}
	// Replays cannot repeat destructive calls.
	n := len(r.rpc)
	_ = r.click(key, "cleanremove", 7, 70)
	if len(r.rpc) != n {
		t.Fatal("replayed")
	}
}
func TestCleanupSeasonPreservesOtherSeason(t *testing.T) {
	r := newCleanupRig(t)
	r.b.state.Requests = []RequestRecord{{Kind: "series", ItemID: 91, Season: 1, Chat: 70}, {Kind: "series", ItemID: 91, Season: 2, Chat: 70}}
	if err := r.message("/cleanup Show S01"); err != nil {
		t.Fatal(err)
	}
	key := r.pickKey(t, "series", 1)
	if err := r.click(key, "cleanpick", 7, 70); err != nil {
		t.Fatal(err)
	}
	if err := r.click(key, "cleanremove", 7, 70); err != nil {
		t.Fatal(err)
	}
	if len(r.files) != 1 || number(r.files[0]["id"]) != 12 {
		t.Fatal(r.files)
	}
	seasons := seasons(r.series)
	if seasons[0]["monitored"] != false || seasons[1]["monitored"] != true || r.series["monitored"] != true {
		t.Fatal(r.series)
	}
	if r.episodes[0]["monitored"] != false || r.episodes[1]["monitored"] == false {
		t.Fatal(r.episodes)
	}
	if !r.b.state.Requests[0].Cancelled || r.b.state.Requests[1].Cancelled {
		t.Fatal("cancelled other season")
	}
	if len(r.torrents) != 2 {
		t.Fatal("wrong torrent scope")
	}
}
func TestCleanupWholeSeries(t *testing.T) {
	r := newCleanupRig(t)
	if err := r.message("/cleanup Show all"); err != nil {
		t.Fatal(err)
	}
	key := r.pickKey(t, "series", -1)
	if err := r.click(key, "cleanpick", 7, 70); err != nil {
		t.Fatal(err)
	}
	if err := r.click(key, "cleanremove", 7, 70); err != nil {
		t.Fatal(err)
	}
	if len(r.files) != 0 || r.series["monitored"] != false || r.series["monitorNewItems"] != "none" || len(r.torrents) != 1 {
		t.Fatal("whole scope not deleted")
	}
}
func TestCleanupSafetyRefusals(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*cleanupRig)
		scope  CleanupScope
	}{
		{"unproven pack video", func(r *cleanupRig) {
			r.torrents[0]["files"] = []map[string]any{{"name": "Film/Film.mkv"}, {"name": "Other.mkv"}}
		}, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
		{"shared history hash", func(r *cleanupRig) { r.historySeries[0]["downloadId"] = "hash-film" }, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
		{"mixed season", func(r *cleanupRig) { r.historySeries[1]["downloadId"] = "hash-s1" }, CleanupScope{Kind: "series", ItemID: 91, Season: 1}},
		{"shared torrent paths", func(r *cleanupRig) {
			r.torrents = append(r.torrents, map[string]any{"hashString": "other", "downloadDir": "/downloads/incoming", "files": []map[string]any{{"name": "Film/Film.mkv"}}})
		}, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
		{"outside media root", func(r *cleanupRig) { r.torrents[0]["downloadDir"] = "/elsewhere" }, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
		{"traversal", func(r *cleanupRig) { r.torrents[0]["files"] = []map[string]any{{"name": "../Film.mkv"}} }, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
		{"queue active", func(r *cleanupRig) { r.queue = []map[string]any{{"movieId": 81}} }, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
		{"episode shared across seasons", func(r *cleanupRig) { r.episodes[1]["episodeFileId"] = 11 }, CleanupScope{Kind: "series", ItemID: 91, Season: 1}},
		{"unsafe imported path", func(r *cleanupRig) { r.movie["path"] = "/downloads/films" }, CleanupScope{Kind: "movie", ItemID: 81, Season: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newCleanupRig(t)
			tc.mutate(r)
			if _, err := r.b.cleanupPlan(tc.scope); err == nil {
				t.Fatal("unsafe cleanup accepted")
			}
			r.assertNoDeletion(t)
		})
	}
}
func TestCleanupRevalidatesBeforeDeleting(t *testing.T) {
	for _, change := range []string{"file", "torrent path"} {
		t.Run(change, func(t *testing.T) {
			r := newCleanupRig(t)
			_ = r.message("/cleanup Film")
			key := r.pickKey(t, "movie", -1)
			if err := r.click(key, "cleanpick", 7, 70); err != nil {
				t.Fatal(err)
			}
			if change == "file" {
				r.movie["movieFile"].(map[string]any)["id"] = 10
			} else {
				r.torrents[0]["files"] = []map[string]any{{"name": "Film/Film.mkv"}, {"name": "Film/new.nfo"}}
			}
			if err := r.click(key, "cleanremove", 7, 70); err != nil {
				t.Fatal(err)
			}
			r.assertNoDeletion(t)
			if !strings.Contains(field(r.sent[len(r.sent)-1], "text"), "changed") {
				t.Fatal(r.sent)
			}
		})
	}
}
func TestCleanupAllowlistAndCancel(t *testing.T) {
	r := newCleanupRig(t)
	r.b.cfg.Allowed = map[int64]bool{}
	if err := r.message("/cleanup Film"); err != nil {
		t.Fatal(err)
	}
	key := r.pickKey(t, "movie", -1)
	_ = r.click(key, "cleanpick", 7, 70)
	_ = r.click(key, "cleanremove", 7, 70)
	r.assertNoDeletion(t)
	if !strings.Contains(field(r.sent[len(r.sent)-1], "text"), "restricted to cleanup admins") {
		t.Fatal(r.sent)
	}
	r.b.cfg.Allowed[7] = true
	_ = r.message("/cleanup Film")
	key = r.pickKey(t, "movie", -1)
	_ = r.click(key, "cleanpick", 7, 70)
	_ = r.click(key, "cancel", 7, 70)
	r.assertNoDeletion(t)
}
func TestCleanupPartialFailureRetry(t *testing.T) {
	r := newCleanupRig(t)
	r.failDelete = true
	_ = r.message("/cleanup Film")
	key := r.pickKey(t, "movie", -1)
	_ = r.click(key, "cleanpick", 7, 70)
	if err := r.click(key, "cleanremove", 7, 70); err == nil {
		t.Fatal("failure swallowed")
	}
	if len(r.torrents) != 2 || r.movie["monitored"] != false || r.movie["hasFile"] != true {
		t.Fatal("unexpected partial state")
	}
	r.failDelete = false
	_ = r.message("/cleanup Film")
	key = r.pickKey(t, "movie", -1)
	_ = r.click(key, "cleanpick", 7, 70)
	if err := r.click(key, "cleanremove", 7, 70); err != nil {
		t.Fatal(err)
	}
	if r.movie["hasFile"] != false {
		t.Fatal("retry failed")
	}
}
func TestCleanupDurablePublicationAndPendingScope(t *testing.T) {
	r := newCleanupRig(t)
	r.b.cfg.PreparationDir = t.TempDir()
	_ = prepareDirs(r.b.cfg.PreparationDir)
	task := CleanupTask{ID: "0123456789abcdef", Scope: CleanupScope{Kind: "series", ItemID: 91, Season: 1, Label: "Show S01"}, Output: "shows/Show/Season 01"}
	r.b.state.Cleanups = []CleanupTask{task}
	if err := r.b.save(); err != nil {
		t.Fatal(err)
	}
	if err := r.b.load(); err != nil {
		t.Fatal(err)
	}
	if err := r.b.restoreCleanupTasks(); err != nil {
		t.Fatal(err)
	}
	if !r.b.cleanupPending("series", 91, -1) || r.b.cleanupPending("series", 91, 2) {
		t.Fatal("pending overlap incorrect")
	}
	if err := atomicWrite(filepath.Join(r.b.cfg.PreparationDir, "cleanup-done", task.ID), []byte("complete")); err != nil {
		t.Fatal(err)
	}
	if r.b.cleanupPending("series", 91, 1) {
		t.Fatal("completion ignored")
	}
	if err := r.message("/list"); err != nil {
		t.Fatal(err)
	}
	if len(r.b.state.Cleanups) != 0 {
		t.Fatal("receipt not pruned")
	}
}

func TestWorkerCleanupSerializedAndIdempotent(t *testing.T) {
	queue, output := t.TempDir(), t.TempDir()
	_ = prepareDirs(queue)
	task := CleanupTask{ID: "0123456789abcdef", Scope: CleanupScope{Kind: "series", ItemID: 91, Season: 1}, Output: "shows/Show/Season 01"}
	job := PrepareJob{ID: strings.Repeat("a", 64), Output: task.Output + "/S01E01.mp4"}
	_ = writeJSON(filepath.Join(queue, "requests", job.ID+".json"), job)
	_ = writeJSON(filepath.Join(queue, "cleanup", task.ID+".json"), task)
	for _, path := range []string{job.Output, task.Output + "/S01E01.en.srt", "shows/Show/Season 02/S02E01.mp4", ".staging/" + job.ID + "/unfinished.mp4"} {
		full := filepath.Join(output, path)
		_ = os.MkdirAll(filepath.Dir(full), 0755)
		_ = os.WriteFile(full, []byte("video"), 0600)
	}
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		if q.URL.Path != "/Library/Refresh" {
			t.Error(q.URL.Path)
		}
		refreshes++
		w.WriteHeader(204)
	}))
	defer server.Close()
	w := &Worker{Queue: queue, Output: output, JellyfinURL: server.URL, Client: server.Client()}
	if err := w.tick(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(output, task.Output)); !os.IsNotExist(err) {
		t.Fatal("optimized season retained")
	}
	if _, err := os.Stat(filepath.Join(output, ".staging", job.ID)); !os.IsNotExist(err) {
		t.Fatal("staging retained")
	}
	if _, err := os.Stat(filepath.Join(output, "shows/Show/Season 02/S02E01.mp4")); err != nil {
		t.Fatal("deleted other season")
	}
	if _, err := os.Stat(filepath.Join(queue, "cleanup-done", task.ID)); err != nil {
		t.Fatal(err)
	}
	// A subsequent deliberate new output must not be removed by old receipts.
	_ = os.MkdirAll(filepath.Join(output, task.Output), 0755)
	_ = os.WriteFile(filepath.Join(output, job.Output), []byte("new"), 0600)
	if err := w.tick(); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatal("replayed cleanup")
	}
	if _, err := os.Stat(filepath.Join(output, job.Output)); err != nil {
		t.Fatal("deleted new output")
	}
}
func TestWorkerCleanupRejectsUnsafeOutput(t *testing.T) {
	for _, path := range []string{"films", "shows", "../outside", "shows/Show/Season 02"} {
		t.Run(path, func(t *testing.T) {
			queue, output := t.TempDir(), t.TempDir()
			_ = prepareDirs(queue)
			task := CleanupTask{ID: "0123456789abcdef", Scope: CleanupScope{Kind: "series", ItemID: 91, Season: 1}, Output: path}
			_ = writeJSON(filepath.Join(queue, "cleanup", task.ID+".json"), task)
			w := &Worker{Queue: queue, Output: output}
			if err := w.cleanupTick(); err == nil {
				t.Fatal("unsafe output allowed")
			}
		})
	}
	root, outside := t.TempDir(), t.TempDir()
	_ = os.MkdirAll(filepath.Join(root, "shows"), 0755)
	_ = os.Symlink(outside, filepath.Join(root, "shows", "Show"))
	if _, err := cleanupDestination(root, "shows/Show/Season 01"); err == nil {
		t.Fatal("followed parent symlink")
	}
}
