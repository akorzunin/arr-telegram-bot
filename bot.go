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
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type AppConfig struct {
	URL, Key, Root string
	Profile        int
}
type Config struct {
	Token                 string
	Allowed               map[int64]bool
	StateDir, TelegramURL string
	Movie, Series         AppConfig
}
type RequestRecord struct {
	Kind       string `json:"kind"`
	ItemID     int    `json:"item_id"`
	Season     int    `json:"season"`
	Chat       int64  `json:"chat"`
	Label      string `json:"label"`
	EpisodeIDs []int  `json:"episode_ids,omitempty"`
	Notified   []int  `json:"notified,omitempty"`
}
type State struct {
	Offset   int64           `json:"offset"`
	Cursor   map[string]int  `json:"cursor"`
	Requests []RequestRecord `json:"requests"`
}
type Selection struct {
	Kind       string
	Item       map[string]any
	User, Chat int64
	Expires    time.Time
	Label      string
	Season     int
	Confirmed  bool
}
type Bot struct {
	cfg     Config
	client  *http.Client
	state   State
	pending map[string]*Selection
}
type User struct {
	ID int64 `json:"id"`
}
type Chat struct {
	ID   int64  `json:"id"`
	Type string `json:"type"`
}
type Message struct {
	From User   `json:"from"`
	Chat Chat   `json:"chat"`
	Text string `json:"text"`
}
type Callback struct {
	ID      string  `json:"id"`
	From    User    `json:"from"`
	Message Message `json:"message"`
	Data    string  `json:"data"`
}
type Update struct {
	ID       int64     `json:"update_id"`
	Message  *Message  `json:"message,omitempty"`
	Callback *Callback `json:"callback_query,omitempty"`
}

