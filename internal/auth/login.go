package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

// ErrBusy is returned when too many sign-ins are already waiting to be
// checked. Checking a password is deliberately slow and memory-hungry, so the
// queue for it is kept short rather than letting it grow without bound.
var ErrBusy = errors.New("the server is busy checking other sign-ins; try again in a moment")

// maxWaiting bounds how many sign-ins may wait for a hashing slot at once,
// across all addresses.
const maxWaiting = 16

// Login checks a password and starts a session. The returned token is the
// session cookie's value; only its hash is stored.
//
// Attempts from one address are limited: freeFailures of them may fail, then
// each further one locks the address out for a while. An attempt counts from
// the moment it arrives, not from when its password has been checked, so a
// burst of guesses sent at once cannot slip in ahead of the count; and the
// lockout is checked again after the wait for a hashing slot, so a guess
// that queued up before the lockout began is not tried after it.
func (s *Service) Login(ctx context.Context, addr, username, password string) (token string, sess Session, err error) {
	ticket, err := s.admit(addr)
	if err != nil {
		return "", Session{}, err
	}
	result := undecided
	defer func() { ticket.settle(result) }()
	if len(password) > maxPassword {
		result = failed
		return "", Session{}, ErrInvalid
	}
	var name, stored string
	err = s.db.QueryRow(`SELECT name, pwhash FROM users WHERE name = ?`, strings.TrimSpace(username)).Scan(&name, &stored)
	missing := errors.Is(err, sql.ErrNoRows)
	if err != nil && !missing {
		return "", Session{}, err
	}
	if missing {
		// Do the same work as for a real user so timing does not reveal
		// which usernames exist.
		stored = s.dummyHash()
	}
	ok, err := s.verify(ctx, ticket, stored, password)
	if err != nil {
		return "", Session{}, err
	}
	if missing || !ok {
		result = failed
		return "", Session{}, ErrInvalid
	}
	result = succeeded

	token, csrf := randomToken(), randomToken()
	now := s.now()
	s.db.Exec(`DELETE FROM sessions WHERE created < ? OR seen < ?`, now.Add(-maxLifetime).Unix(), now.Add(-idleTimeout).Unix())
	if _, err := s.db.Exec(`INSERT INTO sessions(token, user, csrf, created, seen) VALUES(?,?,?,?,?)`,
		tokenHash(token), name, csrf, now.Unix(), now.Unix()); err != nil {
		return "", Session{}, err
	}
	return token, Session{User: name, CSRF: csrf, Created: now}, nil
}

// outcome is how an attempt ended.
type outcome int

const (
	undecided outcome = iota // an error of the server's own; not held against the address
	failed
	succeeded
)

// ticket is one admitted attempt, from arrival to its outcome.
type ticket struct {
	s        *Service
	addr     string
	a        *attempt
	checking bool // counted among the attempts being checked
}

// admit counts an attempt from addr as it arrives. It is refused while the
// address is locked out, and when as many attempts as the address has free
// failures are already in flight.
func (s *Service) admit(addr string) (*ticket, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	a := s.attemptLocked(addr, now)
	if wait := a.until.Sub(now); wait > 0 {
		return nil, &LockedError{RetryAfter: wait}
	}
	if a.inflight >= freeFailures {
		// A burst: more guesses at once than the address has free failures.
		// They are not counted; the ones admitted will be.
		return nil, &LockedError{RetryAfter: time.Second}
	}
	a.inflight++
	a.last = now
	return &ticket{s: s, addr: addr, a: a}, nil
}

// check runs once a hashing slot is held. The lockout may have begun while
// the attempt waited. And only as many attempts may be checked at once as
// the address has free failures left, with a floor of one, so that two
// slots cannot let one guess too many through before the lockout begins.
func (t *ticket) check() error {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	now := t.s.now()
	if wait := t.a.until.Sub(now); wait > 0 {
		return &LockedError{RetryAfter: wait}
	}
	if t.a.checking >= max(1, freeFailures-t.a.fails) {
		return &LockedError{RetryAfter: time.Second}
	}
	t.a.checking++
	t.checking = true
	return nil
}

// settle records how the attempt ended.
func (t *ticket) settle(o outcome) {
	t.s.mu.Lock()
	defer t.s.mu.Unlock()
	t.a.inflight--
	if t.checking {
		t.a.checking--
	}
	switch o {
	case failed:
		t.a.failLocked(t.s.now())
	case succeeded:
		if t.s.attempts[t.addr] == t.a {
			delete(t.s.attempts, t.addr)
		}
	}
}

// fail records a failed attempt that did not go through Login, such as a
// wrong setup code. The first few are free; after that each one doubles the
// lockout, up to a cap.
func (s *Service) fail(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.attemptLocked(addr, now).failLocked(now)
}

// attemptLocked returns the record for addr, fresh when there is none or the
// old one has been forgotten. The caller holds s.mu.
func (s *Service) attemptLocked(addr string, now time.Time) *attempt {
	if len(s.attempts) > 4096 {
		for k, a := range s.attempts {
			if now.Sub(a.last) > forgetAfter && a.inflight == 0 {
				delete(s.attempts, k)
			}
		}
	}
	a := s.attempts[addr]
	if a == nil || (now.Sub(a.last) > forgetAfter && a.inflight == 0) {
		a = &attempt{}
		s.attempts[addr] = a
	}
	return a
}

func (a *attempt) failLocked(now time.Time) {
	a.fails++
	a.last = now
	if over := a.fails - freeFailures; over >= 0 {
		lock := maxLockout
		if over < 10 {
			lock = min(baseLockout<<over, maxLockout)
		}
		a.until = now.Add(lock)
	}
}

// verify checks a password against a stored hash, using the cost settings
// recorded in the hash rather than the current defaults. It waits for one of
// the hashing slots, which bounds the memory a burst of sign-ins can take;
// the wait is bounded too, and gives up with ctx. With a slot in hand, the
// ticket is checked once more before the work is done.
func (s *Service) verify(ctx context.Context, t *ticket, encoded, password string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false, nil
	}
	var version int
	var p Params
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false, nil
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return false, nil
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 || p.Time == 0 || p.Threads == 0 || p.MemoryKiB > 1024*1024 {
		return false, nil
	}

	s.mu.Lock()
	if s.waiting >= maxWaiting {
		s.mu.Unlock()
		return false, ErrBusy
	}
	s.waiting++
	s.mu.Unlock()
	select {
	case s.hashing <- struct{}{}:
	case <-ctx.Done():
		s.mu.Lock()
		s.waiting--
		s.mu.Unlock()
		return false, ctx.Err()
	}
	s.mu.Lock()
	s.waiting--
	s.mu.Unlock()
	defer func() { <-s.hashing }()
	if t != nil {
		if err := t.check(); err != nil {
			return false, err
		}
	}
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1, nil
}
