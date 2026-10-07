package auth

import (
	"crypto/rand"
	"errors"
	"sync"
	"testing"
)

// Stop hash work at its entropy dependency, before allocating Argon2 memory.
// Each attempt reports either admission to that dependency or an early refusal.
type blockedEntropy struct {
	events  chan bool
	release chan struct{}
}

func (b *blockedEntropy) Read(p []byte) (int, error) {
	b.events <- true
	<-b.release
	clear(p)
	return len(p), nil
}

func TestPasswordHashAdmissionBounded(t *testing.T) {
	b := &blockedEntropy{events: make(chan bool, 3), release: make(chan struct{})}
	original := rand.Reader
	rand.Reader = b
	defer func() { rand.Reader = original }()
	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := HashPassword("password")
			if err != nil {
				b.events <- false
			}
		}()
	}
	admitted := 0
	for range 3 {
		if <-b.events {
			admitted++
		}
	}
	close(b.release)
	wg.Wait()
	if admitted > 2 {
		t.Fatalf("%d simultaneous password computations admitted, want at most 2", admitted)
	}
}

func TestPasswordAdmissionSharedByEveryOperation(t *testing.T) {
	for range cap(passwordSlots) {
		passwordSlots <- struct{}{}
	}
	if _, err := HashPassword("password"); !errors.Is(err, ErrBusy) {
		t.Errorf("hash admission: %v", err)
	}
	if _, err := VerifyPassword(decoyHash, "password"); !errors.Is(err, ErrBusy) {
		t.Errorf("verify admission: %v", err)
	}
	if err := SpendVerifyBudget("password"); !errors.Is(err, ErrBusy) {
		t.Errorf("unknown account admission: %v", err)
	}
	for range cap(passwordSlots) {
		<-passwordSlots
	}
	if err := SpendVerifyBudget("password"); err != nil {
		t.Errorf("admission after release: %v", err)
	}
}