func newBot(cfg Config, client *http.Client) *Bot {
	if client == nil {
		client = &http.Client{Timeout: 40 * time.Second}
	}
	return &Bot{cfg: cfg, client: client, state: State{Cursor: map[string]int{}}, pending: map[string]*Selection{}}
}
func (b *Bot) load() error {
	data, err := os.ReadFile(filepath.Join(b.cfg.StateDir, "notifications.json"))
	if err == nil {
		if err = json.Unmarshal(data, &b.state); err != nil {
			return fmt.Errorf("invalid notification state: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if b.state.Cursor == nil {
		b.state.Cursor = map[string]int{}
	}
	data, err = os.ReadFile(filepath.Join(b.cfg.StateDir, "offset"))
	if err == nil {
		b.state.Offset, err = strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
		if err != nil {
			return fmt.Errorf("invalid offset: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
func (b *Bot) save() error {
	data, err := json.Marshal(b.state)
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(b.cfg.StateDir, "notifications.json"), data)
}
func (b *Bot) process(u Update) error {
	if u.ID < b.state.Offset {
		return nil
	}
	b.state.Offset = u.ID + 1
	if err := atomicWrite(filepath.Join(b.cfg.StateDir, "offset"), []byte(strconv.FormatInt(b.state.Offset, 10))); err != nil {
		return err
	}
	return b.handle(u)
}
func (b *Bot) api(endpoint string, body any, result any, key string, method string) error {
	var rd io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(data)
	}
	if method == "" {
		if body != nil {
			method = "POST"
		} else {
			method = "GET"
		}
	}
	req, err := http.NewRequest(method, endpoint, rd)
	if err != nil {
		return errors.New("invalid endpoint")
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		req.Header.Set("X-Api-Key", key)
	}
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New("connection failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if result == nil {
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 10<<20)).Decode(result); err != nil {
		return errors.New("invalid response")
	}
	return nil
}
func (b *Bot) telegram(method string, data any) error {
	base := b.cfg.TelegramURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	var res struct {
		OK bool `json:"ok"`
	}
	if err := b.api(strings.TrimRight(base, "/")+"/bot"+b.cfg.Token+"/"+method, data, &res, "", ""); err != nil {
		return err
	}
	if !res.OK {
		return errors.New("Telegram request failed")
	}
	return nil
}
func (b *Bot) send(chat int64, text string, buttons any) error {
	data := map[string]any{"chat_id": chat, "text": text}
	if buttons != nil {
		data["reply_markup"] = map[string]any{"inline_keyboard": buttons}
	}
	return b.telegram("sendMessage", data)
}
func (b *Bot) app(kind string) AppConfig {
	if kind == "movie" {
		return b.cfg.Movie
	}
	return b.cfg.Series
}
func (b *Bot) arr(kind, path string, data any, method string, out any) error {
	app := b.app(kind)
	resource := kind
	if path == "/command" || strings.HasPrefix(path, "/episode") || strings.HasPrefix(path, "/history") {
		resource = ""
	}
	endpoint := strings.TrimRight(app.URL, "/") + "/api/v3/" + resource + strings.TrimPrefix(path, "/")
	if resource == kind {
		endpoint = strings.TrimRight(app.URL, "/") + "/api/v3/" + kind + path
	}
	return b.api(endpoint, data, out, app.Key, method)
}
func number(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case json.Number:
		x, _ := n.Int64()
		return int(x)
	}
	return 0
}
func field(m map[string]any, k string) string { return fmt.Sprint(m[k]) }
func maps(v any) []map[string]any {
	out := []map[string]any{}
	a, _ := v.([]any)
	for _, x := range a {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

var seasonPattern = regexp.MustCompile(`(?i)\s+(?:s|season\s+)(\d{1,3})$`)

func (b *Bot) search(chat, user int64, text string) error {
	kind := ""
	if strings.HasPrefix(text, "/movie ") || strings.HasPrefix(text, "/series ") {
		parts := strings.SplitN(text, " ", 2)
		kind = parts[0][1:]
		text = parts[1]
	}
	text = strings.TrimSpace(text)
	season := 1
	if m := seasonPattern.FindStringSubmatchIndex(text); m != nil && kind != "movie" {
		season, _ = strconv.Atoi(text[m[2]:m[3]])
		text = strings.TrimSpace(text[:m[0]])
		kind = "series"
	}
	if text == "" || strings.HasPrefix(text, "/") {
		return b.send(chat, "Send a title, /movie TITLE, or /series TITLE S02. Series default to season 1; only the requested season is monitored. Choose a match, then confirm. Plot descriptions are not supported yet.", nil)
	}
	if len(text) > 200 {
		return b.send(chat, "Please use a title of at most 200 characters.", nil)
	}
	for key, v := range b.pending {
		if v.User == user || !v.Expires.After(time.Now()) {
			delete(b.pending, key)
		}
	}
	kinds := []string{kind}
	if kind == "" {
		kinds = []string{"movie", "series"}
	}
	buttons := [][]map[string]string{}
	failed := false
	for _, k := range kinds {
		var matches []map[string]any
		if err := b.arr(k, "/lookup?term="+url.QueryEscape(text), nil, "", &matches); err != nil {
			failed = true
			continue
		}
		for i, item := range matches {
			if i >= 5 {
				break
			}
			buf := make([]byte, 8)
			if _, err := rand.Read(buf); err != nil {
				return err
			}
			key := hex.EncodeToString(buf)
			year := "?"
			if item["year"] != nil {
				year = field(item, "year")
			}
			label := fmt.Sprintf("%s (%s) — %s", field(item, "title"), year, k)
			b.pending[key] = &Selection{Kind: k, Item: item, User: user, Chat: chat, Expires: time.Now().Add(15 * time.Minute), Label: label, Season: season}
			buttonLabel := label
			if len(buttonLabel) > 120 {
				buttonLabel = buttonLabel[:120]
			}
			buttons = append(buttons, []map[string]string{{"text": buttonLabel, "callback_data": "pick:" + key}})
		}
	}
	if len(buttons) > 0 {
		text := "Choose a title:"
		if failed {
			text = "Partial results (one service unavailable):"
		}
		return b.send(chat, text, buttons)
	}
	if failed {
		return b.send(chat, "Search service unavailable; try again later.", nil)
	}
	return b.send(chat, "No matches. Try another title or spelling.", nil)
}
func seasons(item map[string]any) []map[string]any { return maps(item["seasons"]) }
func contains(xs []int, v int) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
func buildPayload(kind string, item map[string]any, app AppConfig, season int) (map[string]any, error) {
	p := map[string]any{"title": item["title"], "qualityProfileId": app.Profile, "rootFolderPath": app.Root, "monitored": true}
	if kind == "movie" {
		p["tmdbId"] = item["tmdbId"]
		p["minimumAvailability"] = "released"
		p["addOptions"] = map[string]any{"searchForMovie": true}
		return p, nil
	}
	found := false
	out := []map[string]any{}
	for _, s := range seasons(item) {
		n := number(s["seasonNumber"])
		if n == season {
			found = true
		}
		out = append(out, map[string]any{"seasonNumber": n, "monitored": n == season})
	}
	if !found {
		return nil, errors.New("season not found")
	}
	genre := "standard"
	for _, g := range maps(item["genres"]) {
		_ = g
	}
	if gs, ok := item["genres"].([]any); ok {
		for _, g := range gs {
			if strings.EqualFold(fmt.Sprint(g), "anime") {
				genre = "anime"
			}
		}
	}
	p["monitorNewItems"] = "none"
	p["tvdbId"] = item["tvdbId"]
	p["titleSlug"] = item["titleSlug"]
	p["seasonFolder"] = true
	p["seriesType"] = genre
	p["seasons"] = out
	p["addOptions"] = map[string]any{"monitor": "skip", "searchForMissingEpisodes": true}
	return p, nil
}
func (b *Bot) callback(q Callback) error {
	parts := strings.SplitN(q.Data, ":", 2)
	action, key := "", ""
	if len(parts) == 2 {
		action, key = parts[0], parts[1]
	}
	v := b.pending[key]
	chat := q.Message.Chat.ID
	if v == nil || v.User != q.From.ID || v.Chat != chat || !v.Expires.After(time.Now()) {
		return b.telegram("answerCallbackQuery", map[string]any{"callback_query_id": q.ID, "text": "Selection expired. Search again."})
	}
	if err := b.telegram("answerCallbackQuery", map[string]any{"callback_query_id": q.ID}); err != nil {
		return err
	}
	switch action {
	case "cancel":
		delete(b.pending, key)
		return b.send(chat, "Cancelled.", nil)
	case "pick":
		if v.Kind == "series" {
			found := false
			for _, s := range seasons(v.Item) {
				if number(s["seasonNumber"]) == v.Season {
					found = true
				}
			}
			if !found {
				return b.send(chat, "That season is not listed. Search again with /series TITLE S02 (or the desired season number).", nil)
			}
		}
		v.Confirmed = true
		scope := "One movie."
		if v.Kind == "series" {
			scope = fmt.Sprintf("Season %d only. Previously requested seasons are left unchanged.", v.Season)
		}
		return b.send(chat, fmt.Sprintf("Request %s?\n%s\nUses the configured quality/language profile.", v.Label, scope), [][]map[string]string{{{"text": "Confirm request", "callback_data": "add:" + key}, {"text": "Cancel", "callback_data": "cancel:" + key}}})
	case "add":
		if v.Confirmed {
			delete(b.pending, key)
			return b.add(v)
		}
	}
	return nil
}
func (b *Bot) add(v *Selection) error {
	// Catch up before the ARR request so prior imports cannot be credited to it.
	if err := b.pollHistory(); err != nil {
		return err
	}
	var library []map[string]any
	if err := b.arr(v.Kind, "", nil, "", &library); err != nil {
		return err
	}
	identity := "tmdbId"
	if v.Kind == "series" {
		identity = "tvdbId"
	}
	var existing map[string]any
	for _, entry := range library {
		if number(entry[identity]) == number(v.Item[identity]) {
			existing = entry
			break
		}
	}
	if existing != nil && v.Kind == "movie" {
		return b.send(v.Chat, "Already in the library manager; monitoring and search settings were left unchanged.", nil)
	}
	id := 0
	episodeIDs := []int{}
	if existing != nil {
		id = number(existing["id"])
		found := false
		for _, s := range seasons(existing) {
			if number(s["seasonNumber"]) == v.Season {
				s["monitored"] = true
				found = true
			}
		}
		if !found {
			return b.send(v.Chat, "Season not present in Sonarr yet. Refresh the series and try again.", nil)
		}
		existing["monitored"] = true
		existing["monitorNewItems"] = "none"
		if err := b.arr("series", fmt.Sprintf("/%d", id), existing, "PUT", nil); err != nil {
			return err
		}
		var episodes []map[string]any
		if err := b.arr("series", fmt.Sprintf("/episode?seriesId=%d", id), nil, "", &episodes); err != nil {
			return err
		}
		for _, e := range episodes {
			if number(e["seasonNumber"]) == v.Season {
				episodeIDs = append(episodeIDs, number(e["id"]))
			}
		}
		if len(episodeIDs) > 0 {
			if err := b.arr("series", "/episode/monitor", map[string]any{"episodeIds": episodeIDs, "monitored": true}, "PUT", nil); err != nil {
				return err
			}
		}
		if err := b.arr("series", "/command", map[string]any{"name": "SeasonSearch", "seriesId": id, "seasonNumber": v.Season}, "", nil); err != nil {
			return err
		}
	} else {
		p, err := buildPayload(v.Kind, v.Item, b.app(v.Kind), v.Season)
		if err != nil {
			return err
		}
		var created map[string]any
		if err := b.arr(v.Kind, "", p, "", &created); err != nil {
			return err
		}
		id = number(created["id"])
		if id <= 0 {
			return errors.New("ARR response missing item ID")
		}
	}
	b.state.Requests = append(b.state.Requests, RequestRecord{Kind: v.Kind, ItemID: id, Season: v.Season, Chat: v.Chat, Label: v.Label, EpisodeIDs: episodeIDs})
	if err := b.save(); err != nil {
		return err
	}
	return b.send(v.Chat, fmt.Sprintf("Requested %s. Search queued; availability depends on your indexers. Jellyfin can see it after download and import.", v.Label), nil)
}
func (b *Bot) handle(u Update) error {
	var from User
	var chat Chat
	if u.Callback != nil {
		from = u.Callback.From
		chat = u.Callback.Message.Chat
	} else if u.Message != nil {
		from = u.Message.From
		chat = u.Message.Chat
	} else {
		return nil
	}
	if from.ID == 0 || chat.Type != "private" || (len(b.cfg.Allowed) > 0 && !b.cfg.Allowed[from.ID]) {
		return nil
	}
	var err error
	if u.Callback != nil {
		err = b.callback(*u.Callback)
	} else if u.Message != nil && u.Message.Text != "" {
		err = b.search(chat.ID, from.ID, u.Message.Text)
	}
	if err != nil {
		_ = b.send(chat.ID, "Request failed. Check Radarr/Sonarr before retrying: a timed-out request may already have been added.", nil)
	}
	return err
}
func (b *Bot) pollUpdates() error {
	base := b.cfg.TelegramURL
	if base == "" {
		base = "https://api.telegram.org"
	}
	var response struct {
		OK     bool     `json:"ok"`
		Result []Update `json:"result"`
	}
	data := map[string]any{"offset": b.state.Offset, "timeout": 25, "allowed_updates": []string{"message", "callback_query"}}
	if err := b.api(strings.TrimRight(base, "/")+"/bot"+b.cfg.Token+"/getUpdates", data, &response, "", ""); err != nil {
		return err
	}
	if !response.OK {
		return errors.New("Telegram request failed")
	}
	for _, u := range response.Result {
		if err := b.process(u); err != nil {
			return err
		}
	}
	return nil
}
func (b *Bot) history(kind string, page int) ([]map[string]any, error) {
	var response struct {
		Records []map[string]any `json:"records"`
	}
	path := fmt.Sprintf("/history?page=%d&pageSize=100&sortKey=id&sortDirection=descending", page)
	err := b.arr(kind, path, nil, "", &response)
	return response.Records, err
}
func (b *Bot) pollHistory() error {
	for _, kind := range []string{"movie", "series"} {
		// First run establishes a high-water mark; previous imports are never attributed to new requests.
		if _, ok := b.state.Cursor[kind]; !ok {
			rows, err := b.history(kind, 1)
			if err != nil {
				return err
			}
			highest := 0
			for _, row := range rows {
				if number(row["id"]) > highest {
					highest = number(row["id"])
				}
			}
			b.state.Cursor[kind] = highest
			if err := b.save(); err != nil {
				return err
			}
			continue
		}
		cursor := b.state.Cursor[kind]
		var fresh []map[string]any
		for page := 1; page <= 100; page++ {
			rows, err := b.history(kind, page)
			if err != nil {
				return err
			}
			more := false
			for _, row := range rows {
				if number(row["id"]) > cursor {
					fresh = append(fresh, row)
					more = true
				}
			}
			if !more || len(rows) < 100 {
				break
			}
			if page == 100 {
				return errors.New("history backlog exceeds page limit")
			}
		}
		sort.Slice(fresh, func(i, j int) bool { return number(fresh[i]["id"]) < number(fresh[j]["id"]) })
		for _, row := range fresh {
			id := number(row["id"])
			if row["eventType"] == "downloadFolderImported" {
				itemField := "movieId"
				if kind == "series" {
					itemField = "seriesId"
				}
				for i := range b.state.Requests {
					req := &b.state.Requests[i]
					if req.Kind != kind || req.ItemID != number(row[itemField]) || contains(req.Notified, id) {
						continue
					}
					if kind == "series" {
						ep := number(row["episodeId"])
						if ep == 0 {
							continue
						}
						if len(req.EpisodeIDs) > 0 {
							if !contains(req.EpisodeIDs, ep) {
								continue
							}
						} else {
							var episode map[string]any
							if err := b.arr("series", fmt.Sprintf("/episode/%d", ep), nil, "", &episode); err != nil {
								return err
							}
							if number(episode["seasonNumber"]) != req.Season {
								continue
							}
						}
					}
					if err := b.send(req.Chat, fmt.Sprintf("%s imported and available in the library.", req.Label), nil); err != nil {
						return err
					}
					req.Notified = append(req.Notified, id)
					if err := b.save(); err != nil {
						return err
					}
				}
			}
			b.state.Cursor[kind] = id
			if err := b.save(); err != nil {
				return err
			}
		}
	}
	return nil
}
