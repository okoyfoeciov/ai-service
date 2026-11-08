package server

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"

	"ai-service/internal/llm"
)

// Screenshots are transient Ask context. The extension holds only an id and a
// small thumbnail; the bytes live here until the Ask card is dismissed or the
// TTL expires. Nothing is written to disk by this store.
var (
	errScreenshotGone = errors.New("screenshot is no longer available")
	errScreenshotBig  = errors.New("screenshot is too large")
	errScreenshotFull = errors.New("screenshot store is full")
)

type storedScreenshot struct {
	data      []byte
	mediaType string
	timer     *time.Timer
	timerGen  uint64
}

type screenshotStore struct {
	mu       sync.Mutex
	items    map[string]*storedScreenshot
	total    int64
	ttl      time.Duration
	maxBytes int64
	maxShots int
}

func newScreenshotStore(ttl time.Duration, maxBytes int64, maxShots int) *screenshotStore {
	return &screenshotStore{
		items:    make(map[string]*storedScreenshot),
		ttl:      ttl,
		maxBytes: maxBytes,
		maxShots: maxShots,
	}
}

// put stores one screenshot and returns its unguessable id.
func (s *screenshotStore) put(data []byte, mediaType string) (string, error) {
	if s == nil {
		return "", errors.New("screenshot store is unavailable")
	}
	if int64(len(data)) > s.maxBytes || s.maxShots <= 0 {
		return "", errScreenshotBig
	}
	id, err := newScreenshotID()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	// Never evict a live entry: the client keeps every round's ids and a
	// silently deleted image would break the whole open Ask card. Refuse the
	// new capture instead; normal releases keep the store far below these caps.
	if len(s.items) >= s.maxShots || s.total+int64(len(data)) > s.maxBytes {
		return "", errScreenshotFull
	}
	entry := &storedScreenshot{
		data:      data,
		mediaType: mediaType,
	}
	s.items[id] = entry
	s.total += int64(len(data))
	s.armTimerLocked(id, entry)
	return id, nil
}

// getMany returns the images in the order of ids. It refreshes each entry's
// idle TTL, because a long-lived Ask card is still using them.
func (s *screenshotStore) getMany(ids []string) ([]llm.Image, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	if s == nil {
		return nil, errScreenshotGone
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.Image, 0, len(ids))
	for _, id := range ids {
		entry, ok := s.items[id]
		if !ok {
			return nil, fmt.Errorf("%w: %s", errScreenshotGone, id)
		}
		s.armTimerLocked(id, entry)
		out = append(out, llm.Image{
			MediaType: entry.mediaType,
			Data:      base64.StdEncoding.EncodeToString(entry.data),
		})
	}
	return out, nil
}

// release deletes the named screenshots. It is idempotent: the card close and
// a failed stop can both race here without hurting anything.
func (s *screenshotStore) release(ids []string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range ids {
		s.dropLocked(id)
	}
}

func (s *screenshotStore) dropLocked(id string) {
	entry, ok := s.items[id]
	if !ok {
		return
	}
	if entry.timer != nil {
		entry.timer.Stop()
	}
	delete(s.items, id)
	s.total -= int64(len(entry.data))
}

// armTimerLocked replaces the entry's idle timer. The caller holds s.mu.
// The generation matters: Stop() loses against a callback that has already
// fired and is waiting for the lock, and that old callback must not delete an
// entry whose TTL was just refreshed.
func (s *screenshotStore) armTimerLocked(id string, entry *storedScreenshot) {
	if entry.timer != nil {
		entry.timer.Stop()
	}
	entry.timerGen++
	gen := entry.timerGen
	entry.timer = time.AfterFunc(s.ttl, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if current, ok := s.items[id]; ok && current == entry && entry.timerGen == gen {
			s.dropLocked(id)
		}
	})
}

func newScreenshotID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
