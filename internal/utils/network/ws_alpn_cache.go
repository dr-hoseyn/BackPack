package network

import (
	"sync"
	"time"
)

// Capability decisions are bounded and short lived. They never replace TLS
// authentication, and a failed handshake clears them immediately.
const wssALPNTTL = 10 * time.Minute
const wssALPNEntries = 128

var wssALPN = alpnCapabilityCache{entries: make(map[string]time.Time)}

type alpnCapabilityCache struct {
	mu      sync.Mutex
	entries map[string]time.Time
}

func (c *alpnCapabilityCache) http11(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	until, ok := c.entries[key]
	if ok && now.Before(until) {
		return true
	}
	delete(c.entries, key)
	return false
}

func (c *alpnCapabilityCache) remember(key string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[string]time.Time)
	}
	if len(c.entries) >= wssALPNEntries {
		oldestKey := ""
		var oldest time.Time
		for k, until := range c.entries {
			if oldestKey == "" || until.Before(oldest) {
				oldestKey, oldest = k, until
			}
		}
		delete(c.entries, oldestKey)
	}
	c.entries[key] = now.Add(wssALPNTTL)
}

func (c *alpnCapabilityCache) forget(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.entries, key)
}
