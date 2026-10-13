package main

import (
	"crypto/rand"
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

// Season -1 means the entire series. ARR entries are retained, unmonitored,
// so partial failures can be retried without losing their import history.
type CleanupScope struct {
	Kind   string `json:"kind"`
	ItemID int    `json:"item_id"`
	Season int    `json:"season"`
	Label  string `json:"label"`
}
type CleanupPlan struct {
	Scope        CleanupScope
	Item         map[string]any
	Files        []map[string]any
	Episodes     []map[string]any
	Torrents     []string
	TorrentPaths []string
	Output       string
	Date         time.Time
	Size         float64
}
type CleanupTask struct {
	ID     string       `json:"id"`
	Scope  CleanupScope `json:"scope"`
	Output string       `json:"output"`
}

func cleanupMatches(scope CleanupScope, kind string, id, season int) bool {
	return scope.Kind == kind && scope.ItemID == id && (kind == "movie" || scope.Season < 0 || scope.Season == season)
}
func (b *Bot) cleanupPending(kind string, id, season int) bool {
	for _, task := range b.state.Cleanups {
		if task.Scope.Kind == kind && task.Scope.ItemID == id && (kind == "movie" || task.Scope.Season < 0 || season < 0 || task.Scope.Season == season) {
			if _, err := os.Stat(filepath.Join(b.cfg.PreparationDir, "cleanup-done", task.ID)); err != nil {
				return true
			}
		}
	}
	return false
}
func cleanupToken() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
func (b *Bot) cleanupSelection(chat, user int64) (string, *Selection, error) {
	key, err := cleanupToken()
	if err != nil {
		return "", nil, err
	}
	v := &Selection{Chat: chat, User: user, Expires: time.Now().Add(15 * time.Minute)}
	b.pending[key] = v
	return key, v, nil
}

func (b *Bot) cleanupAuthorized(user int64) bool {
	users := b.cfg.CleanupAllowed
	if len(users) == 0 {
		users = b.cfg.Allowed
	}
	return users[user] // No cleanup admins configured means fail closed, not public deletion.
}

func (b *Bot) browseCleanup(chat, user int64, query string) error {
	if len([]rune(query)) > 200 {
		return b.send(chat, "Please use a title of at most 200 characters.", nil)
	}
	if err := b.restoreCleanupTasks(); err != nil {
		return err
	}
	if query == "/list" {
		query = ""
	}
	for key, v := range b.pending {
		if v.User == user || !v.Expires.After(time.Now()) {
			delete(b.pending, key)
		}
	}
	for _, task := range b.state.Cleanups {
		status := "pending (worker must be running)"
		if _, err := os.Stat(filepath.Join(b.cfg.PreparationDir, "cleanup-done", task.ID)); err == nil {
			status = "complete"
		}
		if err := b.send(chat, "Optimized cleanup: "+task.Scope.Label+" — "+status, nil); err != nil {
			return err
		}
	}
	// Keep only unfinished receipts; completion survives until this status is sent.
	remaining := []CleanupTask{}
	for _, task := range b.state.Cleanups {
		if _, err := os.Stat(filepath.Join(b.cfg.PreparationDir, "cleanup-done", task.ID)); err != nil {
			remaining = append(remaining, task)
		}
	}
	b.state.Cleanups = remaining
	if err := b.save(); err != nil {
		return err
	}
	season := -2
	if m := seasonPattern.FindStringSubmatchIndex(query); m != nil {
		fmt.Sscanf(query[m[2]:m[3]], "%d", &season)
		query = strings.TrimSpace(query[:m[0]])
	}
	whole := strings.HasSuffix(strings.ToLower(query), " all")
	if whole {
		query = strings.TrimSpace(query[:len(query)-4])
	}
	type dated struct {
		scope CleanupScope
		date  time.Time
	}
	entries := []dated{}
	for _, kind := range []string{"movie", "series"} {
		var items []map[string]any
		if err := b.arr(kind, "", nil, "", &items); err != nil {
			return err
		}
		for _, item := range items {
			title, _ := item["title"].(string)
			if !strings.Contains(strings.ToLower(title), strings.ToLower(query)) {
				continue
			}
			scope := CleanupScope{Kind: kind, ItemID: number(item["id"]), Season: -1, Label: title}
			if scope.ItemID <= 0 {
				continue
			}
			files := []map[string]any{}
			if kind == "movie" {
				if season >= 0 || whole {
					continue
				}
				if f, ok := item["movieFile"].(map[string]any); ok && number(f["id"]) > 0 {
					files = append(files, f)
				}
				if len(files) > 0 || query != "" {
					entries = append(entries, dated{scope, cleanupDate(files)})
				}
				continue
			}
			if err := b.arr(kind, fmt.Sprintf("/episodefile?seriesId=%d", scope.ItemID), nil, "", &files); err != nil {
				return err
			}
			if whole {
				scope.Label += " — ALL seasons"
				entries = append(entries, dated{scope, cleanupDate(files)})
				continue
			}
			for _, s := range seasons(item) {
				n := number(s["seasonNumber"])
				if season >= 0 && n != season {
					continue
				}
				selected := []map[string]any{}
				for _, f := range files {
					if f["seasonNumber"] != nil && number(f["seasonNumber"]) == n {
						selected = append(selected, f)
					}
				}
				if len(selected) == 0 && query == "" {
					continue
				}
				copy := scope
				copy.Season = n
				copy.Label += fmt.Sprintf(" — S%02d", n)
				entries = append(entries, dated{copy, cleanupDate(selected)})
			}
		}
	}
	sort.Slice(entries, func(i, j int) bool {
		a, c := entries[i], entries[j]
		if a.date.Equal(c.date) {
			return a.scope.Label < c.scope.Label
		}
		if a.date.IsZero() {
			return false
		}
		if c.date.IsZero() {
			return true
		}
		return a.date.Before(c.date)
	})
	if len(entries) == 0 {
		return b.send(chat, "No matching downloaded ARR movies/seasons. Use /cleanup TITLE, /cleanup TITLE S02, or /cleanup TITLE all (whole series).", nil)
	}
	key, v, err := b.cleanupSelection(chat, user)
	if err != nil {
		return err
	}
	v.CleanupList = []CleanupScope{}
	for _, entry := range entries {
		v.CleanupList = append(v.CleanupList, entry.scope)
	}
	return b.cleanupPage(key, v)
}
func cleanupDate(files []map[string]any) time.Time {
	var date time.Time
	for _, f := range files {
		d, err := time.Parse(time.RFC3339, field(f, "dateAdded"))
		if err == nil && (date.IsZero() || d.Before(date)) {
			date = d
		}
	}
	return date
}
func (b *Bot) cleanupPage(key string, v *Selection) error {
	const pageSize = 8
	start := v.CleanupPage * pageSize
	end := start + pageSize
	if end > len(v.CleanupList) {
		end = len(v.CleanupList)
	}
	buttons := [][]map[string]string{}
	text := fmt.Sprintf("Cleanup candidates — oldest ARR import first (page %d/%d).\nSelect to inspect date/size and confirm permanent deletion.\n/cleanup TITLE searches by name; append S02 or all.\nSizes are imported files, not guaranteed disk savings.\n", v.CleanupPage+1, (len(v.CleanupList)+pageSize-1)/pageSize)
	for _, scope := range v.CleanupList[start:end] {
		// Per-page ARR reads; cache details if large libraries make browsing slow.
		plan, err := b.cleanupFiles(scope)
		if err != nil {
			return err
		}
		date := "unknown date"
		if !plan.Date.IsZero() {
			date = plan.Date.Format("2006-01-02")
		}
		label := string([]rune(scope.Label)[:min(len([]rune(scope.Label)), 160)])
		text += fmt.Sprintf("%s · %.2f GiB · %s\n", date, plan.Size/(1<<30), label)
		itemKey, itemV, err := b.cleanupSelection(v.Chat, v.User)
		if err != nil {
			return err
		}
		itemV.Cleanup = plan
		buttons = append(buttons, []map[string]string{{"text": label, "callback_data": "cleanpick:" + itemKey}})
	}
	nav := []map[string]string{}
	if start > 0 {
		nav = append(nav, map[string]string{"text": "Previous", "callback_data": "cleanprev:" + key})
	}
	if end < len(v.CleanupList) {
		nav = append(nav, map[string]string{"text": "Next", "callback_data": "cleannext:" + key})
	}
	if len(nav) > 0 {
		buttons = append(buttons, nav)
	}
	return b.send(v.Chat, text, buttons)
}
func (b *Bot) cleanupCallback(action, key string, v *Selection) error {
	switch action {
	case "cancel":
		delete(b.pending, key)
		return b.send(v.Chat, "Cleanup cancelled; nothing deleted.", nil)
	case "cleanprev", "cleannext":
		if v.CleanupList == nil {
			return nil
		}
		if action == "cleanprev" && v.CleanupPage > 0 {
			v.CleanupPage--
		}
		if action == "cleannext" && (v.CleanupPage+1)*8 < len(v.CleanupList) {
			v.CleanupPage++
		}
		return b.cleanupPage(key, v)
	case "cleanpick":
		if v.Cleanup == nil {
			return nil
		}
		if b.cleanupPending(v.Cleanup.Scope.Kind, v.Cleanup.Scope.ItemID, v.Cleanup.Scope.Season) {
			return b.send(v.Chat, "Optimized cleanup is still pending. Check /list later.", nil)
		}
		plan, err := b.cleanupPlan(v.Cleanup.Scope)
		if err != nil {
			return b.send(v.Chat, "Cannot safely clean this scope: "+err.Error()+". Nothing was deleted; inspect ARR/Transmission manually.", nil)
		}
		v.Cleanup = plan
		if !b.cleanupAuthorized(v.User) {
			return b.send(v.Chat, fmt.Sprintf("%s: %.2f GiB imported files, %d associated torrents. Read-only: only cleanup admins can permanently delete media.", plan.Scope.Label, plan.Size/(1<<30), len(plan.Torrents)), nil)
		}
		v.Confirmed = true
		note := " Optimized cleanup is DISABLED (PREPARATION_DIR is unset); any existing optimized copies will remain."
		if b.cfg.PreparationDir != "" {
			note = " Optimized copies are deleted by the worker; it must be running. Check /list for completion."
		}
		return b.send(v.Chat, fmt.Sprintf("PERMANENTLY delete %s?\n%.2f GiB imported files + %d associated torrents and their data + optimized copies. Shared library: affects everyone. ARR entry stays, monitoring is disabled; other seasons stay unchanged. This cannot be undone.%s\nUntracked duplicates and ARR recycle-bin contents are not purged. Already-running searches may still finish; check ARR afterwards.", plan.Scope.Label, plan.Size/(1<<30), len(plan.Torrents), note), [][]map[string]string{{{"text": "Delete permanently", "callback_data": "cleanremove:" + key}, {"text": "Keep files", "callback_data": "cancel:" + key}}})
	case "cleanremove":
		if !b.cleanupAuthorized(v.User) {
			return b.send(v.Chat, "Permanent deletion is restricted to cleanup admins. Download requests and /list remain available.", nil)
		}
		if v.Cleanup == nil || !v.Confirmed {
			return nil
		}
		delete(b.pending, key)
		if b.cleanupPending(v.Cleanup.Scope.Kind, v.Cleanup.Scope.ItemID, v.Cleanup.Scope.Season) {
			return b.send(v.Chat, "Cleanup is already pending for this scope. Check /list later.", nil)
		}
		plan, err := b.cleanupPlan(v.Cleanup.Scope)
		if err != nil {
			return err
		}
		old, _ := json.Marshal(cleanupSnapshot(v.Cleanup))
		fresh, _ := json.Marshal(cleanupSnapshot(plan))
		if string(old) != string(fresh) {
			return b.send(v.Chat, "Files or torrents changed since confirmation. Nothing deleted; use /cleanup again.", nil)
		}
		return b.executeCleanup(v.Chat, plan)
	}
	return nil
}
func cleanupSnapshot(p *CleanupPlan) any {
	files := []string{}
	for _, f := range p.Files {
		files = append(files, fmt.Sprintf("%d:%v:%v:%v", number(f["id"]), f["path"], f["size"], f["dateAdded"]))
	}
	sort.Strings(files)
	return []any{p.Scope, p.Output, files, p.Torrents, p.TorrentPaths}
}
func (b *Bot) cleanupFiles(scope CleanupScope) (*CleanupPlan, error) {
	p := &CleanupPlan{Scope: scope}
	if err := b.arr(scope.Kind, fmt.Sprintf("/%d", scope.ItemID), nil, "", &p.Item); err != nil {
		return nil, err
	}
	dir, ok := p.Item["path"].(string)
	if !ok {
		return nil, errors.New("missing media path")
	}
	rel, err := filepath.Rel(b.app(scope.Kind).Root, dir)
	if err != nil {
		return nil, err
	}
	if _, err = confined(b.app(scope.Kind).Root, rel); err != nil || rel == "." {
		return nil, errors.New("unsafe media directory")
	}
	if scope.Kind == "movie" {
		p.Output = filepath.Join("films", rel)
		if f, ok := p.Item["movieFile"].(map[string]any); ok && number(f["id"]) > 0 {
			p.Files = append(p.Files, f)
		} else if p.Item["hasFile"] == true {
			return nil, errors.New("movie file metadata missing")
		}
	} else {
		p.Output = filepath.Join("shows", rel)
		if scope.Season >= 0 {
			p.Output = filepath.Join(p.Output, fmt.Sprintf("Season %02d", scope.Season))
		}
		if err := b.arr("series", fmt.Sprintf("/episode?seriesId=%d", scope.ItemID), nil, "", &p.Episodes); err != nil {
			return nil, err
		}
		var files []map[string]any
		if err := b.arr("series", fmt.Sprintf("/episodefile?seriesId=%d", scope.ItemID), nil, "", &files); err != nil {
			return nil, err
		}
		for _, f := range files {
			if number(f["id"]) <= 0 {
				return nil, errors.New("episode file ID missing")
			}
			if f["seasonNumber"] == nil {
				return nil, errors.New("unknown file season")
			}
			if scope.Season < 0 || number(f["seasonNumber"]) == scope.Season {
				p.Files = append(p.Files, f)
			}
		}
		// O(episodes × files); index by file ID if very large series make this costly.
		for _, ep := range p.Episodes {
			for _, f := range p.Files {
				if number(ep["episodeFileId"]) == number(f["id"]) && scope.Season >= 0 {
					if ep["seasonNumber"] == nil || number(ep["seasonNumber"]) != scope.Season {
						return nil, errors.New("episode file shared across seasons or missing metadata")
					}
				}
			}
		}
	}
	for _, f := range p.Files {
		path, ok := f["path"].(string)
		if !ok {
			path, err = confined(dir, field(f, "relativePath"))
			if err != nil {
				return nil, err
			}
			f["path"] = path
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return nil, err
		}
		if _, err = confined(dir, relative); err != nil {
			return nil, errors.New("unsafe file path")
		}
		size, _ := f["size"].(float64)
		p.Size += size
	}
	p.Date = cleanupDate(p.Files)
	return p, nil
}

func (b *Bot) cleanupPlan(scope CleanupScope) (*CleanupPlan, error) {
	p, err := b.cleanupFiles(scope)
	if err != nil {
		return nil, err
	}
	rows, err := b.queue(scope.Kind)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if number(row[scope.Kind+"Id"]) == scope.ItemID {
			return nil, errors.New("title has an ARR queue entry; cancel downloads first")
		}
	}
	// Full history on both apps proves scope ownership, including old seeding torrents.
	// Bounded scans fail closed; index history by hash if this becomes too expensive.
	histories := map[string][]map[string]any{}
	for _, kind := range []string{"movie", "series"} {
		for page := 1; page <= 100; page++ {
			records, err := b.history(kind, page)
			if err != nil {
				return nil, err
			}
			histories[kind] = append(histories[kind], records...)
			if len(records) < 100 {
				break
			}
			if page == 100 {
				return nil, errors.New("history exceeds safety limit")
			}
		}
	}
	paths := map[string]bool{}
	hashes := map[string]bool{}
	foreign := map[string]bool{}
	episodeSeason := map[int]int{}
	for _, ep := range p.Episodes {
		if number(ep["id"]) > 0 && ep["seasonNumber"] != nil {
			episodeSeason[number(ep["id"])] = number(ep["seasonNumber"])
		}
	}
	for _, f := range p.Files {
		paths[filepath.Clean(field(f, "path"))] = true
	}
	for kind, records := range histories {
		for _, r := range records {
			hash, _ := r["downloadId"].(string)
			hash = strings.ToLower(hash)
			if hash == "" {
				continue
			}
			match := kind == scope.Kind && number(r[kind+"Id"]) == scope.ItemID
			if match && kind == "series" && scope.Season >= 0 {
				n, ok := episodeSeason[number(r["episodeId"])]
				match = ok && n == scope.Season
			}
			if !match {
				foreign[hash] = true
				continue
			}
			hashes[hash] = true
			if r["eventType"] == "downloadFolderImported" {
				data, _ := r["data"].(map[string]any)
				path, _ := data["droppedPath"].(string)
				if path != "" {
					paths[filepath.Clean(path)] = true
				}
			}
		}
	}
	var response struct {
		Torrents []struct {
			Hash  string `json:"hashString"`
			Dir   string `json:"downloadDir"`
			Files []struct {
				Name string `json:"name"`
			} `json:"files"`
		} `json:"torrents"`
	}
	if err := b.transmissionRPC("torrent-get", map[string]any{"fields": []string{"hashString", "downloadDir", "files"}}, &response); err != nil {
		return nil, err
	}
	allPaths := map[string][]string{}
	selected := map[string]bool{}
	media := b.cfg.MediaRoot
	if media == "" {
		media = "/downloads"
	}
	for _, t := range response.Torrents {
		hash := strings.ToLower(t.Hash)
		match := hashes[hash]
		for _, f := range t.Files {
			path, err := confined(t.Dir, f.Name)
			if err != nil {
				return nil, errors.New("unsafe torrent path")
			}
			allPaths[path] = append(allPaths[path], hash)
			if paths[path] {
				match = true
			}
		}
		if !match {
			continue
		}
		if hash == "" || foreign[hash] || len(t.Files) == 0 {
			return nil, errors.New("torrent shared with another scope or missing metadata")
		}
		videos := 0
		for _, f := range t.Files {
			path, _ := confined(t.Dir, f.Name)
			rel, err := filepath.Rel(media, path)
			if err != nil {
				return nil, err
			}
			if _, err = confined(media, rel); err != nil {
				return nil, errors.New("torrent data outside media mount")
			}
			switch strings.ToLower(filepath.Ext(path)) {
			case ".mkv", ".mp4", ".avi", ".m4v", ".ts", ".webm", ".mov", ".m2ts":
				videos++
				if !paths[path] {
					return nil, errors.New("torrent contains unproven video files (possibly a mixed pack)")
				}
			case ".srt", ".ass", ".ssa", ".sub", ".idx", ".nfo", ".txt", ".jpg", ".jpeg", ".png":
			default:
				return nil, errors.New("torrent contains unrecognized files")
			}
		}
		if videos == 0 {
			return nil, errors.New("torrent has no proven video")
		}
		selected[hash] = true
	}
	for _, owners := range allPaths {
		for _, owner := range owners {
			if selected[owner] && len(owners) > 1 {
				return nil, errors.New("torrent data shared with another torrent")
			}
		}
	}
	for hash := range selected {
		p.Torrents = append(p.Torrents, hash)
	}
	for path, owners := range allPaths {
		if selected[owners[0]] {
			p.TorrentPaths = append(p.TorrentPaths, path)
		}
	}
	sort.Strings(p.Torrents)
	sort.Strings(p.TorrentPaths)
	return p, nil
}

