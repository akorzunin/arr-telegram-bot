package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (b *Bot) queue(kind string) ([]map[string]any, error) {
	var rows []map[string]any
	for page := 1; page <= 100; page++ {
		var response struct {
			Records []map[string]any `json:"records"`
			Total   int              `json:"totalRecords"`
		}
		path := fmt.Sprintf("/queue?page=%d&pageSize=100&includeMovie=true&includeSeries=true&includeEpisode=true", page)
		if err := b.arr(kind, path, nil, "", &response); err != nil {
			return nil, err
		}
		rows = append(rows, response.Records...)
		if len(rows) >= response.Total || len(response.Records) < 100 {
			return rows, nil
		}
	}
	return nil, errors.New("queue exceeds page limit")
}

func queueMatches(req RequestRecord, row map[string]any) (bool, error) {
	if req.Kind == "movie" {
		return number(row["movieId"]) == req.ItemID, nil
	}
	if number(row["seriesId"]) != req.ItemID {
		return false, nil
	}
	if episode, ok := row["episode"].(map[string]any); ok && episode["seasonNumber"] != nil {
		return number(episode["seasonNumber"]) == req.Season, nil
	}
	if row["seasonNumber"] != nil {
		return number(row["seasonNumber"]) == req.Season, nil
	}
	// Fail closed: a row without season metadata might be a multi-season pack.
	return false, errors.New("cannot identify queue season; inspect Sonarr")
}

func (b *Bot) active(chat int64) error {
	queues := map[string][]map[string]any{}
	var output []string
	// Linear scan per saved request; index by item/season if histories grow to thousands.
	for i, req := range b.state.Requests {
		if req.Chat != chat || req.Cancelled {
			continue
		}
		rows, ok := queues[req.Kind]
		if !ok {
			var err error
			rows, err = b.queue(req.Kind)
			if err != nil {
				return err
			}
			queues[req.Kind] = rows
		}
		lines := []string{}
		seen := map[string]bool{}
		for _, row := range rows {
			matches, err := queueMatches(req, row)
			if err != nil {
				return err
			}
			if !matches {
				continue
			}
			if hash, ok := row["downloadId"].(string); ok && hash != "" {
				hash = strings.ToLower(hash)
				if seen[hash] {
					continue
				}
				seen[hash] = true
			}
			progress := ""
			if size, ok := row["size"].(float64); ok && size > 0 {
				if left, ok := row["sizeleft"].(float64); ok {
					progress = fmt.Sprintf(" %.0f%%", 100*(size-left)/size)
				}
			}
			lines = append(lines, fmt.Sprintf("%s — %s%s", row["title"], row["status"], progress))
		}
		if len(lines) == 0 {
			complete, err := b.requestComplete(req)
			if err != nil {
				return err
			}
			if complete {
				if b.cfg.PreparationDir == "" {
					continue
				}
				if len(req.Preparation) == 0 {
					lines = append(lines, "Downloaded · waiting for compatibility check")
				} else {
					allReady := true
					for _, ref := range req.Preparation {
						if b.preparationResult(ref).State != "ready" {
							allReady = false
							break
						}
					}
					if allReady {
						continue
					}
				}
			} else {
				lines = append(lines, "No active torrent: search pending or no eligible release found.")
			}
		}
		for _, ref := range req.Preparation {
			result := b.preparationResult(ref)
			status := result.State
			if result.State == "preparing" {
				status = fmt.Sprintf("%s · %d%%", result.Action, result.Progress)
			}
			if result.Detail != "" {
				status += " · " + result.Detail
			}
			if result.Warning != "" {
				status += " · " + result.Warning
			}
			lines = append(lines, ref.Label+" — "+status)
		}
		label := req.Label
		if req.Kind == "series" {
			label += fmt.Sprintf(" — season %d", req.Season)
		}
		output = append(output, fmt.Sprintf("#%d %s\n/cancel %d", i+1, label, i+1))
		output = append(output, lines...)
	}
	if len(output) == 0 {
		return b.send(chat, "No active bot requests in this chat. Downloads added outside the bot are managed in ARR/Transmission.", nil)
	}
	chunk := ""
	for _, line := range output {
		if len([]rune(line)) > 3500 {
			line = string([]rune(line)[:3500])
		}
		if len([]rune(chunk))+len([]rune(line))+1 > 3500 {
			if err := b.send(chat, chunk, nil); err != nil {
				return err
			}
			// Synchronous per-chat pacing; use queued sends if huge lists block polling.
			time.Sleep(time.Second)
			chunk = ""
		}
		if chunk != "" {
			chunk += "\n"
		}
		chunk += line
	}
	return b.send(chat, chunk, nil)
}

