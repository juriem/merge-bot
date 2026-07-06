package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"context"

	"mergebot/internal/queue"
	"mergebot/internal/queuestats"
	"mergebot/internal/review"
)

type fakeQueue struct {
	added        []int
	removed      []int
	requeued     []int
	clearedWith  [][]queue.Phase
	clearedCount int
}

func (f *fakeQueue) List() []queue.Item { return nil }
func (f *fakeQueue) Add(n int)          { f.added = append(f.added, n) }
func (f *fakeQueue) Remove(n int)       { f.removed = append(f.removed, n) }
func (f *fakeQueue) Requeue(n int) bool { f.requeued = append(f.requeued, n); return true }
func (f *fakeQueue) Clear(p []queue.Phase) {
	f.clearedWith = append(f.clearedWith, p)
	f.clearedCount++
}

func do(t *testing.T, h http.Handler, method, target, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func Test_removeItem_RoutesToRemove(t *testing.T) {
	q := &fakeQueue{}
	h := New(q, "o/r", nil).Handler()

	if w := do(t, h, http.MethodDelete, "/api/items/42", ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(q.removed) != 1 || q.removed[0] != 42 {
		t.Fatalf("removed = %v, want [42]", q.removed)
	}
	if q.clearedCount != 0 {
		t.Fatal("clear must not be called for a numbered delete")
	}
}

func Test_clearItems_RoutesToClearWithPhases(t *testing.T) {
	q := &fakeQueue{}
	h := New(q, "o/r", nil).Handler()

	if w := do(t, h, http.MethodDelete, "/api/items?phase=merged,stopped", ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(q.clearedWith) != 1 {
		t.Fatalf("clear called %d times, want 1", len(q.clearedWith))
	}
	got := q.clearedWith[0]
	if len(got) != 2 || got[0] != queue.PhaseMerged || got[1] != queue.PhaseStopped {
		t.Fatalf("cleared phases = %v, want [merged stopped]", got)
	}
	if len(q.removed) != 0 {
		t.Fatal("remove must not be called for a collection delete")
	}
}

func Test_clearItems_RequiresPhase(t *testing.T) {
	q := &fakeQueue{}
	h := New(q, "o/r", nil).Handler()

	if w := do(t, h, http.MethodDelete, "/api/items", ""); w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	if q.clearedCount != 0 {
		t.Fatal("clear must not run without a phase")
	}
}

func Test_requeueItem_RoutesToRequeue(t *testing.T) {
	q := &fakeQueue{}
	h := New(q, "o/r", nil).Handler()

	if w := do(t, h, http.MethodPost, "/api/items/7264/requeue", ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(q.requeued) != 1 || q.requeued[0] != 7264 {
		t.Fatalf("requeued = %v, want [7264]", q.requeued)
	}
	if len(q.added) != 0 || len(q.removed) != 0 {
		t.Fatal("requeue must not add or remove")
	}
}

func Test_addItem_RoutesToAdd(t *testing.T) {
	q := &fakeQueue{}
	h := New(q, "o/r", nil).Handler()

	if w := do(t, h, http.MethodPost, "/api/items", `{"number":7}`); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if len(q.added) != 1 || q.added[0] != 7 {
		t.Fatalf("added = %v, want [7]", q.added)
	}
}

type fakeReviewer struct {
	entries  []review.Entry
	loaded   bool
	refreshN *int
}

func (f fakeReviewer) List() []review.Entry { return f.entries }
func (f fakeReviewer) Loaded() bool         { return f.loaded }
func (f fakeReviewer) TriggerRefresh() {
	if f.refreshN != nil {
		*f.refreshN++
	}
}

func Test_readyForReview_ReturnsDashboard(t *testing.T) {
	q := &fakeQueue{}
	rv := fakeReviewer{entries: []review.Entry{{Number: 5, Title: "x", Approvals: 1, Required: 2}}, loaded: true}
	h := New(q, "o/r", rv).Handler()

	w := do(t, h, http.MethodGet, "/api/ready", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"number":5`) || !strings.Contains(body, `"required":2`) || !strings.Contains(body, `"loaded":true`) {
		t.Fatalf("unexpected body: %s", body)
	}
}

func Test_refreshReady_TriggersDashboard(t *testing.T) {
	n := 0
	rv := fakeReviewer{refreshN: &n}
	h := New(&fakeQueue{}, "o/r", rv).Handler()

	if w := do(t, h, http.MethodPost, "/api/ready/refresh", ""); w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if n != 1 {
		t.Fatalf("TriggerRefresh called %d times, want 1", n)
	}
}

type fakeStats struct {
	history  []queuestats.Snapshot
	recorded []int
}

func (f *fakeStats) History() []queuestats.Snapshot { return f.history }
func (f *fakeStats) Record(waiting int)             { f.recorded = append(f.recorded, waiting) }

type fakeProber struct{ probed []int }

func (f *fakeProber) Probe(_ context.Context, number int) (string, error) {
	f.probed = append(f.probed, number)
	return "Queue length: 3", nil
}

func Test_queueStats_ReturnsHistory(t *testing.T) {
	st := &fakeStats{history: []queuestats.Snapshot{{Waiting: 4}}}
	h := New(&fakeQueue{}, "o/r", nil).WithStats(st).Handler()

	w := do(t, h, http.MethodGet, "/api/queuestats", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"waiting":4`) {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func Test_probeStatus_RecordsQueueDepth(t *testing.T) {
	st := &fakeStats{}
	h := New(&fakeQueue{}, "o/r", nil).WithStats(st).WithProber(&fakeProber{}).Handler()

	if w := do(t, h, http.MethodPost, "/api/items/1/status", ""); w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(st.recorded) != 1 || st.recorded[0] != 3 {
		t.Fatalf("recorded = %v, want [3] parsed from the bot reply", st.recorded)
	}
}

func Test_queueStats_EmptyWithoutCollector(t *testing.T) {
	h := New(&fakeQueue{}, "o/r", nil).Handler()

	w := do(t, h, http.MethodGet, "/api/queuestats", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"history":[]`) {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
}

func Test_probeStatus_RoutesToProber(t *testing.T) {
	p := &fakeProber{}
	h := New(&fakeQueue{}, "o/r", nil).WithProber(p).Handler()

	w := do(t, h, http.MethodPost, "/api/items/7416/status", "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Queue length: 3") {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	if len(p.probed) != 1 || p.probed[0] != 7416 {
		t.Fatalf("probed = %v, want [7416]", p.probed)
	}
}

func Test_probeStatus_NotImplementedWithoutProber(t *testing.T) {
	h := New(&fakeQueue{}, "o/r", nil).Handler()

	if w := do(t, h, http.MethodPost, "/api/items/1/status", ""); w.Code != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", w.Code)
	}
}

func Test_readyForReview_NilReviewerReportsLoaded(t *testing.T) {
	h := New(&fakeQueue{}, "o/r", nil).Handler()

	w := do(t, h, http.MethodGet, "/api/ready", "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if !strings.Contains(w.Body.String(), `"loaded":true`) {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}
