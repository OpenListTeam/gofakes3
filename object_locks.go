package gofakes3

import (
	"slices"
	"sync"
)

type objectLock struct {
	mu   sync.RWMutex
	refs int
}

// Locks coordinate requests handled by one GoFakeS3 instance. Direct backend
// mutations and requests handled by other instances require backend coordination.
type objectLocks struct {
	mu      sync.Mutex
	entries map[string]*objectLock
}

func (l *objectLocks) lock(bucket string, write bool, objects ...string) func() {
	keys := make([]string, len(objects))
	for i, object := range objects {
		keys[i] = bucket + "\x00" + object
	}
	slices.Sort(keys)
	keys = slices.Compact(keys)
	locks := make([]*objectLock, len(keys))
	l.mu.Lock()
	if l.entries == nil {
		l.entries = make(map[string]*objectLock)
	}
	for i, key := range keys {
		entry := l.entries[key]
		if entry == nil {
			entry = &objectLock{}
			l.entries[key] = entry
		}
		entry.refs++
		locks[i] = entry
	}
	l.mu.Unlock()
	for _, entry := range locks {
		if write {
			entry.mu.Lock()
		} else {
			entry.mu.RLock()
		}
	}
	return func() {
		for i := len(locks) - 1; i >= 0; i-- {
			if write {
				locks[i].mu.Unlock()
			} else {
				locks[i].mu.RUnlock()
			}
		}
		l.mu.Lock()
		defer l.mu.Unlock()
		for i, key := range keys {
			locks[i].refs--
			if locks[i].refs == 0 {
				delete(l.entries, key)
			}
		}
	}
}
