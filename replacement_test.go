package main

import (
	"strings"
	"testing"
)

func russianRelease() map[string]any {
	return map[string]any{
		"title":   "The Boy and the Heron [Movie] [RUS(int), ENG, JAP] [2023, BDRemux] [1080p]",
		"indexer": "RuTracker.org (Prowlarr)", "indexerId": 3, "guid": "release-guid",
		"rejected": true, "downloadAllowed": true, "customFormatScore": 100, "seasonNumber": 2,
		"rejections": []any{"Custom Formats ARR Original Audio have score 100 below Movie's profile minimum 10000", "Existing file on disk has a equal or higher Custom Format score: 100"},
	}
}

func TestRussianParserFallbackFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(map[string]any)
		want   bool
	}{
		{"internal audio", func(r map[string]any) {}, true},
		{"language order", func(r map[string]any) { r["title"] = "Film [ENG, RUS(int), JAP] [2023]" }, true},
		{"external dub", func(r map[string]any) { r["title"] = "Film [RUS(ext), JAP]" }, false},
		{"subtitles only", func(r map[string]any) { r["title"] = "Film [JAP] [Sub RUS]" }, false},
		{"generic Russian", func(r map[string]any) { r["title"] = "Russian Film MULTI" }, false},
		{"different indexer", func(r map[string]any) { r["indexer"] = "Nyaa.si (Prowlarr)" }, false},
		{"below seeder minimum", func(r map[string]any) {
			r["rejections"] = append(r["rejections"].([]any), "Not enough seeders: 0. Minimum seeders: 5")
		}, false},
		{"quality downgrade", func(r map[string]any) {
			r["rejections"] = append(r["rejections"].([]any), "Existing file on disk is of equal or higher preference: Remux-1080p v1")
		}, false},
		{"quality cutoff", func(r map[string]any) {
			r["rejections"] = append(r["rejections"].([]any), "Existing file on disk meets quality cutoff: Bluray-1080p")
		}, false},
		{"wrong title", func(r map[string]any) {
			r["rejections"] = append(r["rejections"].([]any), "Unknown Movie. Unable to match to correct movie using release title.")
		}, false},
		{"size", func(r map[string]any) {
			r["rejections"] = append(r["rejections"].([]any), "30 GB is larger than maximum allowed")
		}, false},
		{"blocked", func(r map[string]any) { r["rejections"] = append(r["rejections"].([]any), "Release is blocklisted") }, false},
		{"unknown rejection", func(r map[string]any) { r["rejections"] = append(r["rejections"].([]any), "New ARR rejection") }, false},
		{"wrong profile minimum", func(r map[string]any) {
			r["rejections"] = []any{"Custom Formats ARR Original Audio have score 100 below Movie's profile minimum 20000"}
		}, false},
		{"no missing language score", func(r map[string]any) {
			r["rejections"] = []any{"Existing file on disk has a equal or higher Custom Format score: 100"}
		}, false},
		{"already Russian scored", func(r map[string]any) { r["customFormatScore"] = 10100 }, false},
		{"other custom format penalty", func(r map[string]any) {
			r["customFormatScore"] = -100
			r["rejections"] = []any{"Custom Formats Unwanted Audio have score -100 below Movie's profile minimum 10000"}
		}, false},
		{"other score requirement", func(r map[string]any) {
			r["rejections"] = []any{"Custom Formats Preferred Edition have score 100 below Movie's profile minimum 10000"}
		}, false},
		{"not downloadable", func(r map[string]any) { r["downloadAllowed"] = false }, false},
		{"missing rejections", func(r map[string]any) { delete(r, "rejections") }, false},
		{"invalid rejections", func(r map[string]any) { r["rejections"] = []any{nil} }, false},
		{"missing guid", func(r map[string]any) { delete(r, "guid") }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := russianRelease()
			tc.change(r)
			if got := russianParserFallback(r); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func confirmRussianReplacement(t *testing.T, r *rig) {
	t.Helper()
	enableLanguages(r)
	downloadedMovie(r, "eng/jpn")
	if err := r.bot.handle(msg("/language Film")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "pick")
	r.action(t, "profile-russian")
	r.action(t, "apply")
}

func TestRussianReplacementGrabsLanguageOnlyFallback(t *testing.T) {
	r := setup(t)
	r.releases = []map[string]any{russianRelease()}
	confirmRussianReplacement(t, r)
	if hasCall(r.calls, "GET /api/v3/release") || len(r.added) != 0 {
		t.Fatal("search before replacement consent")
	}
	r.action(t, "replace")
	if !hasCall(r.calls, "GET /api/v3/release?movieId=81") || !hasCall(r.calls, "POST /api/v3/release") || hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal(r.calls)
	}
	grab := r.added[len(r.added)-1]
	if grab["guid"] != "release-guid" || number(grab["movieId"]) != 81 || len(grab) != 3 {
		t.Fatal("overrode ARR cached mapping", grab)
	}
	if len(r.bot.pending) != 0 || !strings.Contains(r.messages[len(r.messages)-1], "replacement grabbed") {
		t.Fatal(r.messages)
	}
	if err := r.bot.pollLanguages(); err != nil {
		t.Fatal(err)
	}
	if !r.bot.state.Requests[0].SubsApplied {
		t.Fatal("lost subtitle search")
	}
}

func TestRussianReplacementNoSafeCandidateReportsNoDownload(t *testing.T) {
	r := setup(t)
	bad := russianRelease()
	bad["rejections"] = append(bad["rejections"].([]any), "Not enough seeders: 0. Minimum seeders: 5")
	r.releases = []map[string]any{bad}
	confirmRussianReplacement(t, r)
	r.action(t, "replace")
	if hasCall(r.calls, "POST /api/v3/release") || hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal("grabbed rejected release")
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "No download was started") {
		t.Fatal(r.messages)
	}
}

func TestRussianReplacementPrefersNormalARRSearch(t *testing.T) {
	r := setup(t)
	r.releases = []map[string]any{russianRelease(), {"rejected": false, "downloadAllowed": true}}
	confirmRussianReplacement(t, r)
	r.action(t, "replace")
	if hasCall(r.calls, "POST /api/v3/release") || !hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal(r.calls)
	}
}

func TestRussianFallbackGrabFailureConsumesConsent(t *testing.T) {
	r := setup(t)
	r.releases = []map[string]any{russianRelease()}
	r.failGrab = true
	confirmRussianReplacement(t, r)
	var key string
	for k := range r.bot.pending {
		key = k
	}
	v := r.bot.pending[key]
	if err := r.bot.languageCallback("replace", key, v); err == nil {
		t.Fatal("expected grab failure")
	}
	if len(r.bot.pending) != 0 || hasCall(r.calls, "POST /api/v3/command") {
		t.Fatal("retryable uncertain download")
	}
}

func TestRussianSeasonFallbackIsScoped(t *testing.T) {
	for _, season := range []int{1, 2} {
		r := setup(t)
		enableLanguages(r)
		release := russianRelease()
		release["seasonNumber"] = season
		release["rejections"] = []any{"Custom Formats ARR Original Audio have score 100 below Series's profile minimum 10000"}
		r.releases = []map[string]any{release}
		status, err := r.bot.russianReplacement(&Selection{Kind: "series", Season: 2, Item: map[string]any{"id": 91}})
		if err != nil {
			t.Fatal(err)
		}
		if !hasCall(r.calls, "GET /api/v3/release?seriesId=91&seasonNumber=2") {
			t.Fatal(r.calls)
		}
		if hasCall(r.calls, "POST /api/v3/release") != (season == 2) {
			t.Fatal("wrong season", status, r.calls)
		}
	}
}
