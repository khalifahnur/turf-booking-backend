package utils

import "sync"

type SlotLocker struct {
	mu    sync.Mutex
	locks map[string]*lockEntry
}

type lockEntry struct {
	mutex sync.Mutex
	count int // Tracks how many Goroutines are waiting for this specific lock
}

func NewSlotLocker() *SlotLocker {
	return &SlotLocker{
		locks: make(map[string]*lockEntry),
	}
}

// Lock acquires a lock for a specific key
func (l *SlotLocker) Lock(key string) {
	// 1. Lock the map just long enough to find or create the padlock
	l.mu.Lock()
	entry, exists := l.locks[key]
	if !exists {
		entry = &lockEntry{}
		l.locks[key] = entry
	}
	entry.count++ // I am waiting in line for this key
	l.mu.Unlock() // Unlock the map so others can access different keys

	// 2. Lock the specific key's padlock
	entry.mutex.Lock()
}

// Unlock releases the lock for a specific key and cleans up memory
func (l *SlotLocker) Unlock(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	entry, exists := l.locks[key]
	if !exists {
		return
	}

	// 1. I am leaving the line
	entry.count--

	// 2. If no one else is waiting, throw away the padlock to free up RAM
	if entry.count == 0 {
		delete(l.locks, key)
	}

	// 3. Unlock the specific key's padlock
	entry.mutex.Unlock()
}
