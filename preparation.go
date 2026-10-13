package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// One worker owns results; the bot owns requests/control files. Atomic renames
// keep polling readers from observing partial JSON. No shared mutable queue file.
type PrepareJob struct {
	ID         string `json:"id"`
	Source     string `json:"source"`
	Output     string `json:"output"`
	Size       int64  `json:"size"`
	Modified   int64  `json:"modified"`
	Created    int64  `json:"created"`
	Generation int64  `json:"generation"`
}
type PrepareResult struct {
	Generation int64  `json:"generation"`
	State      string `json:"state"`
	Action     string `json:"action,omitempty"`
	Progress   int    `json:"progress,omitempty"`
	Detail     string `json:"detail,omitempty"`
	Published  bool   `json:"published,omitempty"`
	Warning    string `json:"warning,omitempty"`
}
type PreparationRef struct {
	ID         string `json:"id"`
	Label      string `json:"label"`
	Output     string `json:"output"`
	Generation int64  `json:"generation"`
	Notified   bool   `json:"notified,omitempty"`
}

func prepareDirs(root string) error {
	for _, dir := range []string{"requests", "results", "cancel", "cleanup", "cleanup-done"} {
		if err := os.MkdirAll(filepath.Join(root, dir), 0700); err != nil {
			return err
		}
	}
	return nil
}
func writeJSON(path string, v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return atomicWrite(path, data)
}
func readJSON(path string, v any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}
func validJobID(id string) bool {
	_, err := hex.DecodeString(id)
	return len(id) == 64 && err == nil
}
func confined(root, relative string) (string, error) {
	if relative == "." || relative == "" || filepath.IsAbs(relative) || filepath.Clean(relative) != relative || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return "", errors.New("unsafe media path")
	}
	return filepath.Join(root, relative), nil
}
func sourcePath(root, path string) (string, error) {
	full, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	base, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(base, full)
	if err != nil {
		return "", err
	}
	return confined(base, rel)
}
func sourceUnchanged(job PrepareJob, root string) bool {
	path, err := sourcePath(root, job.Source)
	if err != nil {
		return false
	}
	st, err := os.Stat(path)
	return err == nil && st.Mode().IsRegular() && st.Size() == job.Size && st.ModTime().UnixNano() == job.Modified
}
func (b *Bot) recordRequest(record RequestRecord) error {
	if b.cfg.PreparationDir != "" {
		for i, existing := range b.state.Requests {
			if !existing.Cancelled && existing.Kind == record.Kind && existing.ItemID == record.ItemID && existing.Season == record.Season && existing.Chat == record.Chat {
				b.state.Requests[i].EpisodeIDs = record.EpisodeIDs
				return b.save()
			}
		}
	}
	b.state.Requests = append(b.state.Requests, record)
	return b.save()
}
func (b *Bot) preparationResult(ref PreparationRef) PrepareResult {
	var result PrepareResult
	if readJSON(filepath.Join(b.cfg.PreparationDir, "results", ref.ID+".json"), &result) != nil || result.Generation != ref.Generation {
		return PrepareResult{State: "queued"}
	}
	return result
}
func (b *Bot) enqueuePreparation(index int, path, output, label string) error {
	path, err := sourcePath(b.cfg.MediaRoot, path)
	if err != nil {
		return err
	}
	if _, err = confined("/optimized", output); err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return errors.New("not a media file")
	}
	// Metadata fingerprint assumes immutable ARR imports; use a content hash if
	// writers start changing files in place while preserving size and timestamps.
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", path, output, st.Size(), st.ModTime().UnixNano())))
	id := hex.EncodeToString(digest[:])
	req := &b.state.Requests[index]
	// Wall-clock supersession assumes a stable host clock; use a persistent producer
	// sequence if clock rewinds become an operational concern.
	job := PrepareJob{ID: id, Source: path, Output: output, Size: st.Size(), Modified: st.ModTime().UnixNano(), Created: time.Now().UnixNano(), Generation: time.Now().UnixNano()}
	var previous PrepareJob
	if err := readJSON(filepath.Join(b.cfg.PreparationDir, "requests", id+".json"), &previous); err == nil {
		job = previous
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for j := range req.Preparation {
		ref := &req.Preparation[j]
		if ref.ID == id {
			if ref.Generation != job.Generation {
				ref.Generation = job.Generation
				ref.Notified = false
				return b.save()
			}
			return nil
		}
	}
	if _, err := os.Stat(filepath.Join(b.cfg.PreparationDir, "cancel", id)); err == nil {
		job.Generation = time.Now().UnixNano()
		if err := os.Remove(filepath.Join(b.cfg.PreparationDir, "cancel", id)); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(b.cfg.PreparationDir, "requests", id+".json"), job); err != nil {
		return err
	}
	// A new source version replaces the old reference, but never removes a good output.
	refs := req.Preparation[:0]
	for _, ref := range req.Preparation {
		if ref.Output != output {
			refs = append(refs, ref)
		}
	}
	req.Preparation = append(refs, PreparationRef{ID: id, Label: label, Output: output, Generation: job.Generation})
	return b.save()
}
func (b *Bot) discoverPreparation(index int) error {
	req := b.state.Requests[index]
	if req.Kind == "movie" {
		var movie map[string]any
		if err := b.arr("movie", fmt.Sprintf("/%d", req.ItemID), nil, "", &movie); err != nil {
			if err.Error() == "HTTP 404" {
				return nil
			}
			return err
		}
		if movie["hasFile"] != true {
			return nil
		}
		file, ok := movie["movieFile"].(map[string]any)
		if !ok {
			return nil
		}
		dir, ok := movie["path"].(string)
		if !ok {
			return errors.New("movie path missing")
		}
		rel, ok := file["relativePath"].(string)
		if !ok {
			return errors.New("movie file path missing")
		}
		path, err := confined(dir, rel)
		if err != nil {
			return err
		}
		libraryRel, err := filepath.Rel(b.cfg.Movie.Root, dir)
		if err != nil {
			return err
		}
		if _, err = confined(b.cfg.Movie.Root, libraryRel); err != nil {
			return err
		}
		return b.enqueuePreparation(index, path, filepath.Join("films", libraryRel, filepath.Base(dir)+".mp4"), req.Label)
	}
	var series map[string]any
	if err := b.arr("series", fmt.Sprintf("/%d", req.ItemID), nil, "", &series); err != nil {
		if err.Error() == "HTTP 404" {
			return nil
		}
		return err
	}
	var episodes []map[string]any
	if err := b.arr("series", "/episode?seriesId="+fmt.Sprint(req.ItemID), nil, "", &episodes); err != nil {
		return err
	}
	byFile := map[int][]int{}
	for _, ep := range episodes {
		if number(ep["seasonNumber"]) != req.Season || ep["hasFile"] != true || (len(req.EpisodeIDs) > 0 && !contains(req.EpisodeIDs, number(ep["id"]))) {
			continue
		}
		fid := number(ep["episodeFileId"])
		if fid > 0 {
			byFile[fid] = append(byFile[fid], number(ep["episodeNumber"]))
		}
	}
	dir, ok := series["path"].(string)
	if !ok && len(byFile) > 0 {
		return errors.New("series path missing")
	}
	libraryRel, err := filepath.Rel(b.cfg.Series.Root, dir)
	if err != nil && len(byFile) > 0 {
		return err
	}
	for fid, nums := range byFile {
		if _, err = confined(b.cfg.Series.Root, libraryRel); err != nil {
			return err
		}
		var file map[string]any
		if err := b.arr("series", fmt.Sprintf("/episodefile/%d", fid), nil, "", &file); err != nil {
			return err
		}
		path, ok := file["path"].(string)
		if !ok {
			return errors.New("episode file path missing")
		}
		sort.Ints(nums)
		label := fmt.Sprintf("%s — S%02dE%02d", req.Label, req.Season, nums[0])
		basename := fmt.Sprintf("%s - S%02dE%02d", filepath.Base(dir), req.Season, nums[0])
		for _, n := range nums[1:] {
			basename += fmt.Sprintf("-E%02d", n)
			label += fmt.Sprintf("-E%02d", n)
		}
		output := filepath.Join("shows", libraryRel, fmt.Sprintf("Season %02d", req.Season), basename+".mp4")
		if err := b.enqueuePreparation(index, path, output, label); err != nil {
			return err
		}
	}
	return nil
}
func (b *Bot) pollPreparation() error {
	if b.cfg.PreparationDir == "" {
		return nil
	}
	if err := prepareDirs(b.cfg.PreparationDir); err != nil {
		return err
	}
	var firstErr error
	// Linear scan of requested titles each poll; index/cache ARR metadata if this grows to thousands.
	for i := range b.state.Requests {
		if b.state.Requests[i].Cancelled {
			continue
		}
		if err := b.discoverPreparation(i); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		}
		var pending []int
		var lines []string
		needsRetry := false
		for j, ref := range b.state.Requests[i].Preparation {
			result := b.preparationResult(ref)
			if ref.Notified || (result.State != "ready" && result.State != "failed" && result.State != "blocked") {
				continue
			}
			pending = append(pending, j)
			library := "Streaming Films"
			if b.state.Requests[i].Kind == "series" {
				library = "Streaming Shows"
			}
			line := ref.Label + " is ready in " + library + "."
			if result.Warning != "" {
				line += " " + result.Warning
			}
			if result.State != "ready" {
				line = ref.Label + ": original available; preparation needs attention (" + result.Detail + ")."
				needsRetry = true
			}
			// Summarize a season pack instead of sending one Telegram message per episode.
			if len(lines) < 8 {
				lines = append(lines, line)
			}
		}
		if len(pending) == 0 {
			continue
		}
		if len(pending) > len(lines) {
			lines = append(lines, fmt.Sprintf("%d more updates; see /active.", len(pending)-len(lines)))
		}
		buttons := any(nil)
		if needsRetry {
			lines = append(lines, fmt.Sprintf("After addressing the issue, use /retry %d.", i+1))
			buttons = [][]map[string]string{{{"text": "Retry preparation", "callback_data": fmt.Sprintf("retry:%d", i+1)}}}
		}
		text := strings.Join(lines, "\n")
		if len([]rune(text)) > 3500 {
			text = string([]rune(text)[:3500]) + "\nSee /active for details."
		}
		if err := b.send(b.state.Requests[i].Chat, text, buttons); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		delivered := map[string]int64{}
		for _, j := range pending {
			ref := b.state.Requests[i].Preparation[j]
			delivered[ref.ID] = ref.Generation
		}
		for ri := range b.state.Requests {
			if b.state.Requests[ri].Chat != b.state.Requests[i].Chat {
				continue
			}
			for rj := range b.state.Requests[ri].Preparation {
				ref := &b.state.Requests[ri].Preparation[rj]
				if generation, ok := delivered[ref.ID]; ok && generation == ref.Generation {
					ref.Notified = true
				}
			}
		}
		if err := b.save(); err != nil {
			return err
		}
	}
	return firstErr
}
func (b *Bot) retryPreparation(chat int64, n int) error {
	if b.cfg.PreparationDir == "" || n < 1 || n > len(b.state.Requests) || b.state.Requests[n-1].Chat != chat || b.state.Requests[n-1].Cancelled {
		return b.send(chat, "Use /active, then /retry NUMBER for your request.", nil)
	}
	req := &b.state.Requests[n-1]
	retried := false
	for i := range req.Preparation {
		ref := &req.Preparation[i]
		result := b.preparationResult(*ref)
		if result.State != "failed" && result.State != "blocked" && result.State != "cancelled" {
			continue
		}
		var job PrepareJob
		if err := readJSON(filepath.Join(b.cfg.PreparationDir, "requests", ref.ID+".json"), &job); err != nil {
			return err
		}
		job.Generation = time.Now().UnixNano()
		if err := writeJSON(filepath.Join(b.cfg.PreparationDir, "requests", ref.ID+".json"), job); err != nil {
			return err
		}
		// All requesters share the same physical job and retry generation.
		for ri := range b.state.Requests {
			for rj := range b.state.Requests[ri].Preparation {
				other := &b.state.Requests[ri].Preparation[rj]
				if other.ID == ref.ID {
					other.Generation = job.Generation
					other.Notified = false
				}
			}
		}
		retried = true
	}
	if err := b.save(); err != nil {
		return err
	}
	if !retried {
		return b.send(chat, "No failed preparation to retry.", nil)
	}
	return b.send(chat, "Streaming preparation queued again; no download was started.", nil)
}
func (b *Bot) cancelUnusedPreparation() error {
	if b.cfg.PreparationDir == "" {
		return nil
	}
	active := map[string]bool{}
	for _, req := range b.state.Requests {
		if !req.Cancelled {
			for _, ref := range req.Preparation {
				active[ref.ID] = true
			}
		}
	}
	for _, req := range b.state.Requests {
		if req.Cancelled {
			for _, ref := range req.Preparation {
				if !active[ref.ID] {
					if err := atomicWrite(filepath.Join(b.cfg.PreparationDir, "cancel", ref.ID), []byte("cancel")); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
