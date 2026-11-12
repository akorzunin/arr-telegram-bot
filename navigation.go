package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Bare numbers are persistent bot request IDs, never ambiguous ARR IDs.
func (b *Bot) request(chat int64, text string) (int, *RequestRecord) {
	n, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(text), "#"))
	if err != nil || n < 1 || n > len(b.state.Requests) {
		return 0, nil
	}
	r := &b.state.Requests[n-1]
	if r.Chat != chat || r.Cancelled {
		return 0, nil
	}
	return n, r
}

func (b *Bot) menu(chat int64) error {
	return b.telegram("sendMessage", map[string]any{
		"chat_id": chat, "text": "Send a movie or series title to search, or choose an action below. Open My requests to manage an item without typing commands.",
		"reply_markup": map[string]any{"keyboard": [][]string{{"My requests", "Downloads"}, {"Languages", "Library"}, {"Search", "Help"}}, "resize_keyboard": true, "is_persistent": true},
	})
}

func (b *Bot) requestsPage(chat int64, page int) error {
	ids := []int{}
	for i := len(b.state.Requests) - 1; i >= 0; i-- {
		r := b.state.Requests[i]
		if r.Chat == chat && !r.Cancelled {
			ids = append(ids, i+1)
		}
	}
	if len(ids) == 0 {
		return b.send(chat, "No requests yet. Send a title to search.", nil)
	}
	if page < 0 || page*8 >= len(ids) {
		page = 0
	}
	buttons := [][]map[string]string{}
	for _, n := range ids[page*8 : min((page+1)*8, len(ids))] {
		r := b.state.Requests[n-1]
		label := fmt.Sprintf("#%d %s", n, r.Label)
		if r.Kind == "series" {
			label += fmt.Sprintf(" S%02d", r.Season)
		}
		buttons = append(buttons, []map[string]string{{"text": string([]rune(label)[:min(100, len([]rune(label)))]), "callback_data": fmt.Sprintf("item:%d", n)}})
	}
	nav := []map[string]string{}
	if page > 0 {
		nav = append(nav, map[string]string{"text": "Previous", "callback_data": fmt.Sprintf("requests:%d", page-1)})
	}
	if (page+1)*8 < len(ids) {
		nav = append(nav, map[string]string{"text": "Next", "callback_data": fmt.Sprintf("requests:%d", page+1)})
	}
	if len(nav) > 0 {
		buttons = append(buttons, nav)
	}
	return b.send(chat, "My requests — choose an item:", buttons)
}

func (b *Bot) itemPanel(chat int64, text string) error {
	n, r := b.request(chat, text)
	if r == nil {
		return b.send(chat, "Request not found in this chat. Open My requests for valid IDs.", nil)
	}
	buttons := [][]map[string]string{
		{{"text": "Audio / subtitles", "callback_data": fmt.Sprintf("language:%d", n)}},
		{{"text": "Provide magnet link", "callback_data": fmt.Sprintf("magnet:%d", n)}},
	}
	if b.cfg.PreparationDir != "" {
		buttons = append(buttons, []map[string]string{{"text": "Retry preparation", "callback_data": fmt.Sprintf("retry:%d", n)}})
	}
	buttons = append(buttons, []map[string]string{{"text": "Cancel request", "callback_data": fmt.Sprintf("cancelrequest:%d", n)}, {"text": "All requests", "callback_data": "requests:0"}})
	return b.send(chat, fmt.Sprintf("#%d %s\n%s", n, r.Label, languageLabel(r.Language)), buttons)
}

func (b *Bot) navigation(q Callback) (bool, error) {
	parts := strings.SplitN(q.Data, ":", 2)
	if len(parts) != 2 {
		return false, nil
	}
	switch parts[0] {
	case "requests", "item", "language", "magnet", "cancelrequest":
	default:
		return false, nil
	}
	if err := b.telegram("answerCallbackQuery", map[string]any{"callback_query_id": q.ID}); err != nil {
		return true, err
	}
	chat := q.Message.Chat.ID
	if parts[0] == "requests" {
		page, _ := strconv.Atoi(parts[1])
		return true, b.requestsPage(chat, page)
	}
	n, r := b.request(chat, parts[1])
	if r == nil {
		return true, b.send(chat, "Request not found in this chat.", nil)
	}
	switch parts[0] {
	case "item":
		return true, b.itemPanel(chat, parts[1])
	case "language":
		return true, b.browseLanguages(chat, q.From.ID, parts[1])
	case "cancelrequest":
		return true, b.confirmCancellation(chat, q.From.ID, fmt.Sprintf("/cancel %d", n))
	case "magnet":
		for key, v := range b.pending {
			if v.AwaitMagnet && v.User == q.From.ID && v.Chat == chat {
				delete(b.pending, key)
			}
		}
		key, v, err := b.cleanupSelection(chat, q.From.ID)
		if err != nil {
			return true, err
		}
		v.RequestNumber = n
		v.Expires = time.Now().Add(15 * time.Minute)
		v.AwaitMagnet = true
		return true, b.send(chat, fmt.Sprintf("Send the magnet link for #%d %s (within 15 minutes). It will be shown for confirmation before downloading.", n, r.Label), [][]map[string]string{{{"text": "Cancel", "callback_data": "cancel:" + key}}})
	}
	return true, nil
}