func (b *Bot) executeCleanup(chat int64, p *CleanupPlan) error {
	scope := p.Scope
	if scope.Kind == "movie" || scope.Season < 0 {
		p.Item["monitored"] = false
	}
	if scope.Kind == "series" {
		for _, s := range seasons(p.Item) {
			if scope.Season < 0 || number(s["seasonNumber"]) == scope.Season {
				s["monitored"] = false
			}
		}
		// Prevent new seasons from bringing back a cleaned whole-series scope.
		if scope.Season < 0 {
			p.Item["monitorNewItems"] = "none"
		}
	}
	if err := b.arr(scope.Kind, fmt.Sprintf("/%d", scope.ItemID), p.Item, "PUT", nil); err != nil {
		return err
	}
	if scope.Kind == "series" {
		ids := []int{}
		for _, ep := range p.Episodes {
			if scope.Season < 0 || number(ep["seasonNumber"]) == scope.Season {
				ids = append(ids, number(ep["id"]))
			}
		}
		if len(ids) > 0 {
			if err := b.arr("series", "/episode/monitor", map[string]any{"episodeIds": ids, "monitored": false}, "PUT", nil); err != nil {
				return err
			}
		}
	}
	for i := range b.state.Requests {
		r := &b.state.Requests[i]
		if cleanupMatches(scope, r.Kind, r.ItemID, r.Season) {
			r.Cancelled = true
		}
	}
	if err := b.save(); err != nil {
		return err
	}
	if err := b.cancelUnusedPreparation(); err != nil {
		return err
	}
	if b.cfg.PreparationDir != "" {
		if err := cancelCleanupJobs(b.cfg.PreparationDir, p.Output); err != nil {
			return err
		}
	}
	if len(p.Torrents) > 0 {
		if err := b.transmissionRPC("torrent-remove", map[string]any{"ids": p.Torrents, "delete-local-data": true}, nil); err != nil {
			return err
		}
	}
	for _, f := range p.Files {
		resource := "/moviefile/"
		if scope.Kind == "series" {
			resource = "/episodefile/"
		}
		if err := b.arr(scope.Kind, resource+fmt.Sprint(number(f["id"])), nil, "DELETE", nil); err != nil && err.Error() != "HTTP 404" {
			return err
		}
	}
	note := ""
	if b.cfg.PreparationDir != "" {
		id, err := cleanupToken()
		if err != nil {
			return err
		}
		task := CleanupTask{ID: id, Scope: scope, Output: p.Output}
		// Save before publishing so a queue-write failure stays visible/retryable.
		b.state.Cleanups = append(b.state.Cleanups, task)
		if err := b.save(); err != nil {
			return err
		}
		if err := writeJSON(filepath.Join(b.cfg.PreparationDir, "cleanup", id+".json"), task); err != nil {
			return err
		}
		note = " Optimized-copy deletion queued in the worker; /list reports completion."
	}
	return b.send(chat, "Cleaned "+scope.Label+": imported files and associated torrent data deleted; monitoring disabled. ARR entry retained for safe retries."+note+" Untracked duplicates and ARR recycle-bin contents may still occupy space.", nil)
}

