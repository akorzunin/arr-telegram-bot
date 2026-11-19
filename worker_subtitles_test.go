package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// A synthetic 2x1 white PGS image, displayed for one second. No downloaded media.
func pgsFixture() []byte {
	var data bytes.Buffer
	segment := func(pts uint32, kind byte, payload []byte) {
		data.WriteString("PG")
		_ = binary.Write(&data, binary.BigEndian, pts)
		_ = binary.Write(&data, binary.BigEndian, pts)
		data.WriteByte(kind)
		_ = binary.Write(&data, binary.BigEndian, uint16(len(payload)))
		data.Write(payload)
	}
	segment(90000, 0x16, []byte{7, 128, 4, 56, 0x10, 0, 0, 0x80, 0, 0, 1, 0, 0, 0, 0, 0, 100, 0, 100})
	segment(90000, 0x17, []byte{1, 0, 0, 100, 0, 100, 0, 2, 0, 1})
	segment(90000, 0x14, []byte{0, 0, 0, 16, 128, 128, 0, 1, 235, 128, 128, 255})
	segment(90000, 0x15, []byte{0, 0, 0, 0xc0, 0, 0, 8, 0, 2, 0, 1, 1, 1, 0, 0})
	segment(90000, 0x80, nil)
	segment(180000, 0x16, []byte{7, 128, 4, 56, 0x10, 0, 1, 0, 0, 0, 0})
	segment(180000, 0x80, nil)
	return data.Bytes()
}

func TestSubtitleFormatsAndPublication(t *testing.T) {
	for codec, want := range map[string]string{"subrip": "srt", "mov_text": "srt", "hdmv_pgs_subtitle": "sup", "ass": "", "dvd_subtitle": ""} {
		if got := subtitleFormat(codec); got != want {
			t.Fatalf("format %s: %s != %s", codec, got, want)
		}
	}
	stage, output := t.TempDir(), t.TempDir()
	destination := filepath.Join(output, "Episode.mp4")
	for _, name := range []string{"video.eng.track3.forced.default.sup", "video.en.srt", "video.mp4"} {
		mediaFile(t, filepath.Join(stage, name))
	}
	for _, name := range []string{"Episode.old.sup", "Episode.old.srt", "Other.sup", "Episode.notes.txt"} {
		mediaFile(t, filepath.Join(output, name))
	}
	if err := publishSidecars(stage, destination); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Episode.eng.track3.forced.default.sup", "Episode.en.srt", "Other.sup", "Episode.notes.txt"} {
		if _, err := os.Stat(filepath.Join(output, name)); err != nil {
			t.Fatal(name, err)
		}
	}
	for _, name := range []string{"Episode.old.sup", "Episode.old.srt", "Episode.mp4"} {
		if _, err := os.Stat(filepath.Join(output, name)); !os.IsNotExist(err) {
			t.Fatal("unexpected publication/retention:", name)
		}
	}
}

