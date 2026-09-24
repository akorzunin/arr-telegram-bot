package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

type rig struct {
	t              *testing.T
	server         *httptest.Server
	bot            *Bot
	calls          []string
	messages       []string
	failSend       bool
	historyMovie   []map[string]any
	historySeries  []map[string]any
	existingMovie  []map[string]any
	existingSeries []map[string]any
	added          []map[string]any
	episodes       []map[string]any
}

func setup(t *testing.T) *rig {
	t.Helper()
	r := &rig{t: t, episodes: []map[string]any{{"id": 41, "seasonNumber": 2}, {"id": 42, "seasonNumber": 1}}}
	r.server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.server.Close)
	r.bot = newBot(Config{Token: "test-token", Allowed: map[int64]bool{7: true}, StateDir: t.TempDir(), TelegramURL: r.server.URL, Movie: AppConfig{URL: r.server.URL, Key: "secret", Profile: 1, Root: "/movies"}, Series: AppConfig{URL: r.server.URL, Key: "series-secret", Profile: 2, Root: "/series"}}, r.server.Client())
	if err := r.bot.load(); err != nil {
		t.Fatal(err)
	}
	return r
}
func (r *rig) serve(w http.ResponseWriter, q *http.Request) {
	path := q.URL.Path
	r.calls = append(r.calls, q.Method+" "+path+"?"+q.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")
	var body map[string]any
	_ = json.NewDecoder(q.Body).Decode(&body)
	reply := any(map[string]any{})
	switch {
	case path == "/bottest-token/sendMessage":
		if r.failSend {
			w.WriteHeader(500)
			return
		}
		r.messages = append(r.messages, fmt.Sprint(body["text"]))
		reply = map[string]any{"ok": true, "result": map[string]any{"message_id": 1}}
	case path == "/bottest-token/answerCallbackQuery":
		reply = map[string]any{"ok": true, "result": true}
	case path == "/bottest-token/getUpdates":
		reply = map[string]any{"ok": true, "result": []any{}}
	case path == "/api/v3/movie/lookup":
		reply = []map[string]any{{"title": "Film", "year": 2024, "tmdbId": 21}}
	case path == "/api/v3/series/lookup":
		reply = []map[string]any{{"title": "Show", "year": 2023, "tvdbId": 31, "titleSlug": "show", "genres": []string{"Anime"}, "seasons": []map[string]any{{"seasonNumber": 1}, {"seasonNumber": 2}}}}
	case path == "/api/v3/movie" && q.Method == "GET":
		reply = r.existingMovie
	case path == "/api/v3/series" && q.Method == "GET":
		reply = r.existingSeries
	case path == "/api/v3/movie" && q.Method == "POST":
		r.added = append(r.added, body)
		reply = map[string]any{"id": 81}
	case path == "/api/v3/series" && q.Method == "POST":
		r.added = append(r.added, body)
		reply = map[string]any{"id": 82}
	case path == "/api/v3/series/91" && q.Method == "PUT":
		r.added = append(r.added, body)
		reply = body
	case path == "/api/v3/episode":
		reply = r.episodes
	case path == "/api/v3/episode/monitor":
		r.added = append(r.added, body)
		reply = body
	case strings.HasPrefix(path, "/api/v3/episode/") && q.Method == "GET":
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "/api/v3/episode/"))
		for _, ep := range r.episodes {
			if number(ep["id"]) == id {
				reply = ep
				break
			}
		}
	case path == "/api/v3/command":
		r.added = append(r.added, body)
		reply = body
	case path == "/api/v3/history" && q.Header.Get("X-Api-Key") == "secret":
		reply = map[string]any{"records": r.historyMovie}
	case path == "/api/v3/history" && q.Header.Get("X-Api-Key") == "series-secret":
		reply = map[string]any{"records": r.historySeries}
	default:
		r.t.Errorf("unexpected %s %s", q.Method, path)
		w.WriteHeader(404)
		return
	}
	_ = json.NewEncoder(w).Encode(reply)
}
func msg(text string) Update {
	return Update{ID: 10, Message: &Message{From: User{ID: 7}, Chat: Chat{ID: 70, Type: "private"}, Text: text}}
}
func (r *rig) action(t *testing.T, action string) {
	t.Helper()
	var key string
	for k := range r.bot.pending {
		key = k
		break
	}
	if key == "" {
		t.Fatal("no pending selection")
	}
	q := Update{ID: 11, Callback: &Callback{ID: "callback", From: User{ID: 7}, Message: Message{Chat: Chat{ID: 70, Type: "private"}}, Data: action + ":" + key}}
	if err := r.bot.handle(q); err != nil {
		t.Fatal(err)
	}
}
func hasCall(calls []string, frag string) bool {
	for _, c := range calls {
		if strings.Contains(c, frag) {
			return true
		}
	}
	return false
}

