package main

import (
	"encoding/base32"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

func parseMagnet(link string) (string, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "magnet" || u.Host != "" || len(link) > 12000 {
		return "", fmt.Errorf("invalid magnet")
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return "", err
	}
	for _, xt := range q["xt"] {
		if !strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
			continue
		}
		hash := xt[len("urn:btih:"):]
		if len(hash) == 40 {
			if _, err = hex.DecodeString(hash); err == nil {
				return strings.ToLower(hash), nil
			}
		}
		if len(hash) == 32 {
			if raw, e := base32.StdEncoding.DecodeString(strings.ToUpper(hash)); e == nil {
				return hex.EncodeToString(raw), nil
			}
		}
	}
	return "", fmt.Errorf("magnet needs a v1 info hash")
}

func (b *Bot) setMagnet(chat, user int64, text string) error {
	parts := strings.SplitN(strings.TrimSpace(text), " ", 3)
	if len(parts) != 3 {
		return b.send(chat, "Use /set REQUEST_ID magnet:?xt=urn:btih:… or choose Provide magnet link from My requests.", nil)
	}
	n, r := b.request(chat, parts[1])
	if r == nil {
		return b.send(chat, "Request not found in this chat.", nil)
	}
	link := strings.TrimSpace(parts[2])
	if _, err := parseMagnet(link); err != nil {
		return b.send(chat, "Invalid magnet link. Include xt=urn:btih: with a 40-character hex or 32-character base32 hash.", nil)
	}
	if b.cfg.TransmissionURL == "" {
		return b.send(chat, "Transmission is not configured.", nil)
	}
	for key, v := range b.pending {
		if v.AwaitMagnet && v.User == user && v.Chat == chat {
			delete(b.pending, key)
		}
	}
	key, v, err := b.cleanupSelection(chat, user)
	if err != nil {
		return err
	}
	v.RequestNumber = n
	v.Magnet = link
	u, _ := url.Parse(link)
	name := u.Query().Get("dn")
	if name == "" {
		name = "Unnamed torrent"
	}
	name = string([]rune(name)[:min(500, len([]rune(name)))])
	return b.send(chat, fmt.Sprintf("Download this manually selected torrent for #%d %s?\n%s\nThis bypasses indexer quality/language checks. Verify the content yourself. Existing library files stay until import. Movie files are explicitly mapped to this item; series episodes must match the selected season. Ambiguous packs require manual import in ARR.", n, r.Label, name), [][]map[string]string{{{"text": "Download magnet", "callback_data": "magnetadd:" + key}, {"text": "Cancel", "callback_data": "cancel:" + key}}})
}

func (b *Bot) addMagnet(v *Selection) error {
	_, r := b.request(v.Chat, fmt.Sprint(v.RequestNumber))
	if r == nil {
		return b.send(v.Chat, "Request is no longer active.", nil)
	}
	if b.cleanupPending(r.Kind, r.ItemID, r.Season) {
		return b.send(v.Chat, "Cleanup is pending; wait before downloading.", nil)
	}
	for _, existing := range b.state.Requests {
		if !existing.Cancelled && existing.Kind == r.Kind && existing.ItemID == r.ItemID && existing.Season == r.Season && existing.ManualHash != "" {
			return b.send(v.Chat, "A manual torrent is already tracked for this item. Check Downloads or ARR before adding another.", nil)
		}
	}
	rows, err := b.queue(r.Kind)
	if err != nil {
		return err
	}
	for _, row := range rows {
		match, e := queueMatches(*r, row)
		if e != nil {
			return e
		}
		if match {
			return b.send(v.Chat, "This item already has an ARR download. Cancel it first to avoid duplicates; existing files are kept.", nil)
		}
	}
	var result struct {
		Added struct {
			Hash string `json:"hashString"`
		} `json:"torrent-added"`
		Duplicate map[string]any `json:"torrent-duplicate"`
	}
	if err = b.transmissionRPC("torrent-add", map[string]any{"filename": v.Magnet, "paused": false}, &result); err != nil {
		return err
	}
	if result.Duplicate != nil {
		return b.send(v.Chat, "That torrent already exists in Transmission. No duplicate or import mapping was added; check ARR/Transmission.", nil)
	}
	if result.Added.Hash == "" {
		return fmt.Errorf("Transmission response missing hash")
	}
	r.ManualHash = strings.ToLower(result.Added.Hash)
	r.ManualImport = false
	if err = b.save(); err != nil {
		return err
	}
	return b.send(v.Chat, "Manual torrent added. I’ll track completion and ask ARR to copy/import matching media, keeping the torrent data for seeding. Check Downloads for status.", nil)
}

