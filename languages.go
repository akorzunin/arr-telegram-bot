package main

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

func languageDefault(language string) string {
	if language == "" {
		return "original"
	}
	return language
}
func validLanguage(language string) bool {
	return language == "original" || language == "russian" || language == "ru-subs"
}
func languageLabel(language string) string {
	switch languageDefault(language) {
	case "russian":
		return "Russian audio + RU subtitles"
	case "ru-subs":
		return "Original audio + RU subtitles"
	default:
		return "Original audio + EN subtitles"
	}
}
func (b *Bot) languagesEnabled() bool { return b.cfg.BazarrURL != "" && b.cfg.BazarrKey != "" }
func (b *Bot) languageProfile(kind, language string) int {
	app := b.app(kind)
	if language == "russian" {
		return app.RussianProfile
	}
	return app.Profile
}
func languageButtons(key, prefix string) [][]map[string]string {
	return [][]map[string]string{
		{{"text": "Original + EN", "callback_data": prefix + "original:" + key}},
		{{"text": "Russian + RU", "callback_data": prefix + "russian:" + key}},
		{{"text": "Original + RU subs", "callback_data": prefix + "ru-subs:" + key}},
	}
}
func (b *Bot) confirmLanguageRequest(key string, v *Selection) error {
	text := fmt.Sprintf("Request %s?\n%s\nOriginal audio is preferred; Russian mode requires a Russian release match. Track metadata can be wrong.", v.Label, languageLabel(v.Language))
	if v.Kind == "series" {
		text += fmt.Sprintf("\nDownload search: season %d. Audio/subtitle profiles apply to the WHOLE series; other seasons may upgrade automatically.", v.Season)
	}
	buttons := languageButtons(key, "lang-")
	buttons = append(buttons, []map[string]string{{"text": "Confirm request", "callback_data": "add:" + key}, {"text": "Cancel", "callback_data": "cancel:" + key}})
	return b.send(v.Chat, text, buttons)
}
func (b *Bot) bazarr(path string, data any, method string, out any) error {
	return b.api(b.cfg.BazarrURL+"/api/"+path, data, out, b.cfg.BazarrKey, method)
}
func bazarrResource(kind string) (string, string) {
	if kind == "movie" {
		return "movies", "radarrid"
	}
	return "series", "seriesid"
}

func (b *Bot) bazarrRows(resource, idKey string, id int) ([]map[string]any, error) {
	var result struct {
		Data []map[string]any `json:"data"`
	}
	err := b.bazarr(resource+"?"+url.QueryEscape(idKey+"[]")+"="+fmt.Sprint(id), nil, "", &result)
	return result.Data, err
}
func externalSubtitlePresent(row map[string]any, language string) bool {
	for _, sub := range maps(row["subtitles"]) {
		path, _ := sub["path"].(string)
		if sub["code2"] == language && sub["forced"] != true && path != "" {
			return true
		}
	}
	return false
}

// Bazarr sync is asynchronous. Check existence before POST: its update API can
// otherwise return success without changing a row. Persist retries across restarts.
func (b *Bot) setSubtitleProfile(req RequestRecord) (bool, error) {
	resource, idKey := bazarrResource(req.Kind)
	rows, err := b.bazarrRows(resource, idKey, req.ItemID)
	if err != nil {
		return false, err
	}
	if len(rows) == 0 {
		return false, nil
	}
	profile := b.cfg.EnglishSubs
	if req.Language == "russian" || req.Language == "ru-subs" {
		profile = b.cfg.RussianSubs
	}
	if err := b.bazarr(resource, map[string]any{idKey: []int{req.ItemID}, "profileid": []string{fmt.Sprint(profile)}}, "POST", nil); err != nil {
		return false, err
	}
	if err := b.bazarr(resource, map[string]any{idKey: req.ItemID, "action": "search-missing"}, "PATCH", nil); err != nil {
		return false, err
	}
	return true, nil
}
func (b *Bot) pollLanguages() error {
	if !b.languagesEnabled() {
		return nil
	}
	// Last active request wins because ARR/Bazarr profiles are title-wide, not chat/season-wide.
	latest := map[string]int{}
	for i, r := range b.state.Requests {
		if !r.Cancelled && r.Language != "" {
			key := fmt.Sprintf("%s:%d", r.Kind, r.ItemID)
			previous, found := latest[key]
			if !found || r.LanguageChanged >= b.state.Requests[previous].LanguageChanged {
				latest[key] = i
			}
		}
	}
	for _, i := range latest {
		r := &b.state.Requests[i]
		if r.SubsApplied {
			continue
		}
		applied, err := b.setSubtitleProfile(*r)
		if err != nil {
			return err
		}
		if applied {
			r.SubsApplied = true
			if err := b.save(); err != nil {
				return err
			}
		}
	}
	return nil
}