// Republish durable tasks after a crash between saving state and queue publication.
func (b *Bot) restoreCleanupTasks() error {
	if len(b.state.Cleanups) == 0 {
		return nil
	}
	if b.cfg.PreparationDir == "" {
		return errors.New("pending cleanup requires PREPARATION_DIR")
	}
	for _, task := range b.state.Cleanups {
		if !validCleanupTask(task) {
			return errors.New("invalid saved cleanup task")
		}
		path := filepath.Join(b.cfg.PreparationDir, "cleanup", task.ID+".json")
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if err := writeJSON(path, task); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	return nil
}

func outputInScope(output, dir string) bool {
	return output == dir || strings.HasPrefix(output, dir+string(filepath.Separator))
}
func cancelCleanupJobs(queue, output string) error {
	entries, err := os.ReadDir(filepath.Join(queue, "requests"))
	if err != nil {
		return err
	}
	for _, entry := range entries {
		var job PrepareJob
		if !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if err := readJSON(filepath.Join(queue, "requests", entry.Name()), &job); err != nil {
			return err
		}
		if !validJobID(job.ID) {
			return errors.New("invalid preparation job")
		}
		if outputInScope(job.Output, output) {
			if err := atomicWrite(filepath.Join(queue, "cancel", job.ID), []byte("cleanup")); err != nil {
				return err
			}
		}
	}
	return nil
}