func TestMovieRequestConfirmation(t *testing.T) {
	r := setup(t)
	if err := r.bot.handle(msg("/movie Film")); err != nil {
		t.Fatal(err)
	}
	if len(r.added) != 0 || !hasCall(r.calls, "movie/lookup?term=Film") {
		t.Fatalf("search calls %v added %v", r.calls, r.added)
	}
	r.action(t, "pick")
	if len(r.added) != 0 || !strings.Contains(r.messages[len(r.messages)-1], "Request Film") {
		t.Fatalf("premature add: %v", r.messages)
	}
	r.action(t, "add")
	if len(r.added) != 1 || r.added[0]["tmdbId"] != float64(21) || r.added[0]["minimumAvailability"] != "released" {
		t.Fatalf("payload: %#v", r.added)
	}
	if len(r.bot.state.Requests) != 1 || r.bot.state.Requests[0].ItemID != 81 {
		t.Fatalf("tracking: %#v", r.bot.state.Requests)
	}
}
func TestExistingSeasonRequest(t *testing.T) {
	r := setup(t)
	r.existingSeries = []map[string]any{{"id": 91, "tvdbId": 31, "monitored": false, "seasons": []map[string]any{{"seasonNumber": 1, "monitored": false}, {"seasonNumber": 2, "monitored": false}}}}
	if err := r.bot.handle(msg("Show season 2")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "add")
	if !hasCall(r.calls, "PUT /api/v3/series/91") || !hasCall(r.calls, "PUT /api/v3/episode/monitor") || !hasCall(r.calls, "POST /api/v3/command") {
		t.Fatalf("calls: %v", r.calls)
	}
	if len(r.bot.state.Requests) != 1 || r.bot.state.Requests[0].Season != 2 || r.bot.state.Requests[0].ItemID != 91 {
		t.Fatalf("tracking: %#v", r.bot.state.Requests)
	}
	if r.added[0]["monitorNewItems"] != "none" {
		t.Fatalf("updated: %v", r.added[0])
	}
}
func TestExistingMovieUnchanged(t *testing.T) {
	r := setup(t)
	r.existingMovie = []map[string]any{{"id": 77, "tmdbId": 21}}
	if err := r.bot.handle(msg("/movie Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "add")
	if len(r.added) != 0 || len(r.bot.state.Requests) != 0 {
		t.Fatalf("existing altered: %v %#v", r.added, r.bot.state.Requests)
	}
}
func TestPrivateAllowlistExpiryAndCancel(t *testing.T) {
	r := setup(t)
	m := msg("Film")
	m.Message.Chat.Type = "group"
	_ = r.bot.handle(m)
	m.Message.Chat.Type = "private"
	m.Message.From.ID = 8
	_ = r.bot.handle(m)
	if len(r.calls) != 0 {
		t.Fatalf("unauthorized calls: %v", r.calls)
	}
	_ = r.bot.handle(msg("/series Show S02"))
	if len(r.bot.pending) != 1 {
		t.Fatal("missing selection")
	}
	for _, v := range r.bot.pending {
		v.Expires = time.Now().Add(-time.Second)
	}
	r.action(t, "add")
	if len(r.added) != 0 {
		t.Fatal("expired request added")
	}
	_ = r.bot.handle(msg("/series Show S02"))
	r.action(t, "cancel")
	if len(r.bot.pending) != 0 {
		t.Fatal("cancel failed")
	}
}
func TestPreexistingImportNotAttributedToNewRequest(t *testing.T) {
	r := setup(t)
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	r.historyMovie = []map[string]any{{"id": 500, "eventType": "downloadFolderImported", "movieId": 81}}
	if err := r.bot.handle(msg("/movie Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "add")
	n := len(r.messages)
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != n {
		t.Fatalf("old import misattributed: %v", r.messages[n:])
	}
}
func TestImportedNotificationDedupeAndRestart(t *testing.T) {
	r := setup(t)
	r.historyMovie = []map[string]any{{"id": 100, "eventType": "downloadFolderImported", "movieId": 81}}
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	} // baseline old unrelated event
	if err := r.bot.handle(msg("/movie Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "add")
	r.historyMovie = append(r.historyMovie, map[string]any{"id": 101, "eventType": "downloadFolderImported", "movieId": 81})
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "imported") {
		t.Fatalf("no import notice: %v", r.messages)
	}
	count := len(r.messages)
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != count {
		t.Fatal("duplicate notice")
	}
	clone := newBot(r.bot.cfg, r.server.Client())
	if err := clone.load(); err != nil {
		t.Fatal(err)
	}
	r.bot = clone
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != count {
		t.Fatal("duplicate after restart")
	}
}
func TestFailedNotificationRetriedAfterRestart(t *testing.T) {
	r := setup(t)
	_ = r.bot.pollHistory()
	_ = r.bot.handle(msg("/movie Film"))
	r.action(t, "pick")
	r.action(t, "add")
	r.historyMovie = []map[string]any{{"id": 201, "eventType": "downloadFolderImported", "movieId": 81}}
	r.failSend = true
	if err := r.bot.pollHistory(); err == nil {
		t.Fatal("failed send returned nil")
	}
	clone := newBot(r.bot.cfg, r.server.Client())
	if err := clone.load(); err != nil {
		t.Fatal(err)
	}
	r.bot = clone
	r.failSend = false
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "imported") {
		t.Fatalf("retry absent %v", r.messages)
	}
}
func TestNewSeriesImportUsesEpisodeSeason(t *testing.T) {
	r := setup(t)
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.handle(msg("/series Show S02")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "add")
	r.historySeries = []map[string]any{{"id": 401, "eventType": "downloadFolderImported", "seriesId": 82, "episodeId": 42}, {"id": 402, "eventType": "downloadFolderImported", "seriesId": 82, "episodeId": 41}}
	n := len(r.messages)
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != n+1 {
		t.Fatalf("new series missing import: %v", r.messages[n:])
	}
}
func TestConfigRequiresCredentialsAndPositiveProfiles(t *testing.T) {
	t.Setenv("TELEGRAM_BOT_TOKEN", " ")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("empty token accepted")
	}
	t.Setenv("TELEGRAM_BOT_TOKEN", "abc")
	t.Setenv("RADARR_API_KEY", "rk")
	t.Setenv("SONARR_API_KEY", "sk")
	t.Setenv("RADARR_QUALITY_PROFILE_ID", "1")
	t.Setenv("SONARR_QUALITY_PROFILE_ID", "2")
	t.Setenv("RADARR_URL", "http://radarr:7878")
	t.Setenv("SONARR_URL", "http://sonarr:8989")
	t.Setenv("RADARR_ROOT", "/movies")
	t.Setenv("SONARR_ROOT", "/tv")
	t.Setenv("TELEGRAM_ALLOWED_USERS", "7, 8")
	cfg, err := configFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Allowed[7] || !cfg.Allowed[8] || cfg.StateDir != "/state" {
		t.Fatalf("cfg %#v", cfg)
	}
}
func TestPollingUsesOffsetAndCheckpoints(t *testing.T) {
	r := setup(t)
	r.bot.state.Offset = 77
	if err := r.bot.pollUpdates(); err != nil {
		t.Fatal(err)
	}
	if !hasCall(r.calls, "/bottest-token/getUpdates") {
		t.Fatalf("not polled: %v", r.calls)
	}
}
func TestSeriesEpisodeFilterAndPersistentOffset(t *testing.T) {
	r := setup(t)
	r.existingSeries = []map[string]any{{"id": 91, "tvdbId": 31, "seasons": []map[string]any{{"seasonNumber": 1}, {"seasonNumber": 2}}}}
	_ = r.bot.pollHistory()
	_ = r.bot.handle(msg("/series Show S02"))
	r.action(t, "pick")
	r.action(t, "add")
	r.historySeries = []map[string]any{{"id": 301, "eventType": "downloadFolderImported", "seriesId": 91, "episodeId": 42}, {"id": 302, "eventType": "downloadFolderImported", "seriesId": 91, "episodeId": 41}}
	n := len(r.messages)
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != n+1 {
		t.Fatalf("series filter got %v", r.messages[n:])
	}
	if err := r.bot.process(Update{ID: 88, Message: &Message{From: User{ID: 9}, Chat: Chat{ID: 90, Type: "private"}, Text: "x"}}); err != nil {
		t.Fatal(err)
	}
	clone := newBot(r.bot.cfg, r.server.Client())
	if err := clone.load(); err != nil {
		t.Fatal(err)
	}
	if clone.state.Offset != 89 {
		t.Fatalf("offset %d", clone.state.Offset)
	}
	if _, err := os.Stat(filepath.Join(r.bot.cfg.StateDir, "offset")); err != nil {
		t.Fatal(err)
	}
}