func (b *Bot) browseLanguages(chat, user int64, query string) error {
	if !b.languagesEnabled() {
		return b.send(chat, "Language profiles are not configured. Configure ARR Russian profiles and Bazarr first.", nil)
	}
	if len([]rune(query)) > 200 {
		return b.send(chat, "Please use a title of at most 200 characters.", nil)
	}
	for key, v := range b.pending {
		if v.User == user {
			delete(b.pending, key)
		}
	}
	season := -1
	if m := seasonPattern.FindStringSubmatchIndex(query); m != nil {
		fmt.Sscanf(query[m[2]:m[3]], "%d", &season)
		query = strings.TrimSpace(query[:m[0]])
	}
	buttons := [][]map[string]string{}
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
			scopes := []int{0}
			if kind == "movie" {
				if season >= 0 || item["hasFile"] != true {
					continue
				}
			} else {
				var files []map[string]any
				if err := b.arr(kind, fmt.Sprintf("/episodefile?seriesId=%d", number(item["id"])), nil, "", &files); err != nil {
					return err
				}
				scopes = nil
				for _, f := range files {
					n := number(f["seasonNumber"])
					if (season < 0 || season == n) && !contains(scopes, n) {
						scopes = append(scopes, n)
					}
				}
			}
			for _, n := range scopes {
				// Bounded list, no pagination: narrow TITLE if a library has over 20 matches.
				if len(buttons) >= 20 {
					continue
				}
				key, v, err := b.cleanupSelection(chat, user)
				if err != nil {
					return err
				}
				v.Library = true
				v.Kind = kind
				v.Item = item
				v.Season = n
				v.Label = title
				if kind == "series" {
					v.Label += fmt.Sprintf(" — S%02d", n)
				}
				label := []rune(v.Label)
				if len(label) > 100 {
					label = label[:100]
				}
				buttons = append(buttons, []map[string]string{{"text": string(label), "callback_data": "pick:" + key}})
			}
		}
	}
	if len(buttons) == 0 {
		return b.send(chat, "No downloaded titles matched. Use /language TITLE or /language TITLE S02.", nil)
	}
	return b.send(chat, "Choose downloaded media (up to 20 matches; narrow the title if needed):", buttons)
}
func (b *Bot) libraryFiles(v *Selection) ([]map[string]any, error) {
	var item map[string]any
	id := number(v.Item["id"])
	if err := b.arr(v.Kind, fmt.Sprintf("/%d", id), nil, "", &item); err != nil {
		return nil, err
	}
	v.Item = item
	if b.cleanupPending(v.Kind, id, v.Season) {
		return nil, fmt.Errorf("cleanup pending")
	}
	if v.Kind == "movie" {
		if item["hasFile"] == true {
			if f, ok := item["movieFile"].(map[string]any); ok {
				return []map[string]any{f}, nil
			}
		}
		return nil, nil
	}
	var all []map[string]any
	if err := b.arr("series", fmt.Sprintf("/episodefile?seriesId=%d", id), nil, "", &all); err != nil {
		return nil, err
	}
	var files []map[string]any
	for _, f := range all {
		if number(f["seasonNumber"]) == v.Season {
			files = append(files, f)
		}
	}
	return files, nil
}
func audioTags(files []map[string]any) string {
	tags := []string{}
	for _, f := range files {
		info, _ := f["mediaInfo"].(map[string]any)
		tag, _ := info["audioLanguages"].(string)
		if tag == "" {
			tag = "unknown"
		}
		if !strings.Contains("|"+strings.Join(tags, "|")+"|", "|"+tag+"|") {
			tags = append(tags, tag)
		}
	}
	if len(tags) == 0 {
		return "unknown"
	}
	text := strings.Join(tags, "; ")
	if len(text) > 500 {
		text = text[:500] + "…"
	}
	return text
}
func hasRussianAudio(files []map[string]any) bool {
	if len(files) == 0 {
		return false
	}
	for _, f := range files {
		info, _ := f["mediaInfo"].(map[string]any)
		langs, _ := info["audioLanguages"].(string)
		found := false
		for _, lang := range strings.FieldsFunc(strings.ToLower(langs), func(r rune) bool { return r == '/' || r == ',' || r == ' ' }) {
			if lang == "rus" || lang == "ru" || lang == "russian" {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	return true
}
func (b *Bot) languagePanel(key string, v *Selection) error {
	files, err := b.libraryFiles(v)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return b.send(v.Chat, "Files are no longer downloaded; search/request the title again.", nil)
	}
	text := v.Label + "\nAudio tags reported by ARR: " + audioTags(files) + ". Unknown/wrong tags cannot verify a language.\nChoose a profile, fetch extra subtitles without changing the profile, or rebuild the streaming copy."
	if v.Kind == "series" {
		text += "\nProfiles apply to the WHOLE series (including automatic upgrades). Extra subtitle searches/rebuilds target this season only."
	}
	buttons := languageButtons(key, "profile-")
	buttons = append(buttons, []map[string]string{{"text": "Fetch EN subtitles", "callback_data": "subs-en:" + key}, {"text": "Fetch RU subtitles", "callback_data": "subs-ru:" + key}})
	if b.cfg.PreparationDir != "" {
		buttons = append(buttons, []map[string]string{{"text": "Rebuild streaming audio", "callback_data": "rebuild:" + key}})
	}
	buttons = append(buttons, []map[string]string{{"text": "Close", "callback_data": "cancel:" + key}})
	return b.send(v.Chat, text, buttons)
}
func (b *Bot) languageRecord(v *Selection) RequestRecord {
	return RequestRecord{Kind: v.Kind, ItemID: number(v.Item["id"]), Season: v.Season, Chat: v.Chat, Label: v.Label, Language: languageDefault(v.Language)}
}
func (b *Bot) applyLibraryLanguage(key string, v *Selection) error {
	files, err := b.libraryFiles(v)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return b.send(v.Chat, "No downloaded files remain.", nil)
	}
	// Do not enable audio upgrades until the replacement confirmation. Changing
	// qualityProfileId now could let RSS upgrade a downloaded title without consent.
	if v.Language == "russian" && !hasRussianAudio(files) {
		v.Replace = true
		return b.send(v.Chat, v.Label+"\nRussian audio is missing or unverified in one or more files. ARR cannot fetch standalone dubbing. Confirm a replacement search? The current file stays until ARR imports a replacement.\nThe audio/subtitle profile change is title-wide; for series, other monitored seasons may also upgrade.", [][]map[string]string{{{"text": "Confirm replacement search", "callback_data": "replace:" + key}, {"text": "Cancel", "callback_data": "cancel:" + key}}})
	}
	v.Item["qualityProfileId"] = b.languageProfile(v.Kind, v.Language)
	// Existing title monitoring is deliberately preserved.
	if err := b.arr(v.Kind, fmt.Sprintf("/%d", number(v.Item["id"])), v.Item, "PUT", nil); err != nil {
		return err
	}
	if err := b.recordLanguageRequest(b.languageRecord(v)); err != nil {
		return err
	}
	v.Replace = true
	return b.send(v.Chat, languageLabel(v.Language)+" profile saved. Bazarr will search missing subtitles after synchronization; availability is not guaranteed. No replacement search was started.\nIf the desired audio is absent, explicitly search a replacement:", [][]map[string]string{{{"text": "Confirm replacement search", "callback_data": "replace:" + key}, {"text": "Back", "callback_data": "pick:" + key}}})
}
func (b *Bot) recordLanguageRequest(record RequestRecord) error {
	record.LanguageChanged = time.Now().UnixNano()
	for i, existing := range b.state.Requests {
		if !existing.Cancelled && existing.Kind == record.Kind && existing.ItemID == record.ItemID && existing.Season == record.Season && existing.Chat == record.Chat {
			r := &b.state.Requests[i]
			r.Language = record.Language
			r.LanguageChanged = record.LanguageChanged
			r.SubsApplied = false
			if len(record.EpisodeIDs) > 0 {
				r.EpisodeIDs = record.EpisodeIDs
			}
			return b.save()
		}
	}
	b.state.Requests = append(b.state.Requests, record)
	return b.save()
}
func (b *Bot) fetchSubtitles(v *Selection, language string) error {
	files, err := b.libraryFiles(v)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return b.send(v.Chat, "No downloaded files remain.", nil)
	}
	id := number(v.Item["id"])
	resource, idKey := bazarrResource(v.Kind)
	if v.Kind == "series" {
		resource = "episodes"
	}
	rows, err := b.bazarrRows(resource, idKey, id)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return b.send(v.Chat, "This title has not synchronized into Bazarr yet. Retry /language TITLE after synchronization.", nil)
	}
	queued, unsynced := 0, 0
	data := map[string]any{"language": language, "forced": "false", "hi": "false"}
	if v.Kind == "movie" {
		if !externalSubtitlePresent(rows[0], language) {
			data["radarrid"] = id
			if err := b.bazarr("movies/subtitles", data, "PATCH", nil); err != nil {
				return err
			}
			queued++
		}
	} else {
		var episodes []map[string]any
		if err := b.arr("series", fmt.Sprintf("/episode?seriesId=%d", id), nil, "", &episodes); err != nil {
			return err
		}
		data["seriesid"] = id
		for _, ep := range episodes {
			if number(ep["seasonNumber"]) != v.Season || ep["hasFile"] != true {
				continue
			}
			var row map[string]any
			for _, candidate := range rows {
				if number(candidate["sonarrEpisodeId"]) == number(ep["id"]) {
					row = candidate
					break
				}
			}
			if row == nil {
				unsynced++
				continue
			}
			if externalSubtitlePresent(row, language) {
				continue
			}
			data["episodeid"] = number(ep["id"])
			if err := b.bazarr("episodes/subtitles", data, "PATCH", nil); err != nil {
				return err
			}
			queued++
		}
	}
	// Track preparation so late Bazarr sidecars are mirrored to the optimized library.
	record := b.languageRecord(v)
	record.Language = ""
	if err := b.recordRequest(record); err != nil {
		return err
	}
	text := fmt.Sprintf("%s subtitle searches queued in Bazarr: %d. Existing external subtitles reported by Bazarr were skipped; audio and profiles were kept. Finding a subtitle is not guaranteed. New SRTs are mirrored to tracked streaming copies.", strings.ToUpper(language), queued)
	if unsynced > 0 {
		text += fmt.Sprintf(" %d downloaded episodes are not synced into Bazarr yet; retry after synchronization.", unsynced)
	}
	return b.send(v.Chat, text, nil)
}
func (b *Bot) languageCallback(action, key string, v *Selection) error {
	switch {
	case action == "cancel":
		delete(b.pending, key)
		return b.send(v.Chat, "Cancelled.", nil)
	case action == "pick":
		v.Confirmed = false
		v.Replace = false
		return b.languagePanel(key, v)
	case strings.HasPrefix(action, "profile-"):
		lang := strings.TrimPrefix(action, "profile-")
		if !validLanguage(lang) {
			return nil
		}
		v.Language = lang
		v.Confirmed = true
		v.Replace = false
		// A separate confirmation discloses the title-wide impact before any changes.
		text := v.Label + "\nApply " + languageLabel(lang) + "? This changes the title's audio/subtitle profiles and can enable automatic audio upgrades."
		if v.Kind == "series" {
			text += " Applies to the WHOLE series, not only this season."
		}
		return b.send(v.Chat, text, [][]map[string]string{{{"text": "Apply profile", "callback_data": "apply:" + key}, {"text": "Back", "callback_data": "pick:" + key}}})
	case action == "apply" && v.Confirmed && validLanguage(v.Language):
		v.Confirmed = false
		return b.applyLibraryLanguage(key, v)
	case action == "subs-en" || action == "subs-ru":
		return b.fetchSubtitles(v, strings.TrimPrefix(action, "subs-"))
	case action == "rebuild":
		return b.rebuildLanguage(v)
	case action == "replace" && v.Replace:
		files, err := b.libraryFiles(v)
		if err != nil {
			return err
		}
		if len(files) == 0 {
			return b.send(v.Chat, "Files disappeared; request the title again.", nil)
		}
		rows, err := b.queue(v.Kind)
		if err != nil {
			return err
		}
		id := number(v.Item["id"])
		idKey := "movieId"
		if v.Kind == "series" {
			idKey = "seriesId"
		}
		for _, row := range rows {
			if number(row[idKey]) == id {
				return b.send(v.Chat, "Already has an active/pending download. Check /active before requesting a replacement.", nil)
			}
		}
		v.Item["qualityProfileId"] = b.languageProfile(v.Kind, v.Language)
		v.Item["monitored"] = true
		if v.Kind == "series" {
			for _, s := range seasons(v.Item) {
				if number(s["seasonNumber"]) == v.Season {
					s["monitored"] = true
				}
			}
		}
		if err := b.arr(v.Kind, fmt.Sprintf("/%d", id), v.Item, "PUT", nil); err != nil {
			return err
		}
		if err := b.recordLanguageRequest(b.languageRecord(v)); err != nil {
			return err
		}
		command := map[string]any{"name": "MoviesSearch", "movieIds": []int{id}}
		if v.Kind == "series" {
			var episodes []map[string]any
			if err := b.arr("series", fmt.Sprintf("/episode?seriesId=%d", id), nil, "", &episodes); err != nil {
				return err
			}
			ids := []int{}
			for _, ep := range episodes {
				if number(ep["seasonNumber"]) == v.Season {
					ids = append(ids, number(ep["id"]))
				}
			}
			if len(ids) > 0 {
				if err := b.arr("series", "/episode/monitor", map[string]any{"episodeIds": ids, "monitored": true}, "PUT", nil); err != nil {
					return err
				}
			}
			command = map[string]any{"name": "SeasonSearch", "seriesId": id, "seasonNumber": v.Season}
		}
		// Consume confirmation before an uncertain command response; never automatically retry a search.
		delete(b.pending, key)
		if err := b.arr(v.Kind, "/command", command, "", nil); err != nil {
			return err
		}
		return b.send(v.Chat, "Replacement search queued. ARR only grabs a release it accepts as an upgrade; availability and audio tags are not guaranteed. Current files stay until import.", nil)
	}
	return nil
}
func (b *Bot) rebuildLanguage(v *Selection) error {
	if b.cfg.PreparationDir == "" {
		return b.send(v.Chat, "Streaming preparation is not enabled.", nil)
	}
	files, err := b.libraryFiles(v)
	if err != nil {
		return err
	}
	if len(files) == 0 {
		return b.send(v.Chat, "No downloaded files remain.", nil)
	}
	record := b.languageRecord(v)
	record.Language = ""
	if err := b.recordRequest(record); err != nil {
		return err
	}
	index := -1
	for i, r := range b.state.Requests {
		if !r.Cancelled && r.Kind == v.Kind && r.ItemID == record.ItemID && r.Chat == v.Chat && r.Season == v.Season {
			index = i
		}
	}
	if index < 0 {
		return fmt.Errorf("request missing")
	}
	if err := b.discoverPreparation(index); err != nil {
		return err
	}
	if err := b.forcePreparation(index); err != nil {
		return err
	}
	return b.send(v.Chat, "Streaming copy queued for rebuilding with all available audio tracks. Original media is unchanged; missing dubbing cannot be created.", nil)
}
