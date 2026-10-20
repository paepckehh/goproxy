package clients

import (
	"strconv"
	"testing"
	"time"
)

func TestTracker_TopRankings(t *testing.T) {
	t.Parallel()
	tr := New()
	tr.Record("10.0.0.1", 1000)
	tr.Record("10.0.0.1", 1000)
	tr.Record("10.0.0.2", 9000)
	tr.Record("", 500) // unusable address: ignored

	byReq := tr.TopByRequests(1)
	if len(byReq) != 1 || byReq[0].ID != "10.0.0.1" || byReq[0].Requests != 2 {
		t.Fatalf("by requests = %+v, want 10.0.0.1 with 2 requests", byReq)
	}
	byBytes := tr.TopByBytes(2)
	if len(byBytes) != 2 || byBytes[0].ID != "10.0.0.2" || byBytes[0].BytesDown != 9000 {
		t.Fatalf("by bytes = %+v, want 10.0.0.2 first", byBytes)
	}
	if tr.Count() != 2 {
		t.Errorf("tracked = %d, want 2 (empty ids ignored)", tr.Count())
	}
}

func TestTracker_RestoreAndDirty(t *testing.T) {
	t.Parallel()
	tr := New()
	tr.Restore(map[string]Usage{
		"10.0.0.9": {Requests: 7, BytesDown: 4096, LastSeen: time.Unix(1700000000, 0).UTC()},
	})
	top := tr.TopByRequests(-1)
	if len(top) != 1 || top[0].Requests != 7 || top[0].BytesDown != 4096 {
		t.Fatalf("restored ranking = %+v", top)
	}
	if dirty := tr.TakeDirty(); len(dirty) != 0 {
		t.Errorf("restored counters must not be dirty, got %+v", dirty)
	}
	tr.Record("10.0.0.9", 10)
	dirty := tr.TakeDirty()
	if u, ok := dirty["10.0.0.9"]; !ok || u.Requests != 8 || u.BytesDown != 4106 {
		t.Fatalf("dirty = %+v, want the restored plus new counters", dirty)
	}
	if dirty := tr.TakeDirty(); len(dirty) != 0 {
		t.Errorf("second take = %+v, want clean", dirty)
	}
}

func TestTracker_Bounded(t *testing.T) {
	t.Parallel()
	tr := New()
	for i := 0; i < MaxTracked+100; i++ {
		tr.Record(string(rune('a'+i%26))+"-"+time.Now().String()+"-"+strconv.Itoa(i), 1)
	}
	if n := tr.Count(); n > MaxTracked {
		t.Errorf("tracked = %d, want at most %d", n, MaxTracked)
	}
}