// A manual magnet has no ARR grab history. Explicitly map the completed files
// through ARR's manual-import API rather than relying on the torrent's title.
func (b *Bot) pollMagnets() error {
	for i := range b.state.Requests {
		r := &b.state.Requests[i]
		if r.Cancelled || r.ManualHash == "" || r.ManualImport {
			continue
		}
		var result struct {
			Torrents []struct {
				Name string  `json:"name"`
				Dir  string  `json:"downloadDir"`
				Done float64 `json:"percentDone"`
			} `json:"torrents"`
		}
		if err := b.transmissionRPC("torrent-get", map[string]any{"ids": []string{r.ManualHash}, "fields": []string{"name", "downloadDir", "percentDone"}}, &result); err != nil {
			return err
		}
		if len(result.Torrents) == 0 || result.Torrents[0].Done < 1 {
			continue
		}
		t := result.Torrents[0]
		folder := filepath.Join(t.Dir, filepath.Base(t.Name))
		if !strings.HasPrefix(folder, "/downloads/") {
			return fmt.Errorf("manual torrent outside ARR downloads mount")
		}
		// Passing downloadId requires ARR grab history, which manual torrents lack.
		// Passing movieId without it scans the existing library instead of this folder.
		path := "/manualimport?folder=" + url.QueryEscape(folder) + "&filterExistingFiles=true"
		var files []map[string]any
		if err := b.arr(r.Kind, path, nil, "", &files); err != nil {
			return err
		}
		if r.Kind == "movie" && len(files) > 0 {
			items := []map[string]any{}
			for _, f := range files {
				items = append(items, map[string]any{"path": f["path"], "movieId": r.ItemID, "quality": f["quality"], "languages": f["languages"], "releaseGroup": f["releaseGroup"]})
			}
			// Reprocessing is read-only: map even an unparseable/Cyrillic release name
			// to the selected movie, retaining ARR's sample/corruption checks.
			if err := b.arr(r.Kind, "/manualimport", items, "POST", &files); err != nil {
				return err
			}
		}
		selected := []map[string]any{}
		for _, f := range files {
			// Never force samples, corrupt files or other rejected candidates into the library.
			if len(maps(f["rejections"])) > 0 {
				continue
			}
			file := map[string]any{"path": f["path"], "quality": f["quality"], "languages": f["languages"], "releaseGroup": f["releaseGroup"], "downloadId": r.ManualHash}
			if r.Kind == "movie" {
				file["movieId"] = r.ItemID
			} else {
				series, _ := f["series"].(map[string]any)
				if number(series["id"]) != r.ItemID {
					continue
				}
				ids := []int{}
				for _, ep := range maps(f["episodes"]) {
					if number(ep["seasonNumber"]) == r.Season {
						ids = append(ids, number(ep["id"]))
					}
				}
				if len(ids) == 0 || len(ids) != len(maps(f["episodes"])) {
					continue
				}
				file["seriesId"] = r.ItemID
				file["episodeIds"] = ids
			}
			selected = append(selected, file)
		}
		// Consume before submitting: a timed-out import may have succeeded; never repeat
		// automatically. Persist command receipts if automatic reconciliation is needed.
		r.ManualImport = true
		if err := b.save(); err != nil {
			return err
		}
		if len(selected) == 0 || (r.Kind == "movie" && len(selected) != 1) {
			if err := b.send(r.Chat, fmt.Sprintf("#%d %s: manual torrent finished, but files are ambiguous or rejected. Review Manual Import in ARR; no files were deleted or forcibly imported.", i+1, r.Label), nil); err != nil {
				return err
			}
			continue
		}
		if err := b.arr(r.Kind, "/command", map[string]any{"name": "ManualImport", "files": selected, "importMode": "copy"}, "", nil); err != nil {
			return err
		}
		if err := b.send(r.Chat, fmt.Sprintf("#%d %s: manual torrent finished; ARR import queued. The library and streaming copy will update after import.", i+1, r.Label), nil); err != nil {
			return err
		}
	}
	return nil
}

func (b *Bot) magnetReply(chat, user int64, text string) (bool, error) {
	if !strings.HasPrefix(text, "magnet:") {
		return false, nil
	}
	for key, v := range b.pending {
		if v.AwaitMagnet && v.Chat == chat && v.User == user && v.Expires.After(time.Now()) {
			delete(b.pending, key)
			return true, b.setMagnet(chat, user, fmt.Sprintf("/set %d %s", v.RequestNumber, text))
		}
	}
	return true, b.send(chat, "Choose Provide magnet link from My requests first, or use /set REQUEST_ID MAGNET_LINK.", nil)
}
