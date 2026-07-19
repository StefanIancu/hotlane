// Package notify delivers events to a user-configured webhook. Detection
// without notification is half a feature: the daemon watches production, so
// it must be able to speak up when nobody is looking at a terminal.
//
// The payload carries both "text" (Slack incoming webhooks) and "content"
// (Discord) alongside the structured fields, so the same URL works
// out of the box with either - or with anything that accepts JSON POSTs.
package notify

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"time"
)

// Event names.
const (
	EventDriftDetected    = "drift_detected"
	EventDriftHealed      = "drift_healed"
	EventPushRejected     = "push_rejected"
	EventCleanBuildFailed = "clean_build_failed"
	EventReplayMismatch   = "replay_mismatch"
)

var headlines = map[string]string{
	EventDriftDetected:    "drift detected - live behavior no longer matches the clean build",
	EventDriftHealed:      "drift healed - live behavior matches the clean build again",
	EventPushRejected:     "push rejected - fork failed verification and was destroyed",
	EventCleanBuildFailed: "clean build failed - the archivist cannot reproduce the app from source",
	EventReplayMismatch:   "replay mismatch - the fork answered recorded live traffic differently",
}

// Notifier posts events to a webhook. A nil Notifier or empty URL is a
// no-op, so callers never need to guard.
type Notifier struct {
	URL string
	App string
}

// Send delivers an event asynchronously; delivery failures are logged,
// never fatal - notifications must not be able to break deploys.
func (n *Notifier) Send(event, detail string) {
	if n == nil || n.URL == "" {
		return
	}
	line := "hotlane [" + n.App + "]: " + headlines[event]
	if detail != "" {
		line += "\n" + detail
	}
	body, _ := json.Marshal(map[string]string{
		"text":    line,
		"content": line,
		"app":     n.App,
		"event":   event,
		"detail":  detail,
		"at":      time.Now().UTC().Format(time.RFC3339),
	})
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Post(n.URL, "application/json", bytes.NewReader(body))
		if err != nil {
			log.Printf("notify: %s: %v", event, err)
			return
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			log.Printf("notify: %s: webhook returned %s", event, resp.Status)
		}
	}()
}