func (b *Bot) requestComplete(req RequestRecord) (bool, error) {
	if req.Kind == "movie" {
		var item map[string]any
		err := b.arr(req.Kind, fmt.Sprintf("/%d", req.ItemID), nil, "", &item)
		if err != nil && err.Error() == "HTTP 404" {
			return true, nil // Deleted from ARR; no active request to display.
		}
		return item["hasFile"] == true, err
	}
	var episodes []map[string]any
	if err := b.arr("series", fmt.Sprintf("/episode?seriesId=%d", req.ItemID), nil, "", &episodes); err != nil {
		return false, err
	}
	found := false
	for _, ep := range episodes {
		if number(ep["seasonNumber"]) == req.Season {
			found = true
			if ep["hasFile"] != true {
				return false, nil
			}
		}
	}
	return found, nil
}

func (b *Bot) confirmCancellation(chat, user int64, text string) error {
	parts := strings.Fields(text)
	n := 0
	if len(parts) == 2 {
		n, _ = strconv.Atoi(parts[1])
	}
	if n <= 0 || n > len(b.state.Requests) || b.state.Requests[n-1].Chat != chat || b.state.Requests[n-1].Cancelled {
		return b.send(chat, "Use /active, then /cancel NUMBER for one of your requests.", nil)
	}
	req := b.state.Requests[n-1]
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return err
	}
	key := hex.EncodeToString(buf)
	for k, v := range b.pending {
		if v.User == user || !v.Expires.After(time.Now()) {
			delete(b.pending, k)
		}
	}
	b.pending[key] = &Selection{User: user, Chat: chat, Expires: time.Now().Add(15 * time.Minute), RequestNumber: n}
	scope := "this movie"
	if req.Kind == "series" {
		scope = fmt.Sprintf("season %d only", req.Season)
	}
	return b.send(chat, fmt.Sprintf("Cancel %s (%s)? Removes its torrents, keeps files, and disables monitoring. This affects the shared ARR item, including other users requesting the same scope. An already-running search may still finish; check ARR afterwards.", req.Label, scope), [][]map[string]string{{{"text": "Confirm cancellation", "callback_data": "stop:" + key}, {"text": "Keep request", "callback_data": "cancel:" + key}}})
}

