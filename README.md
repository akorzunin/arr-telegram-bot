# ARR Telegram bot (standalone Go)

Private-chat long-polling bot for Radarr and Sonarr; no third-party Go modules or public webhook. This repository is independent of the Ansible repository. CI publishes `ghcr.io/akorzunin/arr-telegram-bot:latest`; the ARR Ansible repository runs that image in its existing Compose stack. **Do not run alongside the Python bot with the same Telegram token** (competing getUpdates consumers). No deployment is performed by this repo.

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
| `TRANSMISSION_URL` | RPC endpoint for file-preserving cancellation; defaults to `http://host.docker.internal:9092/transmission/rpc` |

The ARR Compose stack sets internal Radarr/Sonarr URLs, media roots, and the workstation proxy at `host.docker.internal:20171` for Telegram; it bypasses the proxy for the two ARR names. The container runs as the ARR deployment user and mounts `/srv/deploy/arr/config/bot:/state` writable. The existing `/state/offset` is retained. `/state/notifications.json` holds imported-event checkpoints, request/chat mapping and delivery dedupe. Mount it persistently and back it up; it contains chat IDs and titles, not credentials. The process logs generic errors only, never URLs or response bodies containing secrets.

Build/test: `go test ./... && go vet ./...`; `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o arr-telegram-bot .`. Publishing from `master` is handled by `.github/workflows/image.yml`. There are no ports to expose. A private GHCR package may require `docker login ghcr.io` with read:packages on the target host.

## Behavior

Send a plain title to search both apps, `/movie TITLE`, or `/series TITLE S02` (also `season 2`). At most five results per app; select, then confirm within 15 minutes. A series defaults to season 1. A new movie searches immediately; an existing movie is left unchanged. A new series monitors only the requested season; an existing series monitors the requested season and its episodes, and queues `SeasonSearch`. The bot checks history `downloadFolderImported` events; it sends a chat notification only for tracked request IDs and requested episode selections. For a new series, the episode's season is checked via Sonarr before notifying. A startup history high-water mark prevents old imports from being attributed to later requests. An unsuccessful Telegram send leaves the checkpoint behind for a later retry (successful sends can still duplicate if the process crashes between Telegram acceptance and durable checkpoint). An update offset is written before handling each update, as in the Python bot: crashes can drop an unhandled request but cannot replay an add.

### Download controls

- `/active` (alias `/downloads`) lists this private chat's saved bot requests with
  ARR queue status/progress, or a waiting/no-eligible-release message when files
  are missing but no torrent is active. Completed requests without a queue entry
  and cancelled requests are hidden. Downloads added outside the bot are not shown.
- `/cancel NUMBER` uses the stable request number from `/active` and requires a
  user/chat-bound confirmation within 15 minutes. It disables monitoring for the
  movie or selected season and its episodes, leaving other seasons unchanged.
  It removes the torrents through Transmission RPC with `delete-local-data=false`;
  ARR clears these entries on its next download-client poll. Explicitly ignoring
  an already-removed torrent can crash Radarr's `IgnoreDownload` handler, so no
  redundant queue deletion is sent. Pending releases without torrents are removed
  from ARR's delay queue. No releases are blocklisted or searched for replacements.
  ARR's own `removeFromClient=true` would delete files and is deliberately not used.
  Torrent removal negotiates Transmission's 409 session-ID challenge. The current
  stack has RPC authentication disabled; keep that endpoint private.
- Cancellation affects the shared ARR item, including other chats requesting that
  same movie/season. Packs shared across seasons **as reported by ARR's queue**
  and rows with unknown season metadata are refused before changing monitoring.
  Contents absent from ARR's queue are not inspected; manage mixed/untracked
  season packs manually. Non-Transmission downloads are
  refused. Failures can leave monitoring disabled or some torrents removed; inspect
  ARR/Transmission and retry the request (it remains tracked until completion).
- Cancelled scopes no longer receive import notifications; cancellation survives
  restarts. Previously imported files and partial torrent data remain on disk.
  Already-running explicit ARR searches can still finish after cancellation; inspect
  ARR's queue and manage any later grabs there. To restart a cancelled movie, use
  Radarr monitoring/search; the bot leaves existing movies unchanged. A new series
  request can re-enable the selected season.

Checks use mock ARR, Telegram and Transmission servers; no real downloads or live
service integration is exercised by the unit tests. Publish the updated bot image
before pulling/recreating the bot in the Ansible stack; this checkout alone does
not change a running container.

Notifications are polled after each Telegram long poll (normally 25 seconds). History reads page through up to 100 pages of 100 records; an excessive backlog results in a retry/error, not silent skipping. On first startup, ARR history must be reachable before accepting requests. The service must have permission to create files in `/state`.
