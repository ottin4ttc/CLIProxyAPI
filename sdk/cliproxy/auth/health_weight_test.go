package auth

import (
	"context"
	"testing"
	"time"
)

func TestMarkResultRecordsOverloadInRing(t *testing.T) {
	mgr := NewManager(nil, nil, nil)
	auth := &Auth{ID: "auth-1", Provider: "codex", Metadata: map[string]any{"type": "codex"}}
	if _, err := mgr.Register(WithSkipPersist(context.Background()), auth); err != nil {
		t.Fatalf("Register returned error: %v", err)
	}

	mgr.MarkResult(context.Background(), Result{AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: true})
	mgr.MarkResult(context.Background(), Result{
		AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: false,
		Error: &Error{Code: "rate_limit", Message: "server overloaded", HTTPStatus: 429, Cause: FailureCauseOverload},
	})
	mgr.MarkResult(context.Background(), Result{
		AuthID: "auth-1", Provider: "codex", Model: "gpt-5", Success: false,
		Error: &Error{Code: "quota", Message: "quota exhausted", HTTPStatus: 429},
	})

	gotAuth, ok := mgr.GetByID("auth-1")
	if !ok || gotAuth == nil {
		t.Fatalf("GetByID returned ok=%v auth=%v", ok, gotAuth)
	}
	snapshot := gotAuth.RecentRequestsSnapshot(time.Now())
	var successTotal, failedTotal, overloadTotal int64
	for _, bucket := range snapshot {
		successTotal += bucket.Success
		failedTotal += bucket.Failed
		overloadTotal += bucket.Overload
	}
	if successTotal != 1 || failedTotal != 2 || overloadTotal != 1 {
		t.Fatalf("totals = success=%d failed=%d overload=%d, want 1/2/1", successTotal, failedTotal, overloadTotal)
	}
}

func TestRecordRecentRequestOverloadBucketRollover(t *testing.T) {
	t.Parallel()

	auth := &Auth{ID: "a"}
	base := time.Unix(1_700_000_000, 0)
	auth.recordRecentRequest(base, false, true)
	// Same ring slot one full rotation later must reset all counters.
	later := base.Add(time.Duration(recentRequestBucketSeconds*recentRequestBucketCount) * time.Second)
	auth.recordRecentRequest(later, false, false)

	bucket := auth.recentRequests.buckets[recentRequestBucketIndex(recentRequestBucketID(later))]
	if bucket.failed != 1 || bucket.overload != 0 {
		t.Fatalf("bucket = %+v, want failed=1 overload=0", bucket)
	}
}
