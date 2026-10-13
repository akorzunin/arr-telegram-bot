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
	cfg := Config{Token: strings.TrimSpace(os.Getenv("TELEGRAM_BOT_TOKEN")), StateDir: "/state", Allowed: map[int64]bool{}, CleanupAllowed: map[int64]bool{}}
	cfg.PreparationDir = strings.TrimSpace(os.Getenv("PREPARATION_DIR"))
	cfg.MediaRoot = "/downloads"
	cfg.TransmissionURL = strings.TrimSpace(os.Getenv("TRANSMISSION_URL"))
	if cfg.TransmissionURL == "" {
		cfg.TransmissionURL = "http://host.docker.internal:9092/transmission/rpc"
	}
	if cfg.Token == "" {
		return cfg, errors.New("missing bot token")
	}
	for name, users := range map[string]map[int64]bool{"TELEGRAM_ALLOWED_USERS": cfg.Allowed, "TELEGRAM_CLEANUP_USERS": cfg.CleanupAllowed} {
		for _, part := range strings.Split(os.Getenv(name), ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			id, err := strconv.ParseInt(part, 10, 64)
			if err != nil || id <= 0 {
				return cfg, errors.New("invalid allowed user ID")
			}
			users[id] = true
		}
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
	if len(os.Args) == 2 && os.Args[1] == "--worker" {
		worker, err := workerFromEnv()
		if err != nil {
			log.Fatal(err)
		}
		lock, err := os.OpenFile(worker.Queue+"/worker.lock", os.O_CREATE|os.O_RDWR, 0600)
		if err != nil {
			log.Fatal("Cannot open worker lock")
		}
		defer lock.Close()
		if err := lockWorker(lock); err != nil {
			log.Fatal("Another preparation worker is running")
		}
		for {
			if err := worker.tick(); err != nil {
				log.Print("Preparation worker operation failed; will retry")
			}
			time.Sleep(30 * time.Second)
		}
	}
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
		} else {
			log.Printf("History polling failed; retrying: %v", err)
		}
		time.Sleep(5 * time.Second)
	}
	if cfg.PreparationDir != "" {
		if err := prepareDirs(cfg.PreparationDir); err != nil {
			log.Fatal("Cannot initialize preparation queue")
		}
		if err := bot.restoreCleanupTasks(); err != nil {
			log.Fatal("Cannot restore optimized cleanup tasks")
		}
		if err := bot.cancelUnusedPreparation(); err != nil {
			log.Fatal("Cannot restore preparation cancellation")
		}
	}
	// A short getUpdates long-poll ensures imports are checked regularly, even when chat is idle.
	for {
		if err := bot.pollUpdates(); err != nil {
			// API errors deliberately omit endpoints/bodies, so this includes no tokens.
			log.Printf("Telegram/update processing failed; retrying: %v", err)
			time.Sleep(5 * time.Second)
		}
		if err := bot.pollHistory(); err != nil {
			log.Printf("History polling failed; retrying: %v", err)
			time.Sleep(5 * time.Second)
		}
		if err := bot.pollPreparation(); err != nil {
			log.Print("Preparation discovery/status failed; will retry")
		}
	}
}
