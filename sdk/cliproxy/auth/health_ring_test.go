package auth

import (
	"testing"
	"time"
)

// healthRingTestNow returns a fixed time sitting mid-bucket so bucket ids are stable.
func healthRingTestNow() time.Time {
	return time.Unix(2911037*recentRequestBucketSeconds+300, 0).UTC()
}

func recordBucketRequests(a *Auth, at time.Time, success, overloadFailed int) {
	for i := 0; i < success; i++ {
		a.recordRecentRequest(at, true, false)
	}
	for i := 0; i < overloadFailed; i++ {
		a.recordRecentRequest(at, false, true)
	}
}

func bucketTime(now time.Time, bucketsBack int64) time.Time {
	return now.Add(-time.Duration(bucketsBack*recentRequestBucketSeconds) * time.Second)
}

func TestExportRecentRequestsSkipsEmptyAndSorts(t *testing.T) {
	now := healthRingTestNow()
	auth := &Auth{ID: "a"}
	recordBucketRequests(auth, bucketTime(now, 3), 5, 1)
	recordBucketRequests(auth, bucketTime(now, 1), 7, 0)
	recordBucketRequests(auth, now, 2, 0)

	buckets := auth.exportRecentRequests()
	if len(buckets) != 3 {
		t.Fatalf("exportRecentRequests() returned %d buckets, want 3", len(buckets))
	}
	currentID := recentRequestBucketID(now)
	wantIDs := []int64{currentID - 3, currentID - 1, currentID}
	for i, bucket := range buckets {
		if bucket.BucketID != wantIDs[i] {
			t.Fatalf("bucket[%d].BucketID = %d, want %d", i, bucket.BucketID, wantIDs[i])
		}
	}
	if buckets[0].Success != 5 || buckets[0].Failed != 1 || buckets[0].Overload != 1 {
		t.Fatalf("bucket[0] = %+v, want success=5 failed=1 overload=1", buckets[0])
	}
	if buckets[1].Success != 7 || buckets[2].Success != 2 {
		t.Fatalf("unexpected counts: %+v", buckets)
	}
}

func TestImportRecentRequestsRoundTripPreservesHealthTier(t *testing.T) {
	now := healthRingTestNow()
	source := &Auth{ID: "a"}
	recordBucketRequests(source, bucketTime(now, 1), 40, 0)
	recordBucketRequests(source, bucketTime(now, 2), 40, 0)
	recordBucketRequests(source, bucketTime(now, 3), 40, 0)

	wantTier := source.HealthTier(now)
	if wantTier != healthTierBoostMax {
		t.Fatalf("source HealthTier = %d, want %d", wantTier, healthTierBoostMax)
	}

	restored := &Auth{ID: "a"}
	restored.importRecentRequests(now, source.exportRecentRequests())
	if got := restored.HealthTier(now); got != wantTier {
		t.Fatalf("restored HealthTier = %d, want %d", got, wantTier)
	}
	gotTotal, gotOverload := restored.overloadWindowStats(now)
	wantTotal, wantOverload := source.overloadWindowStats(now)
	if gotTotal != wantTotal || gotOverload != wantOverload {
		t.Fatalf("restored window stats = (%d, %d), want (%d, %d)", gotTotal, gotOverload, wantTotal, wantOverload)
	}
}

func TestImportRecentRequestsDropsOutOfWindowBuckets(t *testing.T) {
	now := healthRingTestNow()
	currentID := recentRequestBucketID(now)
	auth := &Auth{ID: "a"}
	auth.importRecentRequests(now, []PersistedRequestBucket{
		{BucketID: currentID - int64(recentRequestBucketCount), Success: 9},     // exactly one span back: dropped
		{BucketID: currentID - int64(recentRequestBucketCount) - 5, Success: 9}, // older: dropped
		{BucketID: currentID + 1, Success: 9},                                   // future: dropped
	})
	if got := auth.exportRecentRequests(); len(got) != 0 {
		t.Fatalf("out-of-window buckets were imported: %+v", got)
	}
}

func TestImportRecentRequestsKeepsWindowBoundaries(t *testing.T) {
	now := healthRingTestNow()
	currentID := recentRequestBucketID(now)
	auth := &Auth{ID: "a"}
	auth.importRecentRequests(now, []PersistedRequestBucket{
		{BucketID: currentID, Success: 3},                                       // current bucket: kept
		{BucketID: currentID - int64(recentRequestBucketCount) + 1, Success: 4}, // oldest kept id
	})
	got := auth.exportRecentRequests()
	if len(got) != 2 {
		t.Fatalf("exportRecentRequests() returned %d buckets, want 2: %+v", len(got), got)
	}
	if got[0].BucketID != currentID-int64(recentRequestBucketCount)+1 || got[0].Success != 4 {
		t.Fatalf("oldest kept bucket = %+v", got[0])
	}
	if got[1].BucketID != currentID || got[1].Success != 3 {
		t.Fatalf("current bucket = %+v", got[1])
	}
}

func TestImportRecentRequestsBucketOneSpanAwayDoesNotOverwriteCurrent(t *testing.T) {
	now := healthRingTestNow()
	currentID := recentRequestBucketID(now)
	auth := &Auth{ID: "a"}
	recordBucketRequests(auth, now, 6, 0)

	auth.importRecentRequests(now, []PersistedRequestBucket{
		{BucketID: currentID - int64(recentRequestBucketCount), Success: 99},
	})
	got := auth.exportRecentRequests()
	if len(got) != 1 || got[0].BucketID != currentID || got[0].Success != 6 {
		t.Fatalf("current bucket was clobbered: %+v", got)
	}
}

func TestImportRecentRequestsNeverOverwritesOccupiedSlot(t *testing.T) {
	now := healthRingTestNow()
	currentID := recentRequestBucketID(now)
	auth := &Auth{ID: "a"}
	recordBucketRequests(auth, bucketTime(now, 3), 11, 2)

	auth.importRecentRequests(now, []PersistedRequestBucket{
		{BucketID: currentID - 3, Success: 500, Failed: 500, Overload: 500},
	})
	got := auth.exportRecentRequests()
	if len(got) != 1 {
		t.Fatalf("exportRecentRequests() returned %d buckets, want 1", len(got))
	}
	if got[0].Success != 11 || got[0].Failed != 2 || got[0].Overload != 2 {
		t.Fatalf("occupied slot was overwritten: %+v", got[0])
	}
}
