package poller

import (
	"testing"

	"github.com/those-who-run/pit-crew/internal/github"
)

// TestGatherReadsReviewsOnlyForBuilderPulls shows the reviews are read for a
// builder's pull request and not for another author's.
func TestGatherReadsReviewsOnlyForBuilderPulls(t *testing.T) {
	src := world()
	src.reviews = map[string][]github.Review{
		key("those-who-run/ecu", 31): {{Author: owner, State: "CHANGES_REQUESTED", CommitSHA: head, SubmittedAt: at(5)}},
		key("those-who-run/ecu", 30): {{Author: owner, State: "CHANGES_REQUESTED", CommitSHA: "2222222222222222222222222222222222222222", SubmittedAt: at(5)}},
	}
	p := policies().Pull
	for _, pr := range src.pulls["those-who-run/ecu"] {
		if _, err := Gather(t.Context(), src, p, pr); err != nil {
			t.Fatal(err)
		}
	}
	if len(src.reviewCalls) != 1 || src.reviewCalls[0] != key("those-who-run/ecu", 31) {
		t.Fatalf("reviews read for %v, want only the builder's pull request", src.reviewCalls)
	}
	got, err := Gather(t.Context(), src, p, src.pulls["those-who-run/ecu"][0])
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Reviews) != 1 || got.Reviews[0].State != "CHANGES_REQUESTED" || got.Reviews[0].Author != owner {
		t.Fatalf("reviews = %+v", got.Reviews)
	}

	// With no maintainer configured, none are read.
	src.reviewCalls = nil
	p.Maintainer = ""
	if _, err := Gather(t.Context(), src, p, src.pulls["those-who-run/ecu"][0]); err != nil {
		t.Fatal(err)
	}
	if len(src.reviewCalls) != 0 {
		t.Fatalf("reviews read with no maintainer: %v", src.reviewCalls)
	}
}
