package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func prepareRig(t *testing.T) *rig {
	r := setup(t)
	r.bot.cfg.PreparationDir = t.TempDir()
	r.bot.cfg.MediaRoot = t.TempDir()
	r.bot.cfg.Movie.Root = filepath.Join(r.bot.cfg.MediaRoot, "complete", "films")
	r.bot.cfg.Series.Root = filepath.Join(r.bot.cfg.MediaRoot, "complete", "shows")
	if err := prepareDirs(r.bot.cfg.PreparationDir); err != nil {
		t.Fatal(err)
	}
	return r
}
func mediaFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("test-media"), 0644); err != nil {
		t.Fatal(err)
	}
}
func prepMovieRequest(t *testing.T, r *rig) string {
	t.Helper()
	dir := filepath.Join(r.bot.cfg.Movie.Root, "Film (2024)")
	path := filepath.Join(dir, "Film.mkv")
	mediaFile(t, path)
	r.movieDetail = map[string]any{"id": 81, "hasFile": true, "path": dir, "movieFile": map[string]any{"id": 11, "relativePath": "Film.mkv"}}
	r.bot.state.Requests = []RequestRecord{{Kind: "movie", ItemID: 81, Chat: 70, Label: "Film"}}
	return path
}
func TestPreparationDiscoveryDedupeAndRestart(t *testing.T) {
	r := prepareRig(t)
	prepMovieRequest(t, r)
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != 0 {
		t.Fatal("queued file announced ready")
	}
	refs := r.bot.state.Requests[0].Preparation
	if len(refs) != 1 || refs[0].Output != filepath.Join("films", "Film (2024)", "Film (2024).mp4") {
		t.Fatalf("bad refs: %+v", refs)
	}
	r.bot.state.Requests = append(r.bot.state.Requests, RequestRecord{Kind: "movie", ItemID: 81, Chat: 80, Label: "Film"})
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(filepath.Join(r.bot.cfg.PreparationDir, "requests"))
	if len(files) != 1 {
		t.Fatal("duplicate physical job")
	}
	restarted := newBot(r.bot.cfg, r.server.Client())
	if err := restarted.load(); err != nil {
		t.Fatal(err)
	}
	if err := restarted.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if len(restarted.state.Requests[0].Preparation) != 1 {
		t.Fatal("restart lost job")
	}
}
func TestPreparationExistingMovieNoSearchAndNoDuplicateRequest(t *testing.T) {
	r := prepareRig(t)
	prepMovieRequest(t, r)
	r.bot.state.Requests = nil
	r.existingMovie = []map[string]any{{"id": 81, "tmdbId": 21, "hasFile": true}}
	v := &Selection{Kind: "movie", Item: map[string]any{"tmdbId": 21}, Chat: 70, Label: "Film"}
	for i := 0; i < 2; i++ {
		if err := r.bot.add(v); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.bot.state.Requests) != 1 {
		t.Fatal("duplicate tracked request")
	}
	if hasCall(r.calls, "POST /api/v3/command") || hasCall(r.calls, "PUT /api/v3/movie") {
		t.Fatal("changed ARR/download state")
	}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if len(r.bot.state.Requests[0].Preparation) != 1 {
		t.Fatal("existing file not queued")
	}
}
func TestPreparationUpgradeAndRetryCrashRecovery(t *testing.T) {
	r := prepareRig(t)
	source := prepMovieRequest(t, r)
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	old := r.bot.state.Requests[0].Preparation[0]
	var job PrepareJob
	if err := readJSON(filepath.Join(r.bot.cfg.PreparationDir, "requests", old.ID+".json"), &job); err != nil {
		t.Fatal(err)
	}
	// Producer wrote a retry but crashed before saving notification state.
	job.Generation++
	if err := writeJSON(filepath.Join(r.bot.cfg.PreparationDir, "requests", old.ID+".json"), job); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if r.bot.state.Requests[0].Preparation[0].Generation != job.Generation {
		t.Fatal("retry generation not reconciled")
	}
	if err := os.WriteFile(source, []byte("upgraded-media-is-longer"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	refs := r.bot.state.Requests[0].Preparation
	if len(refs) != 1 || refs[0].ID == old.ID || refs[0].Output != old.Output {
		t.Fatal("upgrade was not replaced/deduped by output")
	}
	worker := &Worker{Queue: r.bot.cfg.PreparationDir}
	if !worker.superseded(job) {
		t.Fatal("old job can overwrite an upgrade")
	}
}
func TestPreparationNotificationsRetryOwnershipAndActive(t *testing.T) {
	r := prepareRig(t)
	prepMovieRequest(t, r)
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	ref := r.bot.state.Requests[0].Preparation[0]
	path := filepath.Join(r.bot.cfg.PreparationDir, "results", ref.ID+".json")
	if err := writeJSON(path, PrepareResult{Generation: ref.Generation, State: "failed", Detail: "test failure"}); err != nil {
		t.Fatal(err)
	}
	r.failSend = true
	if r.bot.pollPreparation() == nil {
		t.Fatal("expected send error")
	}
	if r.bot.state.Requests[0].Preparation[0].Notified {
		t.Fatal("failed Telegram send checkpointed")
	}
	r.failSend = false
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != 1 || !strings.Contains(r.messages[0], "original available") {
		t.Fatalf("bad notifications: %v", r.messages)
	}
	if err := r.bot.retryPreparation(80, 1); err != nil {
		t.Fatal(err)
	}
	if r.bot.state.Requests[0].Preparation[0].Generation != ref.Generation {
		t.Fatal("other chat retried job")
	}
	if err := r.bot.handle(msg("/retry 1")); err != nil {
		t.Fatal(err)
	}
	next := r.bot.state.Requests[0].Preparation[0]
	if next.Generation == ref.Generation || next.Notified {
		t.Fatal("retry was not persisted")
	}
	if err := writeJSON(path, PrepareResult{Generation: next.Generation, State: "preparing", Action: "audio-only", Progress: 43}); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.active(70); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "audio-only · 43%") {
		t.Fatal("progress missing")
	}
	if err := writeJSON(path, PrepareResult{Generation: next.Generation, State: "ready"}); err != nil {
		t.Fatal(err)
	}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(r.messages[len(r.messages)-1], "Streaming Films") {
		t.Fatal("ready notification missing")
	}
}
func TestPreparationSeasonFilterMultiEpisodeAndBatchNotification(t *testing.T) {
	r := prepareRig(t)
	dir := filepath.Join(r.bot.cfg.Series.Root, "Show (2023)")
	source := filepath.Join(dir, "Season 02", "Show.S02E01-E02.mkv")
	mediaFile(t, source)
	r.seriesDetail = map[string]any{"id": 91, "path": dir}
	r.episodes = []map[string]any{{"id": 41, "seasonNumber": 2, "episodeNumber": 1, "hasFile": true, "episodeFileId": 22}, {"id": 42, "seasonNumber": 2, "episodeNumber": 2, "hasFile": true, "episodeFileId": 22}, {"id": 43, "seasonNumber": 1, "episodeNumber": 3, "hasFile": true, "episodeFileId": 23}}
	r.episodeFiles = map[int]map[string]any{22: {"path": source}}
	r.bot.state.Requests = []RequestRecord{{Kind: "series", ItemID: 91, Season: 2, Chat: 70, Label: "Show"}}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	refs := r.bot.state.Requests[0].Preparation
	if len(refs) != 1 || !strings.Contains(refs[0].Output, "S02E01-E02.mp4") {
		t.Fatalf("bad episode scope: %+v", refs)
	}
	for n := 0; n < 12; n++ {
		ref := PreparationRef{ID: fmt.Sprintf("%064x", n+100), Label: fmt.Sprintf("Show S02E%02d", n+3), Generation: 1, Output: fmt.Sprint(n)}
		r.bot.state.Requests[0].Preparation = append(r.bot.state.Requests[0].Preparation, ref)
	}
	for _, ref := range r.bot.state.Requests[0].Preparation {
		if err := writeJSON(filepath.Join(r.bot.cfg.PreparationDir, "results", ref.ID+".json"), PrepareResult{Generation: ref.Generation, State: "ready"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != 1 || !strings.Contains(r.messages[0], "more updates") {
		t.Fatal("season notification spam")
	}
}
func TestPreparationCancelSharedAndReRequest(t *testing.T) {
	r := prepareRig(t)
	prepMovieRequest(t, r)
	r.bot.state.Requests = append(r.bot.state.Requests, RequestRecord{Kind: "movie", ItemID: 81, Chat: 80, Label: "Film"})
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	ref := r.bot.state.Requests[0].Preparation[0]
	r.bot.state.Requests[0].Cancelled = true
	if err := r.bot.cancelUnusedPreparation(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.bot.cfg.PreparationDir, "cancel", ref.ID)); err == nil {
		t.Fatal("cancelled shared job")
	}
	r.bot.state.Requests[1].Cancelled = true
	if err := r.bot.cancelUnusedPreparation(); err != nil {
		t.Fatal(err)
	}
	r.bot.state.Requests = append(r.bot.state.Requests, RequestRecord{Kind: "movie", ItemID: 81, Chat: 70, Label: "Film"})
	if err := r.bot.pollPreparation(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(r.bot.cfg.PreparationDir, "cancel", ref.ID)); !os.IsNotExist(err) {
		t.Fatal("re-request still cancelled")
	}
	if r.bot.state.Requests[2].Preparation[0].Generation == ref.Generation {
		t.Fatal("re-request uses stale cancelled result")
	}
}
func TestPreparationPathConfinement(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"../escape", "/absolute", "a/../../escape", "a/../b", ".", ""} {
		if _, err := confined(root, path); err == nil {
			t.Fatalf("accepted %q", path)
		}
	}
	outside := filepath.Join(t.TempDir(), "outside.mkv")
	mediaFile(t, outside)
	link := filepath.Join(root, "link.mkv")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := sourcePath(root, link); err == nil {
		t.Fatal("escaped through symlink")
	}
	if validJobID("../../escape") {
		t.Fatal("unsafe job ID")
	}
}
func TestBitrateDecisionAndEncodingArguments(t *testing.T) {
	for _, tc := range []struct {
		name, data string
		duration   float64
		safe       bool
		bad        bool
	}{
		{"low", "0,10000\n1,10000\n6,10000\n", 10, true, false},
		{"peak despite low average", "0,12000000\n", 100, false, false},
		{"short", "0,3000000\n", 1, false, false},
		{"missing timing", "N/A,10000\n", 10, false, true},
		{"invalid timing", "NaN,10000\n", 10, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			safe, err := safePacketBitrate(strings.NewReader(tc.data), tc.duration)
			if safe != tc.safe || (err != nil) != tc.bad {
				t.Fatalf("safe=%v err=%v", safe, err)
			}
		})
	}
	video := MediaStream{Index: 0, Codec: "h264", Pixel: "yuv420p", Width: 1920, Height: 1080}
	if !compatibleVideo(video) {
		t.Fatal("1080p AVC not recognized")
	}
	audio := &MediaStream{Index: 1, Codec: "ac3", Channels: 6}
	args := strings.Join(encodeArgs("input.mkv", "output.mp4", video, audio, true), " ")
	if !strings.Contains(args, "-c:v copy") || !strings.Contains(args, "-c:a aac") || !strings.Contains(args, "-ac 2") || strings.Contains(args, "nvenc") {
		t.Fatal(args)
	}
	args = strings.Join(encodeArgs("input.mkv", "output.mp4", video, audio, false), " ")
	if !strings.Contains(args, "h264_nvenc") || !strings.Contains(args, "min(1080,ih)") {
		t.Fatal(args)
	}
	video.Transfer = "smpte2084"
	if !hdrVideo(video) {
		t.Fatal("HDR not blocked")
	}
}
func TestJellyfinIndexPaginationAndAPIKey(t *testing.T) {
	output := t.TempDir()
	mediaFile(t, filepath.Join(output, "films/Test/Test.mp4"))
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Emby-Token") != "secret" {
			t.Error("API key missing")
		}
		if r.URL.Path == "/Library/Refresh" {
			w.WriteHeader(204)
			return
		}
		if r.URL.Query().Get("Path") != "" {
			t.Error("unsupported Path filter")
		}
		if !strings.Contains(r.URL.Query().Get("Fields"), "MediaSources") {
			t.Error("missing freshness check")
		}
		calls++
		items := []map[string]any{}
		start, _ := strconv.Atoi(r.URL.Query().Get("StartIndex"))
		if start == 0 {
			for i := 0; i < 200; i++ {
				items = append(items, map[string]any{"Path": fmt.Sprint(i)})
			}
			items[0] = map[string]any{"Path": "/optimized/films/Test/Test.mp4", "MediaSources": []map[string]any{{"Path": "/optimized/films/Test/Test.mp4", "Size": 99999}}}
		} else {
			items = append(items, map[string]any{"Path": "/optimized/films/Test/Test.mp4", "MediaSources": []map[string]any{{"Path": "/optimized/films/Test/Test.mp4", "Size": len("test-media")}}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"Items": items, "TotalRecordCount": 201})
	}))
	defer server.Close()
	worker := &Worker{Output: output, JellyfinURL: server.URL, JellyfinKey: "secret", Client: server.Client()}
	if err := worker.ensureIndexed(context.Background(), PrepareJob{Output: "films/Test/Test.mp4"}); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatal("did not paginate")
	}
}
func TestWorkerLockExcludesSecondWorker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "worker.lock")
	first, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	if err := lockWorker(first); err != nil {
		t.Fatal(err)
	}
	if lockWorker(second) == nil {
		t.Fatal("second worker acquired lock")
	}
}
func TestWorkerRealFFmpeg(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip("optional FFmpeg integration test")
		}
	}
	for _, mode := range []string{"remux", "audio-only", "encode"} {
		t.Run(mode, func(t *testing.T) {
			if mode == "encode" && os.Getenv("ARR_TEST_NVENC") != "1" {
				t.Skip("set ARR_TEST_NVENC=1 for GPU test")
			}
			root := t.TempDir()
			queue := filepath.Join(root, "queue")
			media := filepath.Join(root, "media")
			output := filepath.Join(root, "output")
			if err := prepareDirs(queue); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(media, 0755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(output, 0755); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(media, "input.mkv")
			vcodec := "libx264"
			acodec := "aac"
			if mode == "audio-only" {
				acodec = "ac3"
			}
			if mode == "encode" {
				vcodec = "mpeg4"
			}
			cmd := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "testsrc2=size=256x144:rate=24", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000", "-t", "2", "-c:v", vcodec, "-pix_fmt", "yuv420p", "-c:a", acodec, source)
			if data, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("fixture: %v %s", err, data)
			}
			before, err := os.ReadFile(source)
			if err != nil {
				t.Fatal(err)
			}
			checksum := sha256.Sum256(before)
			sidecar := strings.TrimSuffix(source, ".mkv") + ".en.srt"
			if err := os.WriteFile(sidecar, []byte("1\n00:00:00,000 --> 00:00:01,000\nHello\n"), 0644); err != nil {
				t.Fatal(err)
			}
			st, _ := os.Stat(source)
			job := PrepareJob{ID: strings.Repeat("a", 64), Source: source, Output: "films/Test/Test.mp4", Size: st.Size(), Modified: st.ModTime().UnixNano(), Generation: 1, Created: 1}
			if err := writeJSON(filepath.Join(queue, "requests", job.ID+".json"), job); err != nil {
				t.Fatal(err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/Library/VirtualFolders" {
					_ = json.NewEncoder(w).Encode([]map[string]any{{"Locations": []string{"/optimized/films"}}})
					return
				}
				if r.URL.Path == "/Library/Refresh" {
					w.WriteHeader(204)
					return
				}
				st, err := os.Stat(filepath.Join(output, job.Output))
				if err != nil {
					t.Error(err)
					w.WriteHeader(500)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"Items": []map[string]any{{"Path": "/optimized/films/Test/Test.mp4", "MediaSources": []map[string]any{{"Path": "/optimized/films/Test/Test.mp4", "Size": st.Size()}}}}, "TotalRecordCount": 1})
			}))
			defer server.Close()
			worker := &Worker{Queue: queue, Media: media, Output: output, JellyfinURL: server.URL, JellyfinKey: "secret", Client: server.Client()}
			staleSubtitle := filepath.Join(output, "films/Test/Test.stale.en.srt")
			mediaFile(t, staleSubtitle)
			// Simulate a process dying mid-job; the next worker resumes by restarting safely.
			if err := worker.saveResult(job, PrepareResult{State: "preparing", Progress: 30}); err != nil {
				t.Fatal(err)
			}
			if err := worker.tick(); err != nil {
				t.Fatal(err)
			}
			var result PrepareResult
			if err := readJSON(filepath.Join(queue, "results", job.ID+".json"), &result); err != nil {
				t.Fatal(err)
			}
			if result.State != "ready" || result.Action != mode {
				t.Fatalf("unexpected result: %+v", result)
			}
			if _, err := os.Stat(staleSubtitle); !os.IsNotExist(err) {
				t.Fatal("obsolete version's subtitles were retained")
			}
			after, _ := os.ReadFile(source)
			if sha256.Sum256(after) != checksum {
				t.Fatal("source modified")
			}
			destination := filepath.Join(output, job.Output)
			if _, err := os.Stat(destination); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(strings.TrimSuffix(destination, ".mp4") + ".en.srt"); err != nil {
				t.Fatal("sidecar missing")
			}
			outst, _ := os.Stat(destination)
			// Retry an indexing-only failure without another encode/remux.
			result.State = "failed"
			if err := worker.saveResult(job, result); err != nil {
				t.Fatal(err)
			}
			job.Generation = 2
			if err := writeJSON(filepath.Join(queue, "requests", job.ID+".json"), job); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond)
			if err := worker.tick(); err != nil {
				t.Fatal(err)
			}
			nextst, _ := os.Stat(destination)
			if !nextst.ModTime().Equal(outst.ModTime()) {
				t.Fatal("indexing retry unnecessarily encoded")
			}
			// Later Bazarr sidecars are picked up without touching video.
			if err := os.WriteFile(sidecar, []byte("1\n00:00:00,000 --> 00:00:01,000\nUpdated\n"), 0644); err != nil {
				t.Fatal(err)
			}
			if err := worker.tick(); err != nil {
				t.Fatal(err)
			}
			sidecarData, _ := os.ReadFile(strings.TrimSuffix(destination, ".mp4") + ".en.srt")
			if !strings.Contains(string(sidecarData), "Updated") {
				t.Fatal("late subtitle update missing")
			}
			// A failed replacement must not truncate an already good published copy.
			bin := t.TempDir()
			if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
			replacement := job
			replacement.ID = strings.Repeat("b", 64)
			replacement.Created = 2
			if err := writeJSON(filepath.Join(queue, "requests", replacement.ID+".json"), replacement); err != nil {
				t.Fatal(err)
			}
			if err := worker.process(replacement); err != nil {
				t.Fatal(err)
			}
			var failed PrepareResult
			if err := readJSON(filepath.Join(queue, "results", replacement.ID+".json"), &failed); err != nil {
				t.Fatal(err)
			}
			if failed.State != "failed" || failed.Published {
				t.Fatalf("bad failed replacement: %+v", failed)
			}
			finalst, _ := os.Stat(destination)
			if !finalst.ModTime().Equal(outst.ModTime()) {
				t.Fatal("failed replacement changed published copy")
			}
		})
	}
}
func TestWorkerMissingSourcePreservesExistingOutput(t *testing.T) {
	root := t.TempDir()
	queue := filepath.Join(root, "queue")
	output := filepath.Join(root, "output")
	media := filepath.Join(root, "media")
	if err := prepareDirs(queue); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(media, 0755); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(output, "films/Test/Test.mp4")
	mediaFile(t, destination)
	job := PrepareJob{ID: strings.Repeat("b", 64), Source: filepath.Join(media, "missing.mkv"), Output: "films/Test/Test.mp4", Generation: 1}
	worker := &Worker{Queue: queue, Media: media, Output: output}
	if err := worker.process(job); err != nil {
		t.Fatal(err)
	}
	var result PrepareResult
	_ = readJSON(filepath.Join(queue, "results", job.ID+".json"), &result)
	if result.State != "failed" {
		t.Fatal(result)
	}
	data, _ := os.ReadFile(destination)
	if string(data) != "test-media" {
		t.Fatal("previous good output lost")
	}
}
