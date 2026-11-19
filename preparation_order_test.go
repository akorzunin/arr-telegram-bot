package main

import (
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
)

func TestPreparationDiscoversEpisodesInOrder(t *testing.T) {
	r := prepareRig(t)
	dir := filepath.Join(r.bot.cfg.Series.Root, "Show")
	r.seriesDetail = map[string]any{"id": 91, "path": dir}
	r.episodes = nil
	r.episodeFiles = map[int]map[string]any{}
	// ARR order, episode file IDs and multi-episode order must not determine priority.
	for _, episode := range []int{100, 9, 4, 2, 1, 10} {
		fid := 200 - episode
		source := filepath.Join(dir, fmt.Sprintf("episode-%d.mkv", episode))
		mediaFile(t, source)
		r.episodeFiles[fid] = map[string]any{"path": source}
		r.episodes = append(r.episodes, map[string]any{"id": episode, "seasonNumber": 1, "episodeNumber": episode, "hasFile": true, "episodeFileId": fid})
	}
	r.episodes = append(r.episodes, map[string]any{"id": 3, "seasonNumber": 1, "episodeNumber": 3, "hasFile": true, "episodeFileId": 198})
	r.bot.state.Requests = []RequestRecord{{Kind: "series", ItemID: 91, Season: 1, Chat: 70, Label: "Show"}}
	if err := r.bot.discoverPreparation(0); err != nil {
		t.Fatal(err)
	}
	var got []int
	var previous int64
	for _, ref := range r.bot.state.Requests[0].Preparation {
		var job PrepareJob
		if err := readJSON(filepath.Join(r.bot.cfg.PreparationDir, "requests", ref.ID+".json"), &job); err != nil {
			t.Fatal(err)
		}
		if job.Created < previous {
			t.Fatal("creation timestamps do not follow discovery order")
		}
		previous = job.Created
		got = append(got, episodeNumber(job))
	}
	if !reflect.DeepEqual(got, []int{1, 2, 4, 9, 10, 100}) {
		t.Fatalf("episode order: %v", got)
	}
}

func TestPreparationOrdersLegacyQueueWithoutMovingOtherTitleSlots(t *testing.T) {
	job := func(id, output string, created int64) PrepareJob {
		return PrepareJob{ID: id, Output: output, Created: created}
	}
	jobs := []PrepareJob{
		job("episode-100", "shows/Show/Season 01/Show - S01E100.mp4", 5),
		job("episode-4", "shows/Show/Season 01/Show - S01E04.mp4", 1),
		job("film", "films/Film/Film.mp4", 2),
		job("episode-1", "shows/Show/Season 01/Show - S01E01-E02.mp4", 3),
		job("other-season", "shows/Show/Season 02/Show - S02E01.mp4", 4),
		job("episode-10", "shows/Show/Season 01/Show - S01E10.mp4", 6),
		job("other-show", "shows/Another/Season 01/Another - S01E01.mp4", 7),
	}
	orderPreparationJobs(jobs)
	var got []string
	for _, job := range jobs {
		got = append(got, job.ID)
	}
	want := []string{"episode-1", "film", "episode-4", "other-season", "episode-10", "episode-100", "other-show"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	// Equal timestamps and duplicate episode numbers have deterministic FIFO ties.
	jobs = []PrepareJob{job("b", "films/B.mp4", 1), job("a", "films/A.mp4", 1)}
	orderPreparationJobs(jobs)
	if jobs[0].ID != "a" {
		t.Fatal("equal timestamps have unstable ordering")
	}
}
