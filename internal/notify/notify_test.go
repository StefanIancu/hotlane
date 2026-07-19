package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSendDeliversEveryEvent(t *testing.T) {
	got := make(chan map[string]string, 8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p map[string]string
		json.Unmarshal(body, &p)
		got <- p
	}))
	defer srv.Close()

	n := &Notifier{URL: srv.URL, App: "demo"}
	for event := range headlines {
		n.Send(event, "detail for "+event)
	}

	seen := map[string]bool{}
	for range headlines {
		select {
		case p := <-got:
			seen[p["event"]] = true
			if p["app"] != "demo" {
				t.Errorf("app = %q", p["app"])
			}
			if !strings.Contains(p["text"], headlines[p["event"]]) {
				t.Errorf("%s text missing headline: %q", p["event"], p["text"])
			}
			if p["text"] != p["content"] {
				t.Errorf("%s slack/discord fields differ", p["event"])
			}
			if !strings.Contains(p["detail"], p["event"]) {
				t.Errorf("%s detail not carried: %q", p["event"], p["detail"])
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("only %d/%d events delivered", len(seen), len(headlines))
		}
	}
	for event := range headlines {
		if !seen[event] {
			t.Errorf("event %s never delivered", event)
		}
	}
}

func TestNilAndEmptyNotifierAreNoOps(t *testing.T) {
	var n *Notifier
	n.Send(EventPushRejected, "x") // must not panic
	(&Notifier{}).Send(EventPushRejected, "x")
}
