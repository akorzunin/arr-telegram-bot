package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func enableLanguages(r *rig) {
	r.bot.cfg.BazarrURL = r.server.URL
	r.bot.cfg.BazarrKey = "bazarr-secret"
	r.bot.cfg.EnglishSubs = 3
	r.bot.cfg.RussianSubs = 4
	r.bot.cfg.Movie.RussianProfile = 11
	r.bot.cfg.Series.RussianProfile = 12
	r.bazarrSynced = true
}
func downloadedMovie(r *rig, tags string) {
	r.movieDetail = map[string]any{"id": 81, "tmdbId": 21, "title": "Film", "hasFile": true, "qualityProfileId": 1, "monitored": false, "movieFile": map[string]any{"id": 1, "mediaInfo": map[string]any{"audioLanguages": tags}}}
	r.existingMovie = []map[string]any{r.movieDetail}
}
func TestLanguageRequestProfiles(t *testing.T) {
	for _, language := range []string{"original", "russian", "ru-subs"} {
		t.Run(language, func(t *testing.T) {
			r := setup(t)
			enableLanguages(r)
			if err := r.bot.handle(msg("/movie Film")); err != nil {
				t.Fatal(err)
			}
			r.action(t, "pick")
			r.action(t, "lang-"+language)
			if len(r.added) != 0 {
				t.Fatal("premature add")
			}
			r.action(t, "add")
			profile := 1
			subs := 3
			if language == "russian" {
				profile = 11
			}
			if language != "original" {
				subs = 4
			}
			if number(r.added[0]["qualityProfileId"]) != profile {
				t.Fatalf("payload %v", r.added)
			}
			if err := r.bot.pollLanguages(); err != nil {
				t.Fatal(err)
			}
			p := r.bazarrBodies[0]["profileid"].([]any)[0]
			if p != strconv.Itoa(subs) {
				t.Fatalf("subtitle profile %v", p)
			}
			if !r.bot.state.Requests[0].SubsApplied {
				t.Fatal("not persisted")
			}
		})
	}
}
func TestDownloadedRussianRequiresReplacementConsent(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	downloadedMovie(r, "eng")
	if err := r.bot.handle(msg("/language Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "profile-russian")
	if len(r.added) != 0 {
		t.Fatal("changed before confirmation")
	}
	r.action(t, "apply")
	if len(r.added) != 0 || len(r.bot.state.Requests) != 0 {
		t.Fatal("changed without replacement consent")
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "missing or unverified") {
		t.Fatal(r.messages)
	}
	r.action(t, "replace")
	if !hasCall(r.calls, "POST /api/v3/command") || number(r.added[0]["qualityProfileId"]) != 11 {
		t.Fatal(r.added, r.calls)
	}
	if len(r.bot.pending) != 0 {
		t.Fatal("replayable search")
	}
}
func TestDownloadedRequestAlsoRequiresReplacementConsent(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	downloadedMovie(r, "eng")
	if err := r.bot.handle(msg("/movie Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "lang-russian")
	r.action(t, "add")
	if len(r.added) != 0 || hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal("unconfirmed replacement")
	}
	r.action(t, "cancel")
	if len(r.added) != 0 {
		t.Fatal("cancel altered title")
	}
}
func TestDownloadedDualAudioNeedsNoReplacement(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	downloadedMovie(r, "eng/rus")
	if err := r.bot.handle(msg("/language Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "profile-russian")
	r.action(t, "apply")
	if hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal("automatic replacement search")
	}
	if r.added[0]["monitored"] != false {
		t.Fatal("enabled automatic monitoring")
	}
	if err := r.bot.pollLanguages(); err != nil {
		t.Fatal(err)
	}
	if number(r.added[0]["qualityProfileId"]) != 11 {
		t.Fatal(r.added)
	}
}
func TestAdditionalSubtitlesKeepAudioAndProfiles(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	downloadedMovie(r, "eng")
	if err := r.bot.handle(msg("/language Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "subs-ru")
	if len(r.added) != 0 {
		t.Fatal("ARR changed")
	}
	if len(r.bazarrBodies) != 1 || r.bazarrBodies[0]["language"] != "ru" || number(r.bazarrBodies[0]["radarrid"]) != 81 {
		t.Fatal(r.bazarrBodies)
	}
	if err := r.bot.pollLanguages(); err != nil {
		t.Fatal(err)
	}
	if len(r.bazarrBodies) != 1 {
		t.Fatal("changed subtitle profile")
	}
}
func TestAdditionalSubtitlesSkipExistingExternalTracks(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	downloadedMovie(r, "eng")
	r.bazarrRows = []map[string]any{{"radarrId": 81, "subtitles": []map[string]any{{"code2": "ru", "path": "/media/film.ru.srt", "forced": false}}}}
	if err := r.bot.handle(msg("/language Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "subs-ru")
	if len(r.bazarrBodies) != 0 || len(r.added) != 0 {
		t.Fatal("existing subtitle overwritten")
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "Bazarr: 0") {
		t.Fatal(r.messages)
	}
}
func TestSeasonAdditionalSubtitlesAreScoped(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	r.existingSeries = []map[string]any{{"id": 91, "title": "Show"}}
	r.episodeFiles = map[int]map[string]any{1: {"id": 1, "seasonNumber": 2, "mediaInfo": map[string]any{"audioLanguages": "jpn"}}}
	r.episodes = []map[string]any{{"id": 41, "seasonNumber": 2, "hasFile": true}, {"id": 42, "seasonNumber": 1, "hasFile": true}, {"id": 43, "seasonNumber": 2, "hasFile": false}}
	if err := r.bot.handle(msg("/language Show S02")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "subs-en")
	if len(r.bazarrBodies) != 1 || number(r.bazarrBodies[0]["episodeid"]) != 41 {
		t.Fatal(r.bazarrBodies)
	}
	if len(r.added) != 0 {
		t.Fatal("changed ARR")
	}
}
func TestSubtitleProfileWaitsForSyncAndSurvivesRestart(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	r.bazarrSynced = false
	if err := r.bot.recordRequest(RequestRecord{Kind: "movie", ItemID: 81, Chat: 70, Language: "russian"}); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.pollLanguages(); err != nil {
		t.Fatal(err)
	}
	if len(r.bazarrBodies) != 0 || r.bot.state.Requests[0].SubsApplied {
		t.Fatal("accepted nonexistent row")
	}
	restarted := newBot(r.bot.cfg, r.server.Client())
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	r.bot = restarted
	r.bazarrSynced = true
	r.bazarrFail = true
	if err := r.bot.pollLanguages(); err == nil {
		t.Fatal("expected failure")
	}
	if r.bot.state.Requests[0].SubsApplied {
		t.Fatal("failure persisted as success")
	}
	r.bazarrFail = false
	if err := r.bot.pollLanguages(); err != nil {
		t.Fatal(err)
	}
	if !r.bot.state.Requests[0].SubsApplied {
		t.Fatal("not retried")
	}
}
func TestLatestTitlePreferenceWinsWithoutRenumberingRequests(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	for _, rec := range []RequestRecord{{Kind: "series", ItemID: 91, Season: 1, Chat: 70, Language: "russian"}, {Kind: "series", ItemID: 91, Season: 2, Chat: 70, Language: "original"}, {Kind: "series", ItemID: 91, Season: 1, Chat: 70, Language: "ru-subs"}} {
		if err := r.bot.recordLanguageRequest(rec); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.bot.state.Requests) != 2 {
		t.Fatal("duplicated request")
	}
	if err := r.bot.pollLanguages(); err != nil {
		t.Fatal(err)
	}
	if !r.bot.state.Requests[0].SubsApplied || r.bot.state.Requests[1].SubsApplied {
		t.Fatal("older preference won")
	}
	if r.bazarrBodies[0]["profileid"].([]any)[0] != "4" {
		t.Fatal(r.bazarrBodies)
	}
}
func TestReplacementGuardExpiryAndActiveQueue(t *testing.T) {
	r := setup(t)
	enableLanguages(r)
	downloadedMovie(r, "eng")
	if err := r.bot.handle(msg("/language Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "replace")
	if len(r.added) != 0 {
		t.Fatal("unconfirmed replace")
	}
	r.action(t, "pick")
	r.action(t, "profile-russian")
	r.action(t, "apply")
	r.queueMovie = []map[string]any{{"movieId": 81}}
	r.action(t, "replace")
	if len(r.added) != 0 {
		t.Fatal("duplicate search")
	}
	for _, v := range r.bot.pending {
		v.Expires = time.Now().Add(-time.Second)
	}
	r.action(t, "replace")
	if len(r.added) != 0 {
		t.Fatal("expired selection changed title")
	}
}
func TestForcePreparationResetsSharedGeneration(t *testing.T) {
	r := setup(t)
	r.bot.cfg.PreparationDir = t.TempDir()
	if err := prepareDirs(r.bot.cfg.PreparationDir); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	job := PrepareJob{ID: id, Generation: 1}
	if err := writeJSON(filepath.Join(r.bot.cfg.PreparationDir, "requests", id+".json"), job); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		r.bot.state.Requests = append(r.bot.state.Requests, RequestRecord{Preparation: []PreparationRef{{ID: id, Generation: 1, Notified: true}}})
	}
	if err := r.bot.forcePreparation(0); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(filepath.Join(r.bot.cfg.PreparationDir, "requests", id+".json"), &job); err != nil {
		t.Fatal(err)
	}
	if !job.Rebuild || job.Generation == 1 {
		t.Fatal(job)
	}
	for _, r := range r.bot.state.Requests {
		if r.Preparation[0].Generation != job.Generation || r.Preparation[0].Notified {
			t.Fatal(r)
		}
	}
	data, err := os.ReadFile(filepath.Join(r.bot.cfg.StateDir, "notifications.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state State
	if err := json.Unmarshal(data, &state); err != nil {
		t.Fatal(err)
	}
	if state.Requests[0].Preparation[0].Generation != job.Generation {
		t.Fatal("not persisted")
	}
}
