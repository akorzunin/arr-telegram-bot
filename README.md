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

Build/test: `go test ./... && go vet ./...`; optional real-GPU check: `ARR_TEST_NVENC=1 go test -run TestWorkerRealFFmpeg -v ./...`. FFmpeg integration tests skip when the binaries are unavailable. CI also publishes `:worker` and `:worker-SHA` from `Dockerfile.worker`. The bot remains a small scratch image; the worker image includes Ubuntu FFmpeg and uses the host NVIDIA driver through NVIDIA Container Toolkit.

Build: `CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o arr-telegram-bot .`. Publishing from `master` is handled by `.github/workflows/image.yml`. There are no ports to expose. A private GHCR package may require `docker login ghcr.io` with read:packages on the target host.

## Behavior

Send a plain title to search both apps, `/movie TITLE`, or `/series TITLE S02` (also `season 2`). At most five results per app; select, then confirm within 15 minutes. A series defaults to season 1. A new movie searches immediately. A confirmed re-request of an existing movie with no file and no active/pending ARR queue entry re-enables monitoring and runs `MoviesSearch`, preserving its profile and path. Movies with files or active queue entries are left unchanged. A new series monitors only the requested season; an existing series monitors the requested season and its episodes, and queues `SeasonSearch`. The bot checks history `downloadFolderImported` events; it sends a chat notification only for tracked request IDs and requested episode selections. For a new series, the episode's season is checked via Sonarr before notifying. A startup history high-water mark prevents old imports from being attributed to later requests. An unsuccessful Telegram send leaves the checkpoint behind for a later retry (successful sends can still duplicate if the process crashes between Telegram acceptance and durable checkpoint). An update offset is written before handling each update, as in the Python bot: crashes can drop an unhandled request but cannot replay an add.

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
  ARR's queue and manage any later grabs there. To restart a cancelled, missing
  movie, search/select/confirm it again in the bot (or use Radarr monitoring/search).
  The new request is tracked for import notifications; the cancelled record stays
  cancelled. A new series request can re-enable the selected season.

Checks use mock ARR, Telegram and Transmission servers; no real downloads or live
service integration is exercised by the unit tests. Publish the updated bot image
before pulling/recreating the bot in the Ansible stack; this checkout alone does
not change a running container.

## Automatic streaming preparation (opt-in)

Set bot `PREPARATION_DIR=/preparation` to enable; leaving it empty retains the
original behavior. Mount original media read-only at `/downloads` and share a
persistent `/preparation` directory with the worker, using the same UID/GID.
The worker runs the same binary with `--worker`, needs `JELLYFIN_URL` and an admin
`JELLYFIN_API_KEY`, and writes a separate `/optimized` media mount. It needs no
Telegram or ARR credentials. Run exactly one worker; a filesystem lock enforces
this on a single host. Do not run worker and bot on separate machines against a
network filesystem without revisiting locking/publication guarantees.

- Only tracked, non-cancelled Telegram movie/season requests are inspected. This
  includes previous saved requests when the feature is enabled, existing imported
  files requested again, and subsequent upgrades of those requested titles.
- Existing downloaded movie requests do not search again or change ARR monitoring.
- Inspection chooses **video copy/remux**, **audio-only conversion**, or **NVENC
  video encoding**. Stream-copy does not modify originals; it creates a new MP4.
- Initial target: SDR, H.264 8-bit, up to 1920×1080 without upscaling. Video encoding
  targets 12 Mbps, maxrate 16 Mbps, a 32 Mbit VBV buffer. Skip encoding only when the
  source averages at most 15 Mbps and five-second packet windows stay at most
  18 Mbps (audio included). The final output is checked against the same budget.
  This is a buffered-player heuristic, not a guarantee against subsecond peaks.
- Keep one default audio track (otherwise the first); copy compatible AAC-LC
  mono/stereo or convert to 192 kbps AAC stereo. Alternate/surround audio remains
  available in the original library. Multi-language optimized audio is not yet supported.
- Copy external SRT and extract embedded SubRip/mov_text with language labels.
  Later Bazarr SRT additions/changes are mirrored without re-encoding. Image/styled
  subtitles stay in originals; ready/status messages warn about this. No burn-in,
  OCR, ASS styling conversion, or automatic HDR tone mapping. HDR is **blocked**.
- One job at a time; queue/results survive restart. Interrupted jobs restart rather
  than trying to resume a partially encoded MP4. Sources are fingerprinted using
  path/size/mtime; this assumes immutable ARR imports.
- Staging is outside `films`/`shows`; validated output replaces the final file
  atomically. Full decode and bitrate checks precede publication. Free-space checks
  reserve the output budget plus 1 GiB. An upgrade never deletes the old good video
  before a new one validates. Subtitles are staged too; the sidecar set is published
  file-by-file before index refresh, not atomically with the video. Old-version
  subtitle sidecars are removed on successful replacement. Treat optimized folders
  as worker-owned (manual subtitle edits there can be replaced). Originals/hardlinks/
  seeding files are never written.
- Jobs are deduplicated across chats, and newer versions supersede older pending
  jobs for the same output. `/cancel` retains its existing shared ARR semantics and
  also cancels preparation no active requester still needs; published copies remain.
- `/active` includes queued/checking/preparing/progress/validating/indexing/failure
  states, hiding fully downloaded and ready requests. `/retry NUMBER` or the Retry
  button retries failed/blocked preparation without downloading again. Ownership
  is checked, and a retry applies to the shared physical job.
- Ready notifications require Jellyfin indexing, not just ARR import. Season-pack
  updates are batched per poll; duplicate Telegram delivery remains possible if the
  process crashes after Telegram accepts a send but before its checkpoint is saved.
  If indexing alone failed, retry reuses the already published video.

Create Jellyfin **Streaming Films** and **Streaming Shows** libraries pointing to
`/optimized/films` and `/optimized/shows`, respectively; the paths must match the
worker's output mount. Enable user access and real-time monitoring. The worker
checks the matching library exists before encoding and refreshes/index-verifies
published video by path and indexed file size. It does not automatically create
libraries or alter users.

Back up `notifications.json` and the queue's `requests/`, `results/`, and `cancel/`
together. JSON state includes media paths/titles/chat IDs, not API keys. Queue files
are retained for restart/deduplication; per-tick queue scans become quadratic with
large histories, so introduce an output-key index/retention before thousands of jobs.
The worker polls every 30 seconds when idle; status discovery remains tied to
Telegram polling. Verify Direct Play and real two-client SyncPlay on target clients
before assuming all codecs/subtitles/buffering conditions are covered.

Notifications are polled after each Telegram long poll (normally 25 seconds). History reads page through up to 100 pages of 100 records; an excessive backlog results in a retry/error, not silent skipping. On first startup, ARR history must be reachable before accepting requests. The service must have permission to create files in `/state`.
