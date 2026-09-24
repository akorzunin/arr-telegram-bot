# ARR Telegram bot (standalone Go)

Private-chat long-polling bot for Radarr and Sonarr; no third-party Go modules or public webhook. This repository is independent of the Ansible repository. **Do not run alongside the Python bot with the same Telegram token** (competing getUpdates consumers). No deployment is performed by this repo.

## Contract

Requires these variables (normally `/srv/deploy/arr/bot.env`):

| Variable | Purpose |
| --- | --- |
| `TELEGRAM_BOT_TOKEN` | Telegram bot token |
| `TELEGRAM_ALLOWED_USERS` | Comma-separated Telegram numeric user IDs; empty preserves Python behavior and allows any private chat |
| `RADARR_API_KEY`, `SONARR_API_KEY` | ARR API credentials |
| `RADARR_QUALITY_PROFILE_ID`, `SONARR_QUALITY_PROFILE_ID` | Positive numeric profile IDs |
| `RADARR_URL`, `SONARR_URL` | Internal ARR URLs (compose overrides to `http://radarr:7878`, `http://sonarr:8989`) |
| `RADARR_ROOT`, `SONARR_ROOT` | Root folder paths inside ARR |

Compose uses external network `arr_shared`, which must already exist and include Radarr and Sonarr with DNS names `radarr` and `sonarr`. `ARR_BOT_ENV_FILE` overrides the default env file path `/srv/deploy/arr/bot.env`. Host proxies are bypassed for the two ARR names. The process runs as `${BOT_UID:-1000}:${BOT_GID:-1000}`; set these in the standalone Compose `.env` to the owner of `/srv/deploy/arr/config/bot`. The Ansible deployment sets them from the target account. Mount `/srv/deploy/arr/config/bot:/state` writable; the existing `/state/offset` is retained. `/state/notifications.json` holds imported-event checkpoints, request/chat mapping and delivery dedupe. Mount it persistently and back it up; it contains chat IDs and titles, not credentials. The process logs generic errors only, never URLs or response bodies containing secrets.

Build/test: `go test ./... && go vet ./...`; `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o arr-telegram-bot .`. Compose build: `docker compose build`. There are no ports to expose.

## Behavior

Send a plain title to search both apps, `/movie TITLE`, or `/series TITLE S02` (also `season 2`). At most five results per app; select, then confirm within 15 minutes. A series defaults to season 1. A new movie searches immediately; an existing movie is left unchanged. A new series monitors only the requested season; an existing series monitors the requested season and its episodes, and queues `SeasonSearch`. The bot checks history `downloadFolderImported` events; it sends a chat notification only for tracked request IDs and requested episode selections. For a new series, the episode's season is checked via Sonarr before notifying. A startup history high-water mark prevents old imports from being attributed to later requests. An unsuccessful Telegram send leaves the checkpoint behind for a later retry (successful sends can still duplicate if the process crashes between Telegram acceptance and durable checkpoint). An update offset is written before handling each update, as in the Python bot: crashes can drop an unhandled request but cannot replay an add.

Notifications are polled after each Telegram long poll (normally 25 seconds). History reads page through up to 100 pages of 100 records; an excessive backlog results in a retry/error, not silent skipping. On first startup, ARR history must be reachable before accepting requests. The service must have permission to create files in `/state`.
