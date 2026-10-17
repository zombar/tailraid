package webui_test

import (
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/zombar/tailraid/internal/admin"
	"github.com/zombar/tailraid/internal/rpc"
	"github.com/zombar/tailraid/internal/webui"
)

// The dashboard's status carries a done or cancelled restore job for RestoreShown after
// it ends and not after, a failed one as long as its host lists it, and every unfinished
// one; `status` itself is not filtered (docs/16 §6, I-390).
// Invariant: I-390
func TestStatus_FinishedRestoresLeaveTheDashboard(t *testing.T) {
	var jobs []rpc.RestoreJob
	u := newUI(t, func(o *webui.Options) {
		o.Status = func() any { return admin.Report{Node: "one", Restores: jobs} }
	})
	now := u.f.Clk.Now()
	ago := func(d time.Duration) int64 { return now.Add(-d).UnixNano() }
	jobs = []rpc.RestoreJob{
		{ID: "running", State: rpc.RestoreRunning},
		{ID: "waiting", State: rpc.RestoreWaiting},
		{ID: "done-now", State: rpc.RestoreDone, Ended: ago(time.Minute)},
		{ID: "done-edge", State: rpc.RestoreDone, Ended: ago(webui.RestoreShown)},
		{ID: "done-old", State: rpc.RestoreDone, Ended: ago(webui.RestoreShown + time.Second)},
		{ID: "cancelled-old", State: rpc.RestoreCancelled, Ended: ago(2 * time.Hour)},
		{ID: "failed-old", State: rpc.RestoreFailed, Ended: ago(6 * 24 * time.Hour)},
	}
	code, _, body := u.get("/api/status")
	var rep struct {
		Restores []rpc.RestoreJob `json:"restores"`
	}
	if code != 200 || json.Unmarshal(body, &rep) != nil {
		t.Fatalf("status: %d %s", code, body)
	}
	var got []string
	for _, j := range rep.Restores {
		got = append(got, j.ID)
	}
	if want := []string{"running", "waiting", "done-now", "done-edge", "failed-old"}; !slices.Equal(got, want) {
		t.Fatalf("the dashboard lists %v, want %v", got, want)
	}
	if len(jobs) != 7 {
		t.Fatal("the filter changed the report it was handed")
	}
}
