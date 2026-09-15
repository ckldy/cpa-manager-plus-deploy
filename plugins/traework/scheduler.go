package main

import (
	"context"
	"errors"
	"sync"
	"time"
)

type ScheduledAccount struct {
	AuthIndex string
	Name      string
	UID       string
	Disabled  bool
}

type MarkerStore interface {
	Load(context.Context, ScheduledAccount) (CheckinMarker, error)
	Save(context.Context, ScheduledAccount, CheckinMarker) error
}

type DailySchedulerConfig struct {
	Client      AccountClient
	MarkerStore MarkerStore
	HostAuth    *HostAuth
	Location    *time.Location
	Concurrency int
	Enabled     *bool
}

type DailyScheduler struct {
	client      AccountClient
	markers     MarkerStore
	location    *time.Location
	concurrency int
	enabled     bool

	mu       sync.Mutex
	days     map[string]map[string]struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
}

func NewDailyScheduler(config DailySchedulerConfig) *DailyScheduler {
	loc := config.Location
	if loc == nil {
		var err error
		loc, err = time.LoadLocation("Asia/Hong_Kong")
		if err != nil {
			loc = time.FixedZone("Asia/Hong_Kong", 8*60*60)
		}
	}
	concurrency := config.Concurrency
	if concurrency <= 0 {
		concurrency = 2
	}
	enabled := true
	if config.Enabled != nil {
		enabled = *config.Enabled
	}
	markers := config.MarkerStore
	if markers == nil && config.HostAuth != nil {
		markers = HostAuthMarkerStore{Auth: *config.HostAuth}
	}
	return &DailyScheduler{client: config.Client, markers: markers, location: loc, concurrency: concurrency, enabled: enabled, days: make(map[string]map[string]struct{}), stop: make(chan struct{}), done: make(chan struct{})}
}

func (s *DailyScheduler) Enabled() bool            { return s.enabled }
func (s *DailyScheduler) Location() *time.Location { return s.location }
func (s *DailyScheduler) Done() <-chan struct{}    { return s.done }
func (s *DailyScheduler) Stop()                    { s.stopOnce.Do(func() { close(s.stop); close(s.done) }) }

// RunDay is idempotent per Hong Kong calendar day and account. A mark is made
// only after the status/claim/credits sequence succeeds, so failures may retry.
func (s *DailyScheduler) RunDay(ctx context.Context, now time.Time, accounts []ScheduledAccount) error {
	if !s.enabled {
		return nil
	}
	if s.client == nil {
		return errors.New("daily scheduler client unavailable")
	}
	day := now.In(s.location).Format("2006-01-02")
	sem := make(chan struct{}, s.concurrency)
	var wg sync.WaitGroup
	var firstErr error
	var errMu sync.Mutex
	for _, account := range accounts {
		account := account
		key := accountKey(account)
		if account.Disabled || key == "" {
			continue
		}
		if s.markers != nil {
			marker, err := s.markers.Load(ctx, account)
			if err != nil {
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				continue
			}
			if marker.LastDay == day && (marker.LastStatus == "success" || marker.LastStatus == "already_checked") {
				continue
			}
		}
		if !s.reserve(day, key) {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				s.unmark(day, key)
				return
			case <-s.stop:
				s.unmark(day, key)
				return
			}
			defer func() { <-sem }()
			status, err := s.runAccount(ctx, account)
			if err == nil && status == "" {
				s.unmark(day, key)
				return
			}
			if err == nil && s.markers != nil {
				err = s.markers.Save(ctx, account, CheckinMarker{LastDay: day, LastStatus: status, LastAt: now.Unix()})
			}
			if err != nil {
				s.unmark(day, key)
				errMu.Lock()
				if firstErr == nil {
					firstErr = err
				}
				errMu.Unlock()
				return
			}
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return firstErr
	}
	return ctx.Err()
}

func (s *DailyScheduler) runAccount(ctx context.Context, account ScheduledAccount) (string, error) {
	checked, _, enabled, err := s.client.CheckinStatus(ctx, account)
	if err != nil {
		return "", err
	}
	status := "already_checked"
	if !enabled && !checked {
		return "", nil
	}
	if enabled && !checked {
		// Retry claim up to 3 times with backoff for rate-limited responses.
		for attempt := 0; attempt < 3; attempt++ {
			if attempt > 0 {
				select {
				case <-time.After(time.Duration(10*(attempt+1)) * time.Second):
				case <-ctx.Done():
					return "", ctx.Err()
				case <-s.stop:
					return "", errors.New("scheduler stopped")
				}
			}
			if err := s.client.CheckinClaim(ctx, account); err != nil {
				if attempt < 2 {
					continue
				}
				return "claim_failed", err
			}
			checked, _, _, err = s.client.CheckinStatus(ctx, account)
			if err != nil {
				return "", err
			}
			if checked {
				break
			}
		}
		if !checked {
			return "claim_failed", errors.New("checkin_not_confirmed: claim was accepted but upstream status did not confirm success")
		}
		status = "success"
	}
	_, err = s.client.Credits(ctx, account)
	if err != nil {
		return "", err
	}
	return status, nil
}

func accountKey(account ScheduledAccount) string {
	if account.UID != "" {
		return account.UID
	}
	return account.AuthIndex
}
func (s *DailyScheduler) reserve(day, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.days[day] == nil {
		s.days[day] = make(map[string]struct{})
	}
	if _, exists := s.days[day][key]; exists {
		return false
	}
	s.days[day][key] = struct{}{}
	return true
}
func (s *DailyScheduler) unmark(day, key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.days[day], key)
}
