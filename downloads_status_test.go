package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func blockedImportRow() map[string]any {
	return map[string]any{
		"id": 101, "movieId": 81, "title": "Film.Release", "status": "completed",
		"size": 1000, "sizeleft": 0, "downloadId": "ABCD",
		"trackedDownloadStatus": "warning", "trackedDownloadState": "importPending",
		"statusMessages": []map[string]any{{"title": "Film.Release", "messages": []any{"Unable to parse file", "Unable to parse file"}}},
	}
}

func TestActiveShowsImportBlockerAndExistingCopy(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.queueMovie = []map[string]any{blockedImportRow()}
	r.bot.cfg.PreparationDir = t.TempDir()
	if err := prepareDirs(r.bot.cfg.PreparationDir); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	r.bot.state.Requests[0].Preparation = []PreparationRef{{ID: id, Label: "Film", Generation: 1}}
	if err := writeJSON(filepath.Join(r.bot.cfg.PreparationDir, "results", id+".json"), PrepareResult{State: "ready", Generation: 1}); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.active(70); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(r.messages, "\n")
	for _, want := range []string{"completed 100%", "awaiting ARR import", "Unable to parse file", "existing streaming copy; pending downloads are not reflected here"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if strings.Count(text, "Unable to parse file") != 1 || len(r.added) != 0 {
		t.Fatal("duplicate warning or listing mutated ARR", text)
	}
}

func TestReplacementQueueGuardExplainsImportBlocker(t *testing.T) {
	r := setup(t)
	confirmRussianReplacement(t, r)
	r.queueMovie = []map[string]any{blockedImportRow()}
	r.action(t, "replace")
	text := r.messages[len(r.messages)-1]
	for _, want := range []string{"Already has an active/pending download", "awaiting ARR import", "Unable to parse file", "another replacement search will not fix"} {
		if !strings.Contains(text, want) {
			t.Fatalf("missing %q: %s", want, text)
		}
	}
	if len(r.added) != 0 || hasCall(r.calls, "GET /api/v3/release") {
		t.Fatal("blocked import triggered another download search")
	}
}

func TestQueueDetailsDoesNotCallImportedOrDownloadingPending(t *testing.T) {
	for _, row := range []map[string]any{
		{"status": "downloading"},
		{"status": "completed", "trackedDownloadState": "imported"},
		{"status": "downloading", "statusMessages": []any{nil, map[string]any{"messages": []any{nil, ""}}}},
	} {
		if details := queueDetails(row); details != "" {
			t.Fatal("spurious import warning", details)
		}
	}
}
