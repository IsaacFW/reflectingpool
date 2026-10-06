package auth

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/appdb"
)

const goodPassword = "correct horse battery"

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newService(t *testing.T) (*Service, *clock, *string) {
	t.Helper()
	db, err := appdb.Open(filepath.Join(t.TempDir(), "app.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	s := New(db, Params{Time: 1, MemoryKiB: 1024, Threads: 1})
	c := &clock{t: time.Unix(1790000000, 0)}
	s.now = c.now
	code := new(string)
	s.OnSetupCode = func(c string) { *code = c }
	return s, c, code
}

func setUp(t *testing.T, s *Service, code *string) {
	t.Helper()
	if required, err := s.SetupRequired(); err != nil || !required {
		t.Fatalf("SetupRequired = %v, %v", required, err)
	}
	if err := s.Setup("10.0.0.1", *code, "isaac", goodPassword); err != nil {
		t.Fatal(err)
	}
}

func TestSetup(t *testing.T) {
	s, _, code := newService(t)
	if required, _ := s.SetupRequired(); !required || len(*code) != 27 {
		t.Fatalf("no setup code announced: %q", *code)
	}
	first := *code
	s.SetupRequired()
	if *code != first {
		t.Error("setup code changed between checks")
	}

	if err := s.Setup("10.0.0.9", "AAAAAA-AAAAAA-AAAAAA-AAAAAA", "mallory", goodPassword); !errors.Is(err, ErrSetupCode) {
		t.Errorf("wrong code: %v", err)
	}
	var ve ValidationError
	if err := s.Setup("10.0.0.1", *code, "isaac", "short"); !errors.As(err, &ve) {
		t.Errorf("weak password: %v", err)
	}
	// The code is accepted without dashes and in lower case.
	if err := s.Setup("10.0.0.1", normalizeCode(*code), "isaac", goodPassword); err != nil {
		t.Fatal(err)
	}
	if required, _ := s.SetupRequired(); required {
		t.Error("setup still required after it succeeded")
	}
	if err := s.Setup("10.0.0.1", first, "second", goodPassword); !errors.Is(err, ErrSetupDone) {
		t.Errorf("second setup: %v", err)
	}
}

func TestLoginAndSession(t *testing.T) {
	s, c, code := newService(t)
	setUp(t, s, code)

	if _, _, err := s.Login(context.Background(), "10.0.0.1", "isaac", "wrong password!!"); !errors.Is(err, ErrInvalid) {
		t.Errorf("wrong password: %v", err)
	}
	if _, _, err := s.Login(context.Background(), "10.0.0.1", "nobody", goodPassword); !errors.Is(err, ErrInvalid) {
		t.Errorf("unknown user: %v", err)
	}
	token, sess, err := s.Login(context.Background(), "10.0.0.1", "ISAAC", goodPassword) // usernames ignore case
	if err != nil || sess.User != "isaac" || sess.CSRF == "" || token == "" {
		t.Fatalf("login: %+v, %v", sess, err)
	}

	var stored string
	s.db.QueryRow(`SELECT token FROM sessions`).Scan(&stored)
	if stored == token || stored == "" {
		t.Error("the session token must be stored hashed")
	}
	var pw string
	s.db.QueryRow(`SELECT pwhash FROM users`).Scan(&pw)
	if len(pw) < 60 || pw[:10] != "$argon2id$" {
		t.Errorf("password hash = %q", pw)
	}

	got, err := s.Authenticate(token)
	if err != nil || got.User != "isaac" || got.CSRF != sess.CSRF {
		t.Fatalf("authenticate: %+v, %v", got, err)
	}
	if _, err := s.Authenticate("not-a-token"); !errors.Is(err, ErrNoSession) {
		t.Errorf("bad token: %v", err)
	}

	// Activity keeps a session alive, but not past its maximum lifetime.
	for range 6 {
		c.t = c.t.Add(6 * 24 * time.Hour)
		if _, err := s.Authenticate(token); err != nil && c.t.Sub(sess.Created) <= maxLifetime {
			t.Fatalf("session ended early after %s: %v", c.t.Sub(sess.Created), err)
		}
	}
	if _, err := s.Authenticate(token); !errors.Is(err, ErrNoSession) {
		t.Errorf("session outlived its maximum lifetime: %v", err)
	}

	token, _, _ = s.Login(context.Background(), "10.0.0.1", "isaac", goodPassword)
	c.t = c.t.Add(idleTimeout + time.Minute)
	if _, err := s.Authenticate(token); !errors.Is(err, ErrNoSession) {
		t.Errorf("idle session still valid: %v", err)
	}

	token, _, _ = s.Login(context.Background(), "10.0.0.1", "isaac", goodPassword)
	if err := s.Logout(token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(token); !errors.Is(err, ErrNoSession) {
		t.Errorf("session valid after logout: %v", err)
	}
}

func TestLockout(t *testing.T) {
	s, c, code := newService(t)
	setUp(t, s, code)

	for i := range freeFailures {
		if _, _, err := s.Login(context.Background(), "10.0.0.7", "isaac", "wrong password!!"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	var locked *LockedError
	if _, _, err := s.Login(context.Background(), "10.0.0.7", "isaac", goodPassword); !errors.As(err, &locked) {
		t.Fatalf("correct password during lockout: %v", err)
	}
	if locked.RetryAfter <= 0 || locked.RetryAfter > baseLockout {
		t.Errorf("first lockout = %s", locked.RetryAfter)
	}
	// Another address is unaffected.
	if _, _, err := s.Login(context.Background(), "10.0.0.8", "isaac", goodPassword); err != nil {
		t.Errorf("other address locked out: %v", err)
	}

	// Each further failure doubles the wait.
	c.t = c.t.Add(baseLockout + time.Second)
	s.Login(context.Background(), "10.0.0.7", "isaac", "wrong password!!")
	if _, _, err := s.Login(context.Background(), "10.0.0.7", "isaac", goodPassword); !errors.As(err, &locked) || locked.RetryAfter <= baseLockout {
		t.Errorf("second lockout: %v", err)
	}

	c.t = c.t.Add(maxLockout)
	if _, _, err := s.Login(context.Background(), "10.0.0.7", "isaac", goodPassword); err != nil {
		t.Errorf("after the lockout expired: %v", err)
	}
	// A success clears the count.
	for range freeFailures - 1 {
		s.Login(context.Background(), "10.0.0.7", "isaac", "wrong password!!")
	}
	if _, _, err := s.Login(context.Background(), "10.0.0.7", "isaac", goodPassword); err != nil {
		t.Errorf("failure count was not reset by a success: %v", err)
	}
}

func TestSetupCodeGuessingIsThrottled(t *testing.T) {
	s, _, _ := newService(t)
	s.SetupRequired()
	for range freeFailures {
		s.Setup("10.0.0.5", "WRONG", "a", goodPassword)
	}
	var locked *LockedError
	if err := s.Setup("10.0.0.5", "WRONG", "a", goodPassword); !errors.As(err, &locked) {
		t.Errorf("setup code guessing not throttled: %v", err)
	}
}

func TestReset(t *testing.T) {
	s, _, code := newService(t)
	setUp(t, s, code)
	token, _, _ := s.Login(context.Background(), "10.0.0.1", "isaac", goodPassword)
	old := *code
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Authenticate(token); !errors.Is(err, ErrNoSession) {
		t.Error("session survived a reset")
	}
	if required, _ := s.SetupRequired(); !required || *code == old || *code == "" {
		t.Errorf("no fresh setup code after reset: %q", *code)
	}
}

func TestVerifyRejectsMalformedHashes(t *testing.T) {
	s, _, _ := newService(t)
	for _, h := range []string{"", "plain", "$argon2id$v=19$m=1024,t=1,p=1$$", "$argon2i$v=19$m=1024,t=1,p=1$c2FsdA$aGFzaA", "$argon2id$v=19$m=99999999,t=1,p=1$c2FsdA$aGFzaA"} {
		if ok, err := s.verify(context.Background(), nil, h, "x"); ok || err != nil {
			t.Errorf("verify accepted %q", h)
		}
	}
}
