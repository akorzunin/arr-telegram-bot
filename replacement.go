package main

import (
	"fmt"
	"regexp"
)

// RuTracker's explicit internal-audio list, not Cyrillic titles, RUS(ext), or subtitle tags.
var russianInternalAudio = regexp.MustCompile(`(?i)\[(?:(?:ENG|JAP|JPN|UKR)(?:\([^)]*\))?[, /]+)*RUS\(int\)(?:[, /]+(?:ENG|JAP|JPN|UKR)(?:\([^)]*\))?)*\]`)
var missingRussianScore = regexp.MustCompile(`^Custom Formats (?:ARR Original Audio)? have score (?:0|100) below (?:Movie|Series)'s profile minimum 10000$`)
var equalFormatScore = regexp.MustCompile(`^Existing file on disk has a equal or higher Custom Format score: -?\d+$`)

func russianParserFallback(release map[string]any) bool {
	title, _ := release["title"].(string)
	indexer, _ := release["indexer"].(string)
	if indexer != "RuTracker.org (Prowlarr)" || !russianInternalAudio.MatchString(title) || release["downloadAllowed"] != true || release["rejected"] != true {
		return false
	}
	score := number(release["customFormatScore"])
	guid, _ := release["guid"].(string)
	if (score != 0 && score != 100) || release["customFormatScore"] == nil || number(release["indexerId"]) <= 0 || guid == "" {
		return false
	}
	reasons, ok := release["rejections"].([]any)
	if !ok || len(reasons) == 0 {
		return false
	}
	missing := false
	// Rejection wording is an ARR-version ceiling: unknown/new reasons fail closed;
	// update this allowlist against real API responses when ARR changes its messages.
	for _, value := range reasons {
		reason, ok := value.(string)
		if !ok {
			return false
		}
		if missingRussianScore.MatchString(reason) {
			missing = true
		} else if !equalFormatScore.MatchString(reason) {
			return false
		}
	}
	return missing
}

// Empty status means ARR found an accepted release: use its normal automatic search.
func (b *Bot) russianReplacement(v *Selection) (string, error) {
	id := number(v.Item["id"])
	path := fmt.Sprintf("/release?movieId=%d", id)
	if v.Kind == "series" {
		path = fmt.Sprintf("/release?seriesId=%d&seasonNumber=%d", id, v.Season)
	}
	var releases []map[string]any
	if err := b.arr(v.Kind, path, nil, "", &releases); err != nil {
		return "", err
	}
	for _, release := range releases {
		if release["rejected"] == false && release["downloadAllowed"] == true {
			return "", nil
		}
	}
	// ARR returns prioritized releases. Grab only one, retaining all quality,
	// seeder, title-match, blocklist and other checks; never lower the shared profile.
	for _, release := range releases {
		if !russianParserFallback(release) || (v.Kind == "series" && number(release["seasonNumber"]) != v.Season) {
			continue
		}
		rows, err := b.queue(v.Kind)
		if err != nil {
			return "", err
		}
		idKey := "movieId"
		if v.Kind == "series" {
			idKey = "seriesId"
		}
		for _, row := range rows {
			if number(row[idKey]) == id {
				return "Already has an active/pending download. Check /active.", nil
			}
		}
		// Use ARR's cached release, without overriding title/quality/episode mapping.
		payload := map[string]any{"guid": release["guid"], "indexerId": release["indexerId"], idKey: id}
		if err := b.arr(v.Kind, "/release", payload, "POST", nil); err != nil {
			return "", err
		}
		return "Russian replacement grabbed using its explicit RUS(int) audio tag; ARR's language parser missed it. Other rejection checks were kept. Current files stay until import. Audio and RU subtitles are verified only after import; Bazarr may find no subtitles.", nil
	}
	return "No eligible Russian replacement found. No download was started; current files are unchanged. Only explicit internal Russian audio tags with language-score-only rejections qualify for the parser fallback. The Russian profile is saved and Bazarr will still search RU subtitles; availability is not guaranteed.", nil
}