func TestWorkerRealPGSExtractionAndReadyBackfill(t *testing.T) {
	for _, binary := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(binary); err != nil {
			t.Skip("optional FFmpeg integration test")
		}
	}
	media, output, queue := t.TempDir(), t.TempDir(), t.TempDir()
	if err := prepareDirs(queue); err != nil {
		t.Fatal(err)
	}
	pgs := filepath.Join(media, "fixture.sup")
	if err := os.WriteFile(pgs, pgsFixture(), 0644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(media, "input.mkv")
	cmd := exec.Command("ffmpeg", "-v", "error", "-nostdin", "-f", "lavfi", "-i", "color=size=256x144:rate=24", "-i", pgs, "-t", "3", "-map", "0:v", "-map", "1:s", "-map", "1:s", "-c:v", "libx264", "-c:s", "copy", "-metadata:s:s:0", "language=eng", "-disposition:s:0", "default+forced", "-metadata:s:s:1", "language=fre", "-disposition:s:1", "0", source)
	if data, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture: %v %s", err, data)
	}
	before, _ := os.ReadFile(source)
	st, _ := os.Stat(source)
	job := PrepareJob{ID: strings.Repeat("a", 64), Source: source, Output: "films/Test/Test.mp4", Size: st.Size(), Modified: st.ModTime().UnixNano(), Generation: 1, Created: 1}
	probe, err := probeMedia(context.Background(), source)
	if err != nil {
		t.Fatal(err)
	}
	w := &Worker{Queue: queue, Media: media, Output: output}
	realFFmpeg, _ := exec.LookPath("ffmpeg")
	calls := filepath.Join(t.TempDir(), "calls")
	wrapper := t.TempDir()
	if err := os.WriteFile(filepath.Join(wrapper, "ffmpeg"), []byte("#!/bin/sh\nprintf 'call\\n' >> \"$FFMPEG_CALLS\"\nexec \"$REAL_FFMPEG\" \"$@\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("REAL_FFMPEG", realFFmpeg)
	t.Setenv("FFMPEG_CALLS", calls)
	t.Setenv("PATH", wrapper+":"+os.Getenv("PATH"))
	stage := t.TempDir()
	if err := w.stageSubtitles(context.Background(), job, probe, stage); err != nil {
		t.Fatal(err)
	}
	invocations, _ := os.ReadFile(calls)
	if string(invocations) != "call\n" {
		t.Fatal("subtitle tracks required more than one source demux pass")
	}
	if _, err := os.Stat(filepath.Join(stage, "video.fre.track2.sup")); err != nil {
		t.Fatal("second PGS track missing", err)
	}
	extracted := filepath.Join(stage, "video.eng.track1.forced.default.sup")
	got, err := os.ReadFile(extracted)
	if err != nil {
		t.Fatal(err)
	}
	// Matroska remuxing can shift timestamps; compare lossless subtitle payloads.
	normalize := func(data []byte) []byte {
		data = bytes.Clone(data)
		for offset := 0; offset < len(data); {
			if offset+13 > len(data) || string(data[offset:offset+2]) != "PG" {
				t.Fatal("invalid PGS segment")
			}
			clear(data[offset+2 : offset+10])
			offset += 13 + int(binary.BigEndian.Uint16(data[offset+11:offset+13]))
			if offset > len(data) {
				t.Fatal("truncated PGS segment")
			}
		}
		return data
	}
	if !bytes.Equal(normalize(got), normalize(pgsFixture())) {
		t.Fatal("PGS extraction changed source subtitle payloads")
	}
	firstPTS := func(path string) string {
		t.Helper()
		cmd := exec.Command("ffprobe", "-v", "error", "-select_streams", "s:0", "-show_entries", "packet=pts_time", "-of", "csv=p=0", path)
		data, err := cmd.Output()
		if err != nil || len(strings.Fields(string(data))) == 0 {
			t.Fatal("cannot inspect subtitle timing", err)
		}
		return strings.Fields(string(data))[0]
	}
	if firstPTS(source) != firstPTS(extracted) {
		t.Fatal("subtitle extraction changed presentation timing")
	}

	// An old ready job must gain subtitles without encoding or replacing its video.
	destination := filepath.Join(output, job.Output)
	mediaFile(t, destination)
	videoBefore, _ := os.Stat(destination)
	refreshes := 0
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/Library/Refresh" {
			refreshes++
			rw.WriteHeader(http.StatusNoContent)
			return
		}
		_ = json.NewEncoder(rw).Encode(map[string]any{"Items": []map[string]any{{"Path": "/optimized/" + job.Output, "MediaSources": []map[string]any{{"Path": "/optimized/" + job.Output, "Size": videoBefore.Size()}}}}, "TotalRecordCount": 1})
	}))
	defer server.Close()
	w.JellyfinURL, w.JellyfinKey, w.Client = server.URL, "secret", server.Client()
	if err := writeJSON(filepath.Join(queue, "requests", job.ID+".json"), job); err != nil {
		t.Fatal(err)
	}
	if err := w.saveResult(job, PrepareResult{Generation: 1, State: "ready", Published: true}); err != nil {
		t.Fatal(err)
	}
	if err := w.tick(); err != nil {
		t.Fatal(err)
	}
	var result PrepareResult
	if err := readJSON(filepath.Join(queue, "results", job.ID+".json"), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "ready" || result.SubtitleVersion != subtitleVersion || !strings.Contains(result.Warning, "PGS subtitles preserved") {
		t.Fatalf("backfill result: %+v", result)
	}
	sidecar := strings.TrimSuffix(destination, ".mp4") + ".eng.track1.forced.default.sup"
	got, err = os.ReadFile(sidecar)
	if err != nil || !bytes.Equal(normalize(got), normalize(pgsFixture())) {
		t.Fatal("ready job did not publish original PGS", err)
	}
	videoAfter, _ := os.Stat(destination)
	if !videoAfter.ModTime().Equal(videoBefore.ModTime()) || videoAfter.Size() != videoBefore.Size() {
		t.Fatal("subtitle backfill modified video")
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) {
		t.Fatal("source modified")
	}
	// A second tick must not re-extract/reindex unchanged embedded subtitles.
	if err := w.tick(); err != nil {
		t.Fatal(err)
	}
	if refreshes != 1 {
		t.Fatalf("unnecessary subtitle refreshes: %d", refreshes)
	}

	// Failed extraction must not remove an already published video or subtitle set.
	result.SubtitleVersion = 0
	if err := w.saveResult(job, result); err != nil {
		t.Fatal(err)
	}
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "ffmpeg"), []byte("#!/bin/sh\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	if err := w.tick(); err != nil {
		t.Fatal(err)
	}
	if err := readJSON(filepath.Join(queue, "results", job.ID+".json"), &result); err != nil {
		t.Fatal(err)
	}
	if result.State != "failed" || result.Detail != "subtitle preparation failed" || !result.Published {
		t.Fatalf("extraction failure result: %+v", result)
	}
	retained, _ := os.ReadFile(sidecar)
	videoAfter, _ = os.Stat(destination)
	if !bytes.Equal(retained, got) || !videoAfter.ModTime().Equal(videoBefore.ModTime()) {
		t.Fatal("failed extraction changed a published video/subtitle")
	}
}
