package pool

import (
	"path/filepath"
	"testing"
)

func versions(entries []RingEntry) []int {
	out := make([]int, len(entries))
	for i, e := range entries {
		out[i] = e.Version
	}
	return out
}

func ring(vs ...int) []RingEntry {
	out := make([]RingEntry, len(vs))
	for i, v := range vs {
		out[i] = RingEntry{Version: v}
	}
	return out
}

func TestPruneOrder(t *testing.T) {
	cases := []struct {
		name    string
		entries []int // newest-version first, as ring() returns
		history []int // most-recently-live first
		want    []int
	}{
		{
			name:    "no history falls back to version order",
			entries: []int{9, 8, 7, 6},
			history: nil,
			want:    []int{9, 8, 7, 6},
		},
		{
			name:    "steady pushes match version order",
			entries: []int{9, 8, 7, 6},
			history: []int{10, 9, 8, 7, 6},
			want:    []int{9, 8, 7, 6},
		},
		{
			// The bug: rollback v9->v5, then push v10. v5 was serving
			// seconds ago and must outrank v8/v7/v6 despite its number.
			name:    "rolled-back-to version ranks by recency",
			entries: []int{9, 8, 7, 6, 5},
			history: []int{10, 5, 9, 8, 7, 6},
			want:    []int{5, 9, 8, 7, 6},
		},
		{
			name:    "entries missing from history sort last by version",
			entries: []int{9, 8, 7, 6, 5},
			history: []int{10, 5, 9},
			want:    []int{5, 9, 8, 7, 6},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := versions(pruneOrder(ring(tc.entries...), tc.history))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestLiveHistoryRoundTrip(t *testing.T) {
	p := &Pool{DataDir: t.TempDir()}

	if h := p.liveHistory(); h != nil {
		t.Fatalf("empty history = %v, want nil", h)
	}
	for _, v := range []int{1, 2, 3, 2} { // 2 flips live twice (rollback)
		p.recordHistory(v)
	}
	got := p.liveHistory()
	want := []int{2, 3, 1} // deduped, most recent first
	if len(got) != len(want) {
		t.Fatalf("history = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("history = %v, want %v", got, want)
		}
	}
}

func TestRecordHistoryCap(t *testing.T) {
	p := &Pool{DataDir: t.TempDir()}
	for v := 1; v <= maxHistory+10; v++ {
		p.recordHistory(v)
	}
	got := p.liveHistory()
	if len(got) != maxHistory {
		t.Fatalf("history length = %d, want %d", len(got), maxHistory)
	}
	if got[0] != maxHistory+10 {
		t.Fatalf("newest = %d, want %d", got[0], maxHistory+10)
	}
}

func TestRecordLiveWritesHistory(t *testing.T) {
	p := &Pool{DataDir: t.TempDir()}
	p.recordLive("hotlane-demo-v7")
	if got := p.liveHistory(); len(got) != 1 || got[0] != 7 {
		t.Fatalf("history after recordLive = %v, want [7]", got)
	}
	if _, err := filepath.Glob(filepath.Join(p.DataDir, "live-history")); err != nil {
		t.Fatal(err)
	}
}
