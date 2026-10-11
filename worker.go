package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type Worker struct {
	Queue, Media, Output, JellyfinURL, JellyfinKey string
	Client                                         *http.Client
}
type MediaStream struct {
	Index       int    `json:"index"`
	Type        string `json:"codec_type"`
	Codec       string `json:"codec_name"`
	Pixel       string `json:"pix_fmt"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Channels    int    `json:"channels"`
	Profile     string `json:"profile"`
	BitRate     string `json:"bit_rate"`
	Transfer    string `json:"color_transfer"`
	Primaries   string `json:"color_primaries"`
	Disposition struct {
		Default  int `json:"default"`
		Attached int `json:"attached_pic"`
	} `json:"disposition"`
	Tags     map[string]string `json:"tags"`
	SideData []struct {
		Type string `json:"side_data_type"`
	} `json:"side_data_list"`
}
type MediaProbe struct {
	Streams []MediaStream `json:"streams"`
	Format  struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func probeMedia(ctx context.Context, path string) (MediaProbe, error) {
	var probe MediaProbe
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_streams", "-show_format", "-of", "json", path)
	data, err := cmd.Output()
	if err != nil {
		return probe, errors.New("cannot inspect media")
	}
	err = json.Unmarshal(data, &probe)
	return probe, err
}
func selectedStreams(probe MediaProbe) (MediaStream, *MediaStream, error) {
	var video *MediaStream
	var audio *MediaStream
	for i := range probe.Streams {
		s := &probe.Streams[i]
		if s.Type == "video" && s.Disposition.Attached == 0 && video == nil {
			video = s
		}
		if s.Type == "audio" && (audio == nil || s.Disposition.Default > audio.Disposition.Default) {
			audio = s
		}
	}
	if video == nil {
		return MediaStream{}, nil, errors.New("no video stream")
	}
	return *video, audio, nil
}
func hdrVideo(video MediaStream) bool {
	for _, data := range video.SideData {
		if strings.Contains(data.Type, "DOVI") {
			return true
		}
	}
	return video.Transfer == "smpte2084" || video.Transfer == "arib-std-b67" || video.Primaries == "bt2020" || video.Codec == "dolby_vision"
}
func compatibleAudio(audio *MediaStream) bool {
	if audio == nil {
		return true
	}
	if audio.Codec != "aac" || audio.Profile != "LC" || audio.Channels < 1 || audio.Channels > 2 {
		return false
	}
	rate, err := strconv.ParseInt(audio.BitRate, 10, 64)
	return err != nil || (rate > 0 && rate <= 256_000)
}
func compatibleVideo(video MediaStream) bool {
	return video.Codec == "h264" && video.Pixel == "yuv420p" && video.Width <= 1920 && video.Height <= 1080
}
func probeDuration(probe MediaProbe) (float64, error) {
	d, err := strconv.ParseFloat(probe.Format.Duration, 64)
	if err != nil || d <= 0 || math.IsInf(d, 0) || math.IsNaN(d) {
		return 0, errors.New("duration unavailable")
	}
	return d, nil
}

// Five-second rolling bitrate assumes normal player buffering; inspect shorter
// windows or enforce a stricter VBV target if client buffering proves inadequate.
func safePacketBitrate(reader io.Reader, duration float64) (bool, error) {
	type packet struct {
		t    float64
		size float64
	}
	packets := []packet{}
	head := 0
	total := 0.0
	last := -math.MaxFloat64
	csvReader := csv.NewReader(reader)
	csvReader.FieldsPerRecord = -1
	for {
		fields, err := csvReader.Read()
		if err == io.EOF {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		if len(fields) < 2 {
			return false, errors.New("packet timing unavailable")
		}
		stamp, err := strconv.ParseFloat(fields[0], 64)
		if err != nil || math.IsNaN(stamp) || math.IsInf(stamp, 0) {
			return false, errors.New("packet timing unavailable")
		}
		size, err := strconv.ParseFloat(fields[1], 64)
		if err != nil || size < 0 || math.IsNaN(size) || math.IsInf(size, 0) {
			return false, errors.New("packet size unavailable")
		}
		// PTS can be reordered by B-frames/interleaved streams. Conservative monotonic clamp.
		if stamp < last {
			stamp = last
		}
		last = stamp
		packets = append(packets, packet{stamp, size})
		total += size
		for head < len(packets) && packets[head].t < stamp-5 {
			total -= packets[head].size
			head++
		}
		if total*8/math.Min(5, duration) > 18_000_000 {
			return false, nil
		}
		if head > 4096 {
			packets = append([]packet(nil), packets[head:]...)
			head = 0
		}
	}
}
func safeSourceBitrate(ctx context.Context, job PrepareJob, duration float64) (bool, error) {
	if float64(job.Size)*8/duration > 15_000_000 {
		return false, nil
	}
	cmd := exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_packets", "-show_entries", "packet=pts_time,size", "-of", "csv=p=0", job.Source)
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		return false, err
	}
	if err = cmd.Start(); err != nil {
		return false, err
	}
	safe, scanErr := safePacketBitrate(pipe, duration)
	if !safe || scanErr != nil {
		_ = cmd.Process.Kill()
	}
	waitErr := cmd.Wait()
	if scanErr != nil {
		return false, scanErr
	}
	if !safe {
		return false, nil
	}
	return true, waitErr
}
func encodeArgs(source, output string, video MediaStream, audio *MediaStream, copyVideo bool) []string {
	args := []string{"-hide_banner", "-loglevel", "error", "-xerror", "-nostdin", "-y", "-i", source, "-map", fmt.Sprintf("0:%d", video.Index), "-map_metadata", "0"}
	if audio != nil {
		args = append(args, "-map", fmt.Sprintf("0:%d", audio.Index))
	}
	if copyVideo {
		args = append(args, "-c:v", "copy")
	} else {
		// CPU decode/scale is intentionally used for SDR only; CUDA scaling can replace
		// it if CPU usage becomes limiting. NVIDIA still performs the costly encoding.
		args = append(args, "-vf", "scale=w='min(1920,iw)':h='min(1080,ih)':force_original_aspect_ratio=decrease:force_divisible_by=2,format=yuv420p", "-c:v", "h264_nvenc", "-preset", "p6", "-b:v", "12M", "-maxrate", "16M", "-bufsize", "32M")
	}
	if audio != nil {
		if compatibleAudio(audio) && (copyVideo || audio.BitRate != "") {
			args = append(args, "-c:a", "copy")
		} else {
			args = append(args, "-c:a", "aac", "-b:a", "192k", "-ac", "2")
		}
	}
	return append(args, "-sn", "-movflags", "+faststart", "-progress", "pipe:1", "-nostats", output)
}
func (w *Worker) saveResult(job PrepareJob, result PrepareResult) error {
	result.Generation = job.Generation
	return writeJSON(filepath.Join(w.Queue, "results", job.ID+".json"), result)
}
func (w *Worker) cancelled(job PrepareJob) bool {
	_, err := os.Stat(filepath.Join(w.Queue, "cancel", job.ID))
	return err == nil
}
func (w *Worker) superseded(job PrepareJob) bool {
	entries, err := os.ReadDir(filepath.Join(w.Queue, "requests"))
	if err != nil {
		return false
	}
	// Linear scan here makes a whole tick O(n²); add an output-key index if
	// queues grow beyond a few hundred files.
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		var other PrepareJob
		if readJSON(filepath.Join(w.Queue, "requests", entry.Name()), &other) == nil && other.Output == job.Output && other.ID != job.ID && other.Created > job.Created && !w.cancelled(other) {
			return true
		}
	}
	return false
}
func availableSpace(dir string, required uint64) bool {
	var st syscall.Statfs_t
	if syscall.Statfs(dir, &st) != nil {
		return false
	}
	return st.Bavail*uint64(st.Bsize) >= required
}
func (w *Worker) runFFmpeg(ctx context.Context, job PrepareJob, args []string, duration float64, result *PrepareResult) error {
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	// FFmpeg errors can contain paths/titles; keep them out of generic service logs.
	cmd.Stderr = io.Discard
	if err = cmd.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(stdout)
	last := -1
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "out_time_us=") {
			continue
		}
		us, err := strconv.ParseFloat(strings.TrimPrefix(line, "out_time_us="), 64)
		if err != nil {
			continue
		}
		percent := int(us / 1e6 / duration * 100)
		if percent < 0 {
			percent = 0
		}
		if percent > 99 {
			percent = 99
		}
		if percent/5 != last {
			result.Progress = percent
			if err := w.saveResult(job, *result); err != nil {
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				return err
			}
			last = percent / 5
		}
	}
	if err = scanner.Err(); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return err
	}
	return cmd.Wait()
}
func validateOutput(ctx context.Context, path string, original float64) error {
	probe, err := probeMedia(ctx, path)
	if err != nil {
		return err
	}
	duration, err := probeDuration(probe)
	if err != nil {
		return err
	}
	video, audio, err := selectedStreams(probe)
	if err != nil {
		return err
	}
	if !compatibleVideo(video) || hdrVideo(video) || !compatibleAudio(audio) || math.Abs(duration-original) > math.Max(.25, math.Min(2, original*.005)) {
		return errors.New("output validation failed")
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	safe, err := safeSourceBitrate(ctx, PrepareJob{Source: path, Size: st.Size()}, duration)
	if err != nil || !safe {
		return errors.New("output exceeds streaming bitrate budget")
	}
	// Full decode catches damaged streams before publication; expensive but offline.
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-xerror", "-nostdin", "-i", path, "-map", "0:v:0", "-map", "0:a?", "-f", "null", "-")
	cmd.Stderr = io.Discard
	if cmd.Run() != nil {
		return errors.New("output decode validation failed")
	}
	return nil
}
func (w *Worker) process(job PrepareJob) error {
	var prior PrepareResult
	_ = readJSON(filepath.Join(w.Queue, "results", job.ID+".json"), &prior)
	result := PrepareResult{State: "checking", Published: prior.Published, Action: prior.Action}
	if err := w.saveResult(job, result); err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	defer close(done)
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if w.cancelled(job) || w.superseded(job) {
					cancel()
					return
				}
			}
		}
	}()
	fail := func(state, detail string) error {
		if w.cancelled(job) || w.superseded(job) {
			state = "cancelled"
			detail = "cancelled or superseded"
		}
		result.State = state
		result.Detail = detail
		return w.saveResult(job, result)
	}
	if !sourceUnchanged(job, w.Media) {
		return fail("failed", "source changed or missing; wait for ARR discovery")
	}
	destination, err := confined(w.Output, job.Output)
	if err != nil {
		return fail("failed", "unsafe output path")
	}
	// Output trees are worker-owned; reject symlinked parent directories as well.
	if err = os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return fail("failed", "output directory unavailable")
	}
	if _, err = sourcePath(w.Output, filepath.Dir(destination)); err != nil {
		return fail("failed", "unsafe output directory")
	}
	stageDir := filepath.Join(w.Output, ".staging", job.ID)
	if err = os.RemoveAll(stageDir); err != nil {
		return fail("failed", "staging cleanup failed")
	}
	if err = os.MkdirAll(stageDir, 0700); err != nil {
		return fail("failed", "staging directory unavailable")
	}
	defer os.RemoveAll(stageDir)
	probe, err := probeMedia(ctx, job.Source)
	if err != nil {
		return fail("failed", "media inspection failed")
	}
	duration, err := probeDuration(probe)
	if err != nil {
		return fail("blocked", "duration unavailable")
	}
	video, audio, err := selectedStreams(probe)
	if err != nil {
		return fail("blocked", "no video stream")
	}
	if hdrVideo(video) {
		return fail("blocked", "HDR tone mapping is not supported yet")
	}
	for _, stream := range probe.Streams {
		if stream.Type == "subtitle" && stream.Codec != "subrip" && stream.Codec != "mov_text" {
			result.Warning = "Image/styled subtitles remain in originals; choose external SRT for streaming."
		}
	}
	if result.Published {
		if st, err := os.Stat(destination); err == nil && st.Mode().IsRegular() {
			result.State = "indexing"
			result.Progress = 100
			if err = w.saveResult(job, result); err != nil {
				return err
			}
			if err = w.stageSubtitles(ctx, job, probe, stageDir); err != nil {
				return fail("failed", "subtitle preparation failed")
			}
			if err = publishSidecars(stageDir, destination); err != nil {
				return fail("failed", "subtitle publication failed")
			}
			if err = w.ensureIndexed(ctx, job); err != nil {
				return fail("failed", "video published; Jellyfin indexing failed")
			}
			result.State = "ready"
			return w.saveResult(job, result)
		}
		result.Published = false
	}
	if err := w.checkLibrary(ctx, job); err != nil {
		return fail("blocked", "configure the Jellyfin streaming library and check the worker API key")
	}
	copyVideo := false
	if compatibleVideo(video) {
		copyVideo, err = safeSourceBitrate(ctx, job, duration)
		if err != nil {
			copyVideo = false
		}
	}
	result.Action = "encode"
	if copyVideo {
		result.Action = "remux"
		if !compatibleAudio(audio) {
			result.Action = "audio-only"
		}
	}
	// Check enough space for the upper bitrate budget plus 1 GiB safety margin.
	required := uint64(duration*18_000_000/8) + 1<<30
	if !availableSpace(w.Output, required) {
		return fail("failed", "insufficient free disk space")
	}
	stage := filepath.Join(stageDir, "video.mp4")
	result.State = "preparing"
	if err = w.saveResult(job, result); err != nil {
		return err
	}
	if err = w.runFFmpeg(ctx, job, encodeArgs(job.Source, stage, video, audio, copyVideo), duration, &result); err != nil {
		if w.cancelled(job) || w.superseded(job) {
			return fail("cancelled", "preparation cancelled or superseded")
		}
		return fail("failed", "FFmpeg failed; check GPU access and source compatibility")
	}
	result.State = "validating"
	result.Progress = 100
	if err = w.saveResult(job, result); err != nil {
		return err
	}
	if err = validateOutput(ctx, stage, duration); err != nil {
		return fail("failed", "output validation failed")
	}
	if err = w.stageSubtitles(ctx, job, probe, stageDir); err != nil {
		return fail("failed", "subtitle preparation failed")
	}
	if !sourceUnchanged(job, w.Media) {
		return fail("failed", "source changed during preparation")
	}
	if w.cancelled(job) || w.superseded(job) {
		return fail("cancelled", "preparation cancelled or superseded")
	}
	if err = os.Rename(stage, destination); err != nil {
		return fail("failed", "publication failed")
	}
	result.Published = true
	result.State = "indexing"
	if err = w.saveResult(job, result); err != nil {
		return err
	}
	if err = publishSidecars(stageDir, destination); err != nil {
		return fail("failed", "video published but subtitle publication failed")
	}
	result.State = "indexing"
	if err = w.saveResult(job, result); err != nil {
		return err
	}
	if err = w.ensureIndexed(ctx, job); err != nil {
		return fail("failed", "video published; Jellyfin indexing failed")
	}
	result.State = "ready"
	return w.saveResult(job, result)
}
func (w *Worker) stageSubtitles(ctx context.Context, job PrepareJob, probe MediaProbe, dir string) error {
	stager := *w
	stager.Output = dir
	stagedJob := job
	stagedJob.Output = "video.mp4"
	return stager.syncSubtitles(ctx, stagedJob, probe)
}
func publishSidecars(dir, destination string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	base := strings.TrimSuffix(destination, filepath.Ext(destination))
	published := map[string]bool{}
	// Video is atomic; its sidecar set is published file-by-file before index refresh.
	// Versioned directory swaps would be needed for a fully atomic multi-file set.
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "video.") || !strings.EqualFold(filepath.Ext(name), ".srt") {
			continue
		}
		target := base + strings.TrimPrefix(name, "video")
		if err = os.Rename(filepath.Join(dir, name), target); err != nil {
			return err
		}
		published[filepath.Base(target)] = true
	}
	old, err := os.ReadDir(filepath.Dir(destination))
	if err != nil {
		return err
	}
	for _, entry := range old {
		name := entry.Name()
		if strings.HasPrefix(name, filepath.Base(base)+".") && strings.EqualFold(filepath.Ext(name), ".srt") && !published[name] {
			if err = os.Remove(filepath.Join(filepath.Dir(destination), name)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (w *Worker) syncSidecars(job PrepareJob) (bool, error) {
	destination, err := confined(w.Output, job.Output)
	if err != nil {
		return false, err
	}
	changed := false
	base := strings.TrimSuffix(job.Source, filepath.Ext(job.Source))
	outbase := strings.TrimSuffix(destination, filepath.Ext(destination))
	entries, err := os.ReadDir(filepath.Dir(job.Source))
	if err != nil {
		return false, err
	}
	for _, entry := range entries {
		source := filepath.Join(filepath.Dir(job.Source), entry.Name())
		if !strings.HasPrefix(source, base+".") || !strings.EqualFold(filepath.Ext(source), ".srt") {
			continue
		}
		safe, err := sourcePath(w.Media, source)
		if err != nil {
			return changed, err
		}
		st, err := os.Stat(safe)
		if err != nil {
			return changed, err
		}
		if !st.Mode().IsRegular() || st.Size() > 10<<20 {
			continue
		}
		data, err := os.ReadFile(safe)
		if err != nil {
			return changed, err
		}
		target := outbase + strings.TrimPrefix(source, base)
		existing, _ := os.ReadFile(target)
		if bytes.Equal(existing, data) {
			continue
		}
		if err = atomicWrite(target, data); err != nil {
			return changed, err
		}
		if err = os.Chmod(target, 0644); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}
func (w *Worker) syncSubtitles(ctx context.Context, job PrepareJob, probe MediaProbe) error {
	if _, err := w.syncSidecars(job); err != nil {
		return err
	}
	destination, err := confined(w.Output, job.Output)
	if err != nil {
		return err
	}
	outbase := strings.TrimSuffix(destination, filepath.Ext(destination))
	for _, stream := range probe.Streams {
		if stream.Type != "subtitle" || (stream.Codec != "subrip" && stream.Codec != "mov_text") {
			continue
		}
		// Preserve styled/image subtitles in originals; never burn them into optimized video.
		language := strings.ToLower(stream.Tags["language"])
		if len(language) < 2 || len(language) > 3 || strings.IndexFunc(language, func(r rune) bool { return r < 'a' || r > 'z' }) >= 0 {
			language = "und"
		}
		subtitle := fmt.Sprintf("%s.%s.track%d.srt", outbase, language, stream.Index)
		tmp := subtitle + ".tmp"
		cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error", "-nostdin", "-y", "-i", job.Source, "-map", fmt.Sprintf("0:%d", stream.Index), "-c:s", "srt", "-f", "srt", tmp)
		cmd.Stderr = io.Discard
		if cmd.Run() != nil {
			_ = os.Remove(tmp)
			return errors.New("subtitle extraction failed")
		}
		if err = os.Rename(tmp, subtitle); err != nil {
			return err
		}
	}
	return nil
}
func (w *Worker) jf(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(w.JellyfinURL, "/")+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "MediaBrowser Token="+strconv.Quote(w.JellyfinKey))
	response, err := w.Client.Do(req)
	if err != nil {
		return errors.New("Jellyfin unavailable")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return errors.New("Jellyfin request failed")
	}
	if out != nil {
		return json.NewDecoder(io.LimitReader(response.Body, 10<<20)).Decode(out)
	}
	return nil
}
func (w *Worker) checkLibrary(ctx context.Context, job PrepareJob) error {
	var libraries []struct {
		Locations []string `json:"Locations"`
	}
	if err := w.jf(ctx, "GET", "/Library/VirtualFolders", &libraries); err != nil {
		return err
	}
	kind := strings.Split(job.Output, string(os.PathSeparator))[0]
	if kind != "films" && kind != "shows" {
		return errors.New("unknown output library")
	}
	expected := filepath.Join("/optimized", kind)
	for _, library := range libraries {
		for _, path := range library.Locations {
			if filepath.Clean(path) == expected {
				return nil
			}
		}
	}
	return errors.New("streaming library missing")
}
func (w *Worker) ensureIndexed(ctx context.Context, job PrepareJob) error {
	if err := w.jf(ctx, "POST", "/Library/Refresh", nil); err != nil {
		return err
	}
	// Jellyfin sees the output bind mount at /optimized too.
	expected := filepath.Join("/optimized", job.Output)
	actual, err := confined(w.Output, job.Output)
	if err != nil {
		return err
	}
	st, err := os.Stat(actual)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 60; attempt++ {
		// Path is not a supported Jellyfin item filter. Paginate the video library;
		// scope to configured library IDs if very large libraries make this expensive.
		for start := 0; start < 1_000_000; start += 200 {
			var response struct {
				Items []struct {
					Path    string `json:"Path"`
					Sources []struct {
						Path string `json:"Path"`
						Size int64  `json:"Size"`
					} `json:"MediaSources"`
				} `json:"Items"`
				Total int `json:"TotalRecordCount"`
			}
			query := url.Values{"Recursive": {"true"}, "Fields": {"Path,MediaSources"}, "IncludeItemTypes": {"Movie,Episode"}, "Limit": {"200"}, "StartIndex": {strconv.Itoa(start)}}
			if err := w.jf(ctx, "GET", "/Items?"+query.Encode(), &response); err != nil {
				return err
			}
			for _, item := range response.Items {
				if filepath.Clean(item.Path) != expected {
					continue
				}
				// Size catches ordinary upgrade metadata lag; same-size replacements
				// would need a stronger freshness marker if exact identity matters.
				for _, source := range item.Sources {
					if filepath.Clean(source.Path) == expected && source.Size == st.Size() {
						return nil
					}
				}
			}
			if len(response.Items) < 200 || start+200 >= response.Total {
				break
			}
			if start+200 >= 1_000_000 {
				return errors.New("Jellyfin library exceeds scan limit")
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return errors.New("Jellyfin has not indexed the output")
}
func (w *Worker) tick() error {
	entries, err := os.ReadDir(filepath.Join(w.Queue, "requests"))
	if err != nil {
		return err
	}
	jobs := []PrepareJob{}
	for _, entry := range entries {
		id := strings.TrimSuffix(entry.Name(), ".json")
		if !validJobID(id) || entry.Name() != id+".json" {
			continue
		}
		var job PrepareJob
		if readJSON(filepath.Join(w.Queue, "requests", entry.Name()), &job) != nil || job.ID != id {
			continue
		}
		jobs = append(jobs, job)
	}
	sort.Slice(jobs, func(i, j int) bool { return jobs[i].Created < jobs[j].Created })
	for _, job := range jobs {
		if w.cancelled(job) || w.superseded(job) {
			if err := w.saveResult(job, PrepareResult{State: "cancelled", Detail: "cancelled or superseded"}); err != nil {
				return err
			}
			continue
		}
		var result PrepareResult
		if readJSON(filepath.Join(w.Queue, "results", job.ID+".json"), &result) == nil && result.Generation == job.Generation {
			if result.State == "ready" {
				destination, err := confined(w.Output, job.Output)
				if err != nil {
					return err
				}
				if _, err := os.Stat(destination); os.IsNotExist(err) {
					return w.process(job)
				}
				if sourceUnchanged(job, w.Media) {
					changed, err := w.syncSidecars(job)
					if err != nil {
						return err
					}
					if changed {
						if err = w.jf(context.Background(), "POST", "/Library/Refresh", nil); err != nil {
							result.State = "indexing"
							if saveErr := w.saveResult(job, result); saveErr != nil {
								return saveErr
							}
							return err
						}
					}
				}
				continue
			}
			if result.State == "failed" || result.State == "blocked" || result.State == "cancelled" {
				continue
			}
		}
		// Single worker, one encode at a time. Run separate queues/GPU workers if throughput matters.
		return w.process(job)
	}
	return nil
}

// Single host/GPU lock; separate queues and locks if multiple GPUs are introduced.
func lockWorker(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
}
func workerFromEnv() (*Worker, error) {
	w := &Worker{Queue: "/preparation", Media: "/downloads", Output: "/optimized", JellyfinURL: strings.TrimSpace(os.Getenv("JELLYFIN_URL")), JellyfinKey: strings.TrimSpace(os.Getenv("JELLYFIN_API_KEY")), Client: &http.Client{Timeout: 30 * time.Second}}
	if w.JellyfinURL == "" || w.JellyfinKey == "" {
		return nil, errors.New("worker needs JELLYFIN_URL and JELLYFIN_API_KEY")
	}
	if err := prepareDirs(w.Queue); err != nil {
		return nil, err
	}
	return w, nil
}
