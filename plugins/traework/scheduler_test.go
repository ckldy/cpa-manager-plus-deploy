package main

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type fakeDailyClient struct {
	active atomic.Int32
	max    atomic.Int32
	claims atomic.Int32
	mu     sync.Mutex
	status map[string]int
}

func (f *fakeDailyClient) CheckinStatus(_ context.Context, account ScheduledAccount) (bool, int64, bool, error) {
	n := f.active.Add(1)
	defer f.active.Add(-1)
	for old := f.max.Load(); n > old && !f.max.CompareAndSwap(old, n); old = f.max.Load() {
	}
	time.Sleep(10 * time.Millisecond)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = make(map[string]int)
	}
	key := accountKey(account)
	f.status[key]++
	return f.status[key]%2 == 0, 1, true, nil
}
func (f *fakeDailyClient) CheckinClaim(context.Context, ScheduledAccount) error {
	f.claims.Add(1)
	return nil
}
func (f *fakeDailyClient) Credits(context.Context, ScheduledAccount) (int64, error) { return 2, nil }

func TestDailySchedulerDefaultsAndIdempotency(t *testing.T) {
	client := &fakeDailyClient{}
	s := NewDailyScheduler(DailySchedulerConfig{Client: client})
	if !s.Enabled() {
		t.Fatal("scheduler must default enabled")
	}
	if s.Location().String() != "Asia/Hong_Kong" {
		t.Fatalf("location=%s", s.Location())
	}
	accounts := []ScheduledAccount{{AuthIndex: "1", UID: "u1"}, {AuthIndex: "2", UID: "u2"}, {AuthIndex: "3", UID: "u3"}}
	now := time.Date(2026, 8, 31, 9, 0, 0, 0, s.Location())
	if err := s.RunDay(context.Background(), now, accounts); err != nil {
		t.Fatal(err)
	}
	if err := s.RunDay(context.Background(), now.Add(time.Hour), accounts); err != nil {
		t.Fatal(err)
	}
	if got := client.claims.Load(); got != 3 {
		t.Fatalf("claims=%d want 3", got)
	}
	if got := client.max.Load(); got > 2 {
		t.Fatalf("max concurrency=%d want <=2", got)
	}
}

func TestDailySchedulerPersistsAcrossInstancesAndResumesNextDay(t *testing.T) {
	client := &fakeDailyClient{}
	store := &memoryMarkerStore{markers: map[string]CheckinMarker{}}
	account := ScheduledAccount{AuthIndex: "1", Name: "traework-u1.json", UID: "u1"}
	day1 := time.Date(2026, 8, 31, 9, 0, 0, 0, time.FixedZone("HKT", 8*60*60))
	s1 := NewDailyScheduler(DailySchedulerConfig{Client: client, MarkerStore: store})
	if err := s1.RunDay(context.Background(), day1, []ScheduledAccount{account}); err != nil {
		t.Fatal(err)
	}
	s2 := NewDailyScheduler(DailySchedulerConfig{Client: client, MarkerStore: store})
	if err := s2.RunDay(context.Background(), day1.Add(time.Hour), []ScheduledAccount{account}); err != nil {
		t.Fatal(err)
	}
	if got := client.claims.Load(); got != 1 {
		t.Fatalf("same-day claims=%d want 1", got)
	}
	if err := s2.RunDay(context.Background(), day1.Add(24*time.Hour), []ScheduledAccount{account}); err != nil {
		t.Fatal(err)
	}
	if got := client.claims.Load(); got != 2 {
		t.Fatalf("next-day claims=%d want 2", got)
	}
}

type memoryMarkerStore struct {
	mu      sync.Mutex
	markers map[string]CheckinMarker
}

func (m *memoryMarkerStore) Load(_ context.Context, account ScheduledAccount) (CheckinMarker, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.markers[accountKey(account)], nil
}
func (m *memoryMarkerStore) Save(_ context.Context, account ScheduledAccount, marker CheckinMarker) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.markers[accountKey(account)] = marker
	return nil
}

func TestDailySchedulerSkipsDisabledAndAlreadyChecked(t *testing.T) {
	client := &fakeDailyClient{}
	s := NewDailyScheduler(DailySchedulerConfig{Client: client})
	accounts := []ScheduledAccount{{AuthIndex: "1", UID: "u1", Disabled: true}}
	if err := s.RunDay(context.Background(), time.Now(), accounts); err != nil {
		t.Fatal(err)
	}
	if client.claims.Load() != 0 {
		t.Fatal("disabled account was claimed")
	}
}

func TestDailySchedulerStopIsIdempotent(t *testing.T) {
	s := NewDailyScheduler(DailySchedulerConfig{Client: &fakeDailyClient{}})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.Stop() }()
	go func() { defer wg.Done(); s.Stop() }()
	wg.Wait()
	select {
	case <-s.Done():
	default:
		t.Fatal("done channel not closed")
	}
}
