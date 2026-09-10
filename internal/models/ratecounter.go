package models

import "time"

// RateCounter is one fixed window of a rate-limited key, kept in the database
// rather than in a process.
//
// In memory the counters are per-process and vanish on restart, which means an
// upgrade or a crash hands an attacker a fresh budget, and two apiserver
// replicas each grant the full one. Neither is visible from the outside: the
// limit simply is not the limit.
type RateCounter struct {
	// Key is the limiter's own key, prefixed by which limiter it belongs to so
	// a username and an IP cannot collide.
	Key     string    `gorm:"primaryKey;size:190"`
	Count   int       `json:"count"`
	ResetAt time.Time `gorm:"index" json:"resetAt"`
}
