package main

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

const (
	confirmationTTL   = 5 * time.Minute
	confirmationLimit = 128
)

type confirmationEntry struct {
	Purpose   string
	Binding   string
	ExpiresAt time.Time
}

type confirmationStore struct {
	sync.Mutex
	items map[string]confirmationEntry
}

func newConfirmationStore() *confirmationStore {
	return &confirmationStore{items: make(map[string]confirmationEntry)}
}

func (s *confirmationStore) create(purpose, binding string, now time.Time) (string, error) {
	if purpose == "" || binding == "" {
		return "", errors.New("invalid confirmation binding")
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", errors.New("confirmation unavailable")
	}
	token := hex.EncodeToString(random[:])
	s.Lock()
	defer s.Unlock()
	for value, item := range s.items {
		if !item.ExpiresAt.After(now) {
			delete(s.items, value)
		}
	}
	if len(s.items) >= confirmationLimit {
		return "", errors.New("too many pending confirmations")
	}
	s.items[token] = confirmationEntry{Purpose: purpose, Binding: binding, ExpiresAt: now.Add(confirmationTTL)}
	return token, nil
}

func (s *confirmationStore) consume(token, purpose, binding string, now time.Time) bool {
	s.Lock()
	defer s.Unlock()
	item, ok := s.items[token]
	if ok {
		delete(s.items, token)
	}
	return ok && item.ExpiresAt.After(now) && item.Purpose == purpose && item.Binding == binding
}

var confirmations = newConfirmationStore()
