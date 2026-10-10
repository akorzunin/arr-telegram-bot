package main

import (
	"fmt"
	"strings"
	"testing"
)

func movieRequest(r *rig) {
	r.bot.state.Requests = []RequestRecord{{Kind: "movie", ItemID: 81, Chat: 70, Label: "Film"}}
	r.queueMovie = []map[string]any{{"id": 101, "movieId": 81, "title": "Film.Release", "status": "downloading", "size": 1000, "sizeleft": 250, "downloadId": "ABCD", "downloadClient": "Transmission (ARR)"}}
}
func seriesRequest(r *rig) {
	r.bot.state.Requests = []RequestRecord{{Kind: "series", ItemID: 91, Season: 2, Chat: 70, Label: "Show"}}
	r.queueSeries = []map[string]any{{"id": 101, "seriesId": 91, "episode": map[string]any{"seasonNumber": 2}, "title": "Show.S02", "status": "downloading", "downloadId": "ABCD", "downloadClientType": "Transmission"}}
}

func TestActiveOnlyShowsOwnRequests(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.bot.state.Requests = append(r.bot.state.Requests, RequestRecord{Kind: "movie", ItemID: 99, Chat: 80, Label: "Private title"})
	r.queueMovie = append(r.queueMovie, map[string]any{"id": 102, "movieId": 99, "title": "Private release"})
	if err := r.bot.handle(msg("/active")); err != nil {
		t.Fatal(err)
	}
	text := strings.Join(r.messages, "\n")
	if !strings.Contains(text, "75%") || !strings.Contains(text, "/cancel 1") || strings.Contains(text, "Private") {
		t.Fatal(text)
	}
	if len(r.added) != 0 {
		t.Fatal("listing modified ARR")
	}
}

func TestCancellationRequiresConfirmationAndOwnership(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.bot.state.Requests = append(r.bot.state.Requests, RequestRecord{Kind: "movie", ItemID: 99, Chat: 80})
	for _, text := range []string{"/cancel", "/cancel 0", "/cancel 2", "/cancel abc"} {
		if err := r.bot.handle(msg(text)); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.bot.pending) != 0 || len(r.added) != 0 {
		t.Fatal("unauthorized selection")
	}
	if err := r.bot.handle(msg("/cancel 1")); err != nil {
		t.Fatal(err)
	}
	if len(r.added) != 0 || len(r.transmission) != 0 {
		t.Fatal("changed before confirmation")
	}
	// Even an allowlisted user cannot confirm another user's private-chat selection.
	var key string
	for k := range r.bot.pending {
		key = k
	}
	r.bot.cfg.Allowed[8] = true
	err := r.bot.handle(Update{Callback: &Callback{ID: "forged", From: User{ID: 8}, Message: Message{Chat: Chat{ID: 70, Type: "private"}}, Data: "stop:" + key}})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.added) != 0 {
		t.Fatal("forged cancellation accepted")
	}
	r.action(t, "cancel")
	if r.bot.state.Requests[0].Cancelled || len(r.transmission) != 0 {
		t.Fatal("keep request changed state")
	}
}

func TestMovieCancellationKeepsDataAndPersists(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.failQueueDelete = true // Regression: Radarr's redundant IgnoreDownload call crashed.
	if err := r.bot.handle(msg("/cancel 1")); err != nil {
		t.Fatal(err)
	}
	r.action(t, "stop")
	if len(r.added) != 1 || r.added[0]["monitored"] != false {
		t.Fatalf("monitoring: %v", r.added)
	}
	if len(r.transmission) != 1 {
		t.Fatal(r.transmission)
	}
	args := r.transmission[0]["arguments"].(map[string]any)
	if args["delete-local-data"] != false || fmt.Sprint(args["ids"]) != "[abcd]" {
		t.Fatal(args)
	}
	if hasCall(r.calls, "DELETE /api/v3/queue/") || !r.bot.state.Requests[0].Cancelled {
		t.Fatal(r.calls)
	}
	clone := newBot(r.bot.cfg, r.server.Client())
	if err := clone.load(); err != nil {
		t.Fatal(err)
	}
	if !clone.state.Requests[0].Cancelled {
		t.Fatal("cancellation lost on restart")
	}
	before := len(r.calls)
	if err := clone.cancelRequest(70, 1); err != nil {
		t.Fatal(err)
	}
	if len(r.calls) != before+1 {
		t.Fatal("repeated cancellation modified ARR")
	}
}

