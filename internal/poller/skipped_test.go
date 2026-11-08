package poller

import (
	"reflect"
	"testing"

	"github.com/those-who-run/pit-crew/internal/github"
)

// A skipped verdict's green status lights nothing, and puts out a ready light left
// on the same head.
func TestReadyLeavesASkippedPullRequestOut(t *testing.T) {
	sha2 := "2222222222222222222222222222222222222222"
	skipped := github.Status{Context: "review / verdict", State: "success", Description: "skipped: kind A, lane standard", Creator: reviewer, CreatedAt: at(5)}
	src := world()
	src.pulls = map[string][]github.PullRequest{"those-who-run/ecu": {pull("those-who-run/ecu", 30, owner, sha2)}}
	src.statuses[sha2] = []github.Status{skipped}
	w := &statuses{}
	list, err := Ready(t.Context(), src, w, policies(), logger())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 0 || len(w.set) != 0 {
		t.Errorf("list %+v, set %q", list, w.set)
	}

	src.statuses[sha2] = []github.Status{skipped, {Context: "ladder / ready", State: "success", Description: "x", Creator: reviewer, CreatedAt: at(2)}}
	if list, err = Ready(t.Context(), src, w, policies(), logger()); err != nil || len(list) != 0 {
		t.Errorf("list %+v, %v", list, err)
	}
	want := []string{"those-who-run/ecu@2222 ladder / ready=pending (no longer ready: verdict-skipped)"}
	if !reflect.DeepEqual(w.set, want) {
		t.Errorf("set %q, want %q", w.set, want)
	}
}
