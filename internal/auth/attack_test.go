package auth

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// waiting is how many sign-ins are queued for a hashing slot.
func waiting(s *Service) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.waiting
}

// burst sends count wrong guesses from addr at once while both hashing slots
// are held, waits until each has either been refused or is queued for a
// slot, then frees the slots and reports how many guesses were checked and
// how many refused.
func burst(t *testing.T, s *Service, addr string, count int) (tried, refused int) {
	t.Helper()
	for range cap(s.hashing) {
		s.hashing <- struct{}{}
	}
	results := make(chan error, count)
	for range count {
		go func() { _, _, err := s.Login(context.Background(), addr, "isaac", "wrong password"); results <- err }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for waiting(s)+len(results) < count {
		if time.Now().After(deadline) {
			t.Fatalf("after 5 s, %d guesses are queued and %d answered", waiting(s), len(results))
		}
		time.Sleep(time.Millisecond)
	}
	if waiting(s) > freeFailures {
		t.Errorf("%d guesses are queued for checking, at most %d may be", waiting(s), freeFailures)
	}
	for range cap(s.hashing) {
		<-s.hashing
	}
	for range count {
		err := <-results
		var le *LockedError
		switch {
		case errors.Is(err, ErrInvalid):
			tried++
		case errors.As(err, &le):
			refused++
		default:
			t.Errorf("unexpected answer: %v", err)
		}
	}
	return tried, refused
}

// Thirty-two wrong guesses sent at once. Only as many as the address has
// free failures may be checked; the rest are refused on arrival, and once
// the checked ones have brought on the lockout, the right password has to
// wait for it like anyone else.
func TestAttackBurstOfGuessesDoesNotBeatTheLockout(t *testing.T) {
	s, c, code := newService(t)
	setUp(t, s, code)
	if tried, refused := burst(t, s, "192.0.2.1", 32); tried != freeFailures || refused != 32-freeFailures {
		t.Errorf("%d guesses checked and %d refused; want %d and %d", tried, refused, freeFailures, 32-freeFailures)
	}
	var locked *LockedError
	if _, _, err := s.Login(context.Background(), "192.0.2.1", "isaac", goodPassword); !errors.As(err, &locked) {
		t.Errorf("right after the burst: %v", err)
	}
	c.t = c.t.Add(16 * time.Minute)
	if _, _, err := s.Login(context.Background(), "192.0.2.1", "isaac", goodPassword); err != nil {
		t.Errorf("after the lockout: %v", err)
	}
}

// With four failures already counted, a burst gets one more guess checked:
// the lockout it brings on is seen by the others when their turn comes, even
// though they were admitted before it began.
func TestAttackBurstAfterEarlierFailures(t *testing.T) {
	s, _, code := newService(t)
	setUp(t, s, code)
	for range freeFailures - 1 {
		if _, _, err := s.Login(context.Background(), "192.0.2.2", "isaac", "wrong password"); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if tried, refused := burst(t, s, "192.0.2.2", 32); tried != 1 || refused != 31 {
		t.Errorf("%d guesses checked and %d refused; want 1 and 31", tried, refused)
	}
}

// Many addresses at once cannot queue up unbounded work either: beyond
// maxWaiting, a sign-in is turned away at once.
func TestAttackTooManySignInsAtOnceAreTurnedAway(t *testing.T) {
	s, _, code := newService(t)
	setUp(t, s, code)
	for range cap(s.hashing) {
		s.hashing <- struct{}{}
	}
	const count = maxWaiting + 3
	results := make(chan error, count)
	for i := range count {
		go func() {
			_, _, err := s.Login(context.Background(), fmt.Sprintf("10.9.0.%d", i), "isaac", "wrong password")
			results <- err
		}()
	}
	deadline := time.Now().Add(5 * time.Second)
	for waiting(s)+len(results) < count {
		if time.Now().After(deadline) {
			t.Fatalf("after 5 s, %d are queued and %d answered", waiting(s), len(results))
		}
		time.Sleep(time.Millisecond)
	}
	for range cap(s.hashing) {
		<-s.hashing
	}
	var busy, tried int
	for range count {
		switch err := <-results; {
		case errors.Is(err, ErrBusy):
			busy++
		case errors.Is(err, ErrInvalid):
			tried++
		default:
			t.Errorf("unexpected answer: %v", err)
		}
	}
	if busy != count-maxWaiting || tried != maxWaiting {
		t.Errorf("%d turned away and %d checked; want %d and %d", busy, tried, count-maxWaiting, maxWaiting)
	}
}

// A sign-in whose client has gone away does not stay in the queue.
func TestAttackAbandonedSignInLeavesTheQueue(t *testing.T) {
	s, _, code := newService(t)
	setUp(t, s, code)
	for range cap(s.hashing) {
		s.hashing <- struct{}{}
	}
	defer func() {
		for range cap(s.hashing) {
			<-s.hashing
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, _, err := s.Login(ctx, "10.9.1.1", "isaac", goodPassword); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want the context's error", err)
	}
	if time.Since(start) > 2*time.Second || waiting(s) != 0 {
		t.Errorf("took %v, %d still waiting", time.Since(start), waiting(s))
	}
}