func TestSeriesCancellationPreservesOtherSeasonAndDeduplicatesTorrent(t *testing.T) {
	r := setup(t)
	seriesRequest(r)
	r.queueSeries = append(r.queueSeries, map[string]any{"id": 102, "seriesId": 91, "seasonNumber": 2, "downloadId": "abcd", "downloadClientType": "Transmission"}, map[string]any{"id": 103, "seriesId": 91, "seasonNumber": 1, "downloadId": "OTHER", "downloadClientType": "Transmission"})
	if err := r.bot.cancelRequest(70, 1); err != nil {
		t.Fatal(err)
	}
	if r.added[0]["monitored"] != true || r.added[0]["monitorNewItems"] != "none" {
		t.Fatal(r.added[0])
	}
	ss := seasons(r.added[0])
	if ss[0]["monitored"] != true || ss[1]["monitored"] != false {
		t.Fatal(ss)
	}
	if fmt.Sprint(r.added[1]["episodeIds"]) != "[41]" || r.added[1]["monitored"] != false {
		t.Fatal(r.added[1])
	}
	if len(r.transmission) != 1 || fmt.Sprint(r.transmission[0]["arguments"].(map[string]any)["ids"]) != "[abcd]" {
		t.Fatal(r.transmission)
	}
	if hasCall(r.calls, "DELETE /api/v3/queue/") {
		t.Fatal("torrent was unnecessarily ignored in ARR")
	}
}

func TestCancellationRefusesSharedPackAndUnknownSeason(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		t.Run(fmt.Sprint(unknown), func(t *testing.T) {
			r := setup(t)
			seriesRequest(r)
			row := map[string]any{"id": 102, "seriesId": 91, "downloadId": "abcd", "seasonNumber": 1}
			if unknown {
				delete(row, "seasonNumber")
			}
			r.queueSeries = append(r.queueSeries, row)
			err := r.bot.cancelRequest(70, 1)
			if unknown && err == nil {
				t.Fatal("unknown season accepted")
			}
			if len(r.added) != 0 || len(r.transmission) != 0 || r.bot.state.Requests[0].Cancelled {
				t.Fatal("unsafe pack modified")
			}
		})
	}
}

func TestCancellationFailureRemainsRetryable(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.failRemove = true
	if err := r.bot.cancelRequest(70, 1); err == nil {
		t.Fatal("failure swallowed")
	}
	if r.bot.state.Requests[0].Cancelled || hasCall(r.calls, "DELETE /api/v3/queue/") {
		t.Fatal("request lost before removal")
	}
	r.failRemove = false
	if err := r.bot.cancelRequest(70, 1); err != nil {
		t.Fatal(err)
	}
	if !r.bot.state.Requests[0].Cancelled {
		t.Fatal("retry failed")
	}
}

func TestQueuePaginationAndWaitingRequest(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	for i := 0; i < 100; i++ {
		r.queueMovie = append(r.queueMovie, map[string]any{"id": 200 + i, "movieId": 99})
	}
	rows, err := r.bot.queue("movie")
	if err != nil || len(rows) != 101 || !hasCall(r.calls, "page=2") {
		t.Fatalf("%d %v %v", len(rows), err, r.calls)
	}
	r.queueMovie = nil
	if err := r.bot.active(70); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(strings.Join(r.messages, "\n"), "No active torrent") {
		t.Fatal(r.messages)
	}
	if err := r.bot.cancelRequest(70, 1); err != nil {
		t.Fatal(err)
	}
	if len(r.transmission) != 0 || !r.bot.state.Requests[0].Cancelled {
		t.Fatal("waiting request cancellation")
	}
}

func TestPendingReleaseCancellation(t *testing.T) {
	for _, gone := range []bool{false, true} {
		t.Run(fmt.Sprint(gone), func(t *testing.T) {
			r := setup(t)
			movieRequest(r)
			r.queueGone = gone
			r.queueMovie = []map[string]any{{"id": 101, "movieId": 81, "status": "delay"}}
			if err := r.bot.cancelRequest(70, 1); err != nil {
				t.Fatal(err)
			}
			if !hasCall(r.calls, "DELETE /api/v3/queue/101?") || len(r.transmission) != 0 || !r.bot.state.Requests[0].Cancelled {
				t.Fatal("pending release was not cleared safely")
			}
		})
	}
}

func TestPendingReleaseFailureRemainsRetryable(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.queueMovie = []map[string]any{{"id": 101, "movieId": 81, "status": "delay"}}
	r.failQueueDelete = true
	if err := r.bot.cancelRequest(70, 1); err == nil || !strings.Contains(err.Error(), "remove pending ARR release") {
		t.Fatal("missing pending-release failure context")
	}
	if r.bot.state.Requests[0].Cancelled || len(r.transmission) != 0 {
		t.Fatal("failed pending release lost")
	}
}

func TestCancelledRequestDoesNotNotify(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.bot.state.Requests[0].Cancelled = true
	r.bot.state.Cursor = map[string]int{"movie": 0, "series": 0}
	r.historyMovie = []map[string]any{{"id": 1, "movieId": 81, "eventType": "downloadFolderImported"}}
	if err := r.bot.pollHistory(); err != nil {
		t.Fatal(err)
	}
	if len(r.messages) != 0 {
		t.Fatal("cancelled request notified")
	}
}

func TestCancellationRefusesOtherDownloadClient(t *testing.T) {
	r := setup(t)
	movieRequest(r)
	r.queueMovie[0]["downloadClient"] = "qBittorrent"
	if err := r.bot.cancelRequest(70, 1); err != nil {
		t.Fatal(err)
	}
	if len(r.added) != 0 || len(r.transmission) != 0 {
		t.Fatal("wrong client modified")
	}
}
