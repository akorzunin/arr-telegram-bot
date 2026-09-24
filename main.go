package main

import (
	"errors"
	"log"
	"os"
	"strconv"
	"strings"
	"time"
)

func configFromEnv() (Config, error) {
	cfg := Config{Token: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")), StateDir: "/state", Allowed: map[int64]bool{}}
	if cfg.Token == "" {
		return cfg, errors.New("missing bot token")
	}
	for _, part := range strings.Split(os.Getenv("TELEGRAM_ALLOWED_USERS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		id, err := strconv.ParseInt(part, 10, 64)
		if err != nil || id <= 0 {
			return cfg, errors.New("invalid allowed user ID")
		}
		cfg.Allowed[id] = true
	}
	for _, kind := range []string{"RADARR", "SONARR"} {
		key := strings.TrimSpace(os.Getenv(kind + "_API_KEY"))
		root := strings.TrimSpace(os.Getenv(kind + "_ROOT"))
		addr := strings.TrimSpace(os.Getenv(kind + "_URL"))
		profile, err := strconv.Atoi(os.Getenv(kind + "_QUALITY_PROFILE_ID"))
		if key == "" || root == "" || addr == "" || err != nil || profile <= 0 {
			return cfg, errors.New("invalid ARR configuration")
		}
		app := AppConfig{Key: key, Root: root, URL: strings.TrimRight(addr, "/"), Profile: profile}
		if kind == "RADARR" {
			cfg.Movie = app
		} else {
			cfg.Series = app
		}
	}
	return cfg, nil
}
func main() {
	cfg, err := configFromEnv()
	if err != nil {
		log.Fatal("Invalid bot configuration: check token, allowed user IDs, API keys, profile IDs and state directory")
	}
	bot := newBot(cfg, nil)
	if err := bot.load(); err != nil {
		log.Fatal("Cannot load state")
	}
	// Baseline before accepting new requests, so historical imports are never attributed to them.
	for {
		if err := bot.pollHistory(); err == nil {
			break
		}
		log.Print("History polling failed; retrying")
		time.Sleep(5 * time.Second)
	}
	// A short getUpdates long-poll ensures imports are checked regularly, even when chat is idle.
	for {
		if err := bot.pollUpdates(); err != nil {
			log.Print("Telegram polling failed; retrying")
			time.Sleep(5 * time.Second)
		}
		if err := bot.pollHistory(); err != nil {
			log.Print("History polling failed; retrying")
			time.Sleep(5 * time.Second)
		}
	}
}
