package calendar

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/roman-16/proton-cli/internal/proton"
)

// fakeDoer answers the events endpoint from a per-Type fixture and records which Type values
// were asked for.
type fakeDoer struct {
	mu       sync.Mutex
	byType   map[string][]rawEvent // Type -> events returned on page 0
	askedFor []string
	pages    map[string]int // Type -> how many pages were requested
}

func (f *fakeDoer) Do(context.Context, proton.Request) (*proton.Response, error) { return nil, nil }

func (f *fakeDoer) Decode(_ context.Context, r proton.Request, out any) error {
	typ := r.Query.Get("Type")
	page := r.Query.Get("Page")

	f.mu.Lock()
	f.askedFor = append(f.askedFor, typ)
	if f.pages == nil {
		f.pages = map[string]int{}
	}
	f.pages[typ]++
	events := f.byType[typ]
	f.mu.Unlock()

	// Only page 0 carries data; the pagination loop stops on the short/empty page.
	if page != "0" {
		events = nil
	}
	payload, err := json.Marshal(struct{ Events []rawEvent }{Events: events})
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, out)
}

// A window query must ask for all four Type quadrants. Asking only for Type=0 (part-day,
// starting in the window) silently hid every full-day event — trips and birthdays never
// reached the calendar — and every event that began before the window but reached into it.
func TestCollectRawEventsQueriesAllTypeQuadrants(t *testing.T) {
	f := &fakeDoer{byType: map[string][]rawEvent{
		"0": {{ID: "part-starting"}},
		"1": {{ID: "part-ongoing"}},
		"2": {{ID: "fullday-starting", FullDay: 1}},
		"3": {{ID: "fullday-ongoing", FullDay: 1}},
	}}
	s := New(f)

	got, err := s.collectRawEvents(context.Background(), "cal", time.Unix(0, 0), time.Unix(1, 0))
	if err != nil {
		t.Fatalf("collectRawEvents: %v", err)
	}

	asked := append([]string(nil), f.askedFor...)
	sort.Strings(asked)
	uniq := asked[:0]
	for i, v := range asked {
		if i == 0 || asked[i-1] != v {
			uniq = append(uniq, v)
		}
	}
	want := []string{"0", "1", "2", "3"}
	if len(uniq) != len(want) {
		t.Fatalf("queried Type values = %v, want %v", uniq, want)
	}
	for i := range want {
		if uniq[i] != want[i] {
			t.Fatalf("queried Type values = %v, want %v", uniq, want)
		}
	}

	ids := map[string]bool{}
	for _, e := range got {
		ids[e.ID] = true
	}
	for _, id := range []string{"part-starting", "part-ongoing", "fullday-starting", "fullday-ongoing"} {
		if !ids[id] {
			t.Errorf("event %q missing from the merged result", id)
		}
	}
}

// The same event legitimately matches more than one quadrant (a recurring series is both
// "starting" and "ongoing"), so the union must be de-duplicated by ID.
func TestCollectRawEventsDeduplicatesAcrossQuadrants(t *testing.T) {
	shared := rawEvent{ID: "recurring-series"}
	f := &fakeDoer{byType: map[string][]rawEvent{
		"0": {shared, {ID: "only-in-0"}},
		"1": {shared},
		"2": {},
		"3": {shared},
	}}
	s := New(f)

	got, err := s.collectRawEvents(context.Background(), "cal", time.Unix(0, 0), time.Unix(1, 0))
	if err != nil {
		t.Fatalf("collectRawEvents: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d events, want 2 (deduplicated)", len(got))
	}
	seen := map[string]int{}
	for _, e := range got {
		seen[e.ID]++
	}
	if seen["recurring-series"] != 1 {
		t.Errorf("recurring-series appeared %d times, want 1", seen["recurring-series"])
	}
}

// A full window must still be walked page by page within each quadrant.
func TestCollectRawEventsPaginatesWithinAQuadrant(t *testing.T) {
	full := make([]rawEvent, eventsPageSize)
	for i := range full {
		full[i] = rawEvent{ID: string(rune('a'+i%26)) + string(rune('0'+i/26))}
	}
	f := &fakeDoer{byType: map[string][]rawEvent{"0": full}}
	s := New(f)

	if _, err := s.collectRawEvents(context.Background(), "cal", time.Unix(0, 0), time.Unix(1, 0)); err != nil {
		t.Fatalf("collectRawEvents: %v", err)
	}
	// A full first page must trigger a second request; the empty page 1 then stops the loop.
	if f.pages["0"] < 2 {
		t.Errorf("Type=0 requested %d page(s), want >= 2 after a full page", f.pages["0"])
	}
}
