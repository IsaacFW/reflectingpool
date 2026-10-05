package auth

import (
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Assert vulnerable behavior using cheap test hashes and a blocked hash gate.
func TestAuditConcurrentLoginBypassesLockout(t *testing.T) {
	s, _, code := newService(t)
	setUp(t, s, code)
	for range cap(s.hashing) {
		s.hashing <- struct{}{}
	}
	const count = 32
	results := make(chan error, count)
	for range count {
		go func() { _, _, err := s.Login("192.0.2.1", "isaac", "wrong password"); results <- err }()
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		buf := make([]byte, 1<<20)
		n := runtime.Stack(buf, true)
		if strings.Count(string(buf[:n]), "auth.(*Service).verify(") >= count {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("login workers did not reach hashing gate")
		}
		time.Sleep(time.Millisecond)
	}
	for range cap(s.hashing) {
		<-s.hashing
	}
	for range count {
		if err := <-results; !errors.Is(err, ErrInvalid) {
			t.Fatalf("expected every queued guess evaluated; got %v", err)
		}
	}
	if err := s.locked("192.0.2.1"); err == nil {
		t.Fatal("lockout should now be active")
	}
	t.Logf("confirmed all %d queued guesses evaluated from one address despite %d-failure lockout", count, freeFailures)
}