func (b *Bot) cancelRequest(chat int64, n int) error {
	if n <= 0 || n > len(b.state.Requests) || b.state.Requests[n-1].Chat != chat {
		return errors.New("invalid request")
	}
	req := b.state.Requests[n-1]
	if req.Cancelled {
		return b.send(chat, "Already cancelled.", nil)
	}
	rows, err := b.queue(req.Kind)
	if err != nil {
		return err
	}
	var selected []map[string]any
	hashes := map[string]bool{}
	for _, row := range rows {
		match, err := queueMatches(req, row)
		if err != nil {
			return err
		}
		if match {
			selected = append(selected, row)
			if hash, ok := row["downloadId"].(string); ok && hash != "" {
				client, _ := row["downloadClient"].(string)
				if row["downloadClientType"] != "Transmission" && !strings.HasPrefix(client, "Transmission") {
					return b.send(chat, "Cannot safely cancel: this download is not managed by Transmission. Use ARR's queue.", nil)
				}
				hashes[strings.ToLower(hash)] = true
			}
		}
	}
	// Queue-only scope check; inspect Transmission's file list if untracked pack contents matter.
	for _, row := range rows {
		hash, _ := row["downloadId"].(string)
		if hashes[strings.ToLower(hash)] {
			match, err := queueMatches(req, row)
			if err != nil || !match {
				return b.send(chat, "Cannot cancel a torrent shared with another movie/season. Manage that pack in ARR/Transmission; nothing was changed.", nil)
			}
		}
	}
	var item map[string]any
	if err := b.arr(req.Kind, fmt.Sprintf("/%d", req.ItemID), nil, "", &item); err != nil {
		return err
	}
	if req.Kind == "movie" {
		item["monitored"] = false
	} else {
		found := false
		for _, season := range seasons(item) {
			if number(season["seasonNumber"]) == req.Season {
				season["monitored"] = false
				found = true
			}
		}
		if !found {
			return errors.New("season no longer present")
		}
	}
	if err := b.arr(req.Kind, fmt.Sprintf("/%d", req.ItemID), item, "PUT", nil); err != nil {
		return err
	}
	if req.Kind == "series" {
		var episodes []map[string]any
		if err := b.arr("series", fmt.Sprintf("/episode?seriesId=%d", req.ItemID), nil, "", &episodes); err != nil {
			return err
		}
		ids := []int{}
		for _, ep := range episodes {
			if number(ep["seasonNumber"]) == req.Season {
				ids = append(ids, number(ep["id"]))
			}
		}
		if len(ids) > 0 {
			if err := b.arr("series", "/episode/monitor", map[string]any{"episodeIds": ids, "monitored": false}, "PUT", nil); err != nil {
				return err
			}
		}
	}
	ids := []string{}
	for hash := range hashes {
		ids = append(ids, hash)
	}
	if len(ids) > 0 {
		if err := b.removeTorrents(ids); err != nil {
			return fmt.Errorf("remove Transmission torrents: %w", err) // Keep a partial failure retryable.
		}
	}
	for _, row := range selected {
		if hash, ok := row["downloadId"].(string); ok && hash != "" {
			// ARR clears removed torrents on its next poll. Explicitly ignoring them
			// can crash IgnoreDownload after their tracking metadata disappears.
			continue
		}
		// Pending releases have no torrent; remove only their ARR delay-queue entry.
		path := fmt.Sprintf("/queue/%d?removeFromClient=false&blocklist=false&skipRedownload=true", number(row["id"]))
		if err := b.arr(req.Kind, path, nil, "DELETE", nil); err != nil && err.Error() != "HTTP 404" {
			return fmt.Errorf("remove pending ARR release: %w", err)
		}
	}
	for i := range b.state.Requests {
		r := &b.state.Requests[i]
		if r.Kind == req.Kind && r.ItemID == req.ItemID && (req.Kind == "movie" || r.Season == req.Season) {
			r.Cancelled = true
		}
	}
	if err := b.save(); err != nil {
		return err
	}
	if err := b.cancelUnusedPreparation(); err != nil {
		return err
	}
	return b.send(chat, "Cancelled. Monitoring disabled for this movie/season; torrent files were kept. Already-running searches may still finish: check ARR's queue.", nil)
}

func (b *Bot) removeTorrents(ids []string) error {
	body, _ := json.Marshal(map[string]any{"method": "torrent-remove", "arguments": map[string]any{"ids": ids, "delete-local-data": false}})
	session := ""
	for attempt := 0; attempt < 2; attempt++ {
		req, err := http.NewRequest("POST", b.cfg.TransmissionURL, bytes.NewReader(body))
		if err != nil {
			return errors.New("invalid Transmission endpoint")
		}
		req.Header.Set("Content-Type", "application/json")
		if session != "" {
			req.Header.Set("X-Transmission-Session-Id", session)
		}
		resp, err := b.client.Do(req)
		if err != nil {
			return errors.New("Transmission connection failed")
		}
		if resp.StatusCode == http.StatusConflict {
			session = resp.Header.Get("X-Transmission-Session-Id")
			resp.Body.Close()
			if session == "" {
				return errors.New("Transmission missing session ID")
			}
			continue
		}
		var result struct {
			Result string `json:"result"`
		}
		err = json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK || err != nil || result.Result != "success" {
			return errors.New("Transmission removal failed")
		}
		return nil
	}
	return errors.New("Transmission session negotiation failed")
}
