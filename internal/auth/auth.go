// Package auth implements the single admin account: first-run setup, password
// login, sessions and throttling of failed attempts.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"golang.org/x/crypto/argon2"
)

var (
	ErrInvalid   = errors.New("invalid username or password")
	ErrSetupDone = errors.New("setup is already complete")
	ErrSetupCode = errors.New("invalid setup code")
	ErrNoSession = errors.New("not signed in")
)

// LockedError is returned while an address is locked out after repeated failures.
type LockedError struct{ RetryAfter time.Duration }

func (e *LockedError) Error() string {
	return fmt.Sprintf("too many failed attempts; try again in %s", e.RetryAfter.Round(time.Second))
}

// ValidationError describes a username or password that cannot be accepted.
type ValidationError string

func (e ValidationError) Error() string { return string(e) }

// Params are the Argon2id cost settings.
type Params struct {
	Time      uint32
	MemoryKiB uint32
	Threads   uint8
}

// DefaultParams costs about 64 MiB and a fraction of a second per attempt.
var DefaultParams = Params{Time: 3, MemoryKiB: 64 * 1024, Threads: 2}

const (
	MinPassword = 12
	maxPassword = 256 // bounds the work an unauthenticated request can demand
	maxUsername = 64

	idleTimeout = 7 * 24 * time.Hour
	maxLifetime = 30 * 24 * time.Hour

	freeFailures = 5
	baseLockout  = 30 * time.Second
	maxLockout   = 15 * time.Minute
	forgetAfter  = time.Hour
)

type Session struct {
	User    string
	CSRF    string
	Created time.Time
}

type attempt struct {
	fails int
	until time.Time
	last  time.Time
}

type Service struct {
	db     *sql.DB
	params Params
	// OnSetupCode is called whenever a new setup code is generated, so the
	// caller can print it where only the server's owner can read it.
	OnSetupCode func(code string)

	mu        sync.Mutex
	setupCode string
	attempts  map[string]*attempt
	dummy     string

	// Hashing is memory-hard; without a cap a burst of logins could exhaust
	// the container's memory.
	hashing chan struct{}
	now     func() time.Time
}

func New(db *sql.DB, p Params) *Service {
	return &Service{
		db: db, params: p,
		attempts: make(map[string]*attempt),
		hashing:  make(chan struct{}, 2),
		now:      time.Now,
	}
}

// SetupRequired reports whether the admin account has yet to be created,
// generating and announcing a setup code the first time it is true.
func (s *Service) SetupRequired() (bool, error) {
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if n > 0 {
		s.setupCode = ""
		return false, nil
	}
	if s.setupCode == "" {
		raw := make([]byte, 15)
		rand.Read(raw)
		enc := base32.StdEncoding.EncodeToString(raw) // 24 characters, 120 bits
		s.setupCode = enc[:6] + "-" + enc[6:12] + "-" + enc[12:18] + "-" + enc[18:]
		if s.OnSetupCode != nil {
			s.OnSetupCode(s.setupCode)
		}
	}
	return true, nil
}

func normalizeCode(code string) string {
	return strings.ToUpper(strings.NewReplacer("-", "", " ", "").Replace(code))
}

// Setup creates the admin account. It works once, and only with the setup code.
func (s *Service) Setup(addr, code, username, password string) error {
	if err := s.locked(addr); err != nil {
		return err
	}
	required, err := s.SetupRequired()
	if err != nil {
		return err
	}
	if !required {
		return ErrSetupDone
	}
	s.mu.Lock()
	want := s.setupCode
	s.mu.Unlock()
	if subtle.ConstantTimeCompare([]byte(normalizeCode(code)), []byte(normalizeCode(want))) != 1 {
		s.fail(addr)
		return ErrSetupCode
	}
	username = strings.TrimSpace(username)
	if err := validate(username, password); err != nil {
		return err
	}
	hash := s.hash(password)
	res, err := s.db.Exec(`INSERT INTO users(name, pwhash, created) SELECT ?, ?, ? WHERE NOT EXISTS (SELECT 1 FROM users)`,
		username, hash, s.now().Unix())
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrSetupDone
	}
	s.mu.Lock()
	s.setupCode = ""
	s.mu.Unlock()
	return nil
}

func validate(username, password string) error {
	if username == "" || len(username) > maxUsername {
		return ValidationError(fmt.Sprintf("username must be 1 to %d characters", maxUsername))
	}
	for _, r := range username {
		if !unicode.IsPrint(r) {
			return ValidationError("username contains unprintable characters")
		}
	}
	if len(password) < MinPassword {
		return ValidationError(fmt.Sprintf("password must be at least %d characters", MinPassword))
	}
	if len(password) > maxPassword {
		return ValidationError(fmt.Sprintf("password must be at most %d characters", maxPassword))
	}
	return nil
}

// Login checks a password and starts a session. The returned token is the
// session cookie's value; only its hash is stored.
func (s *Service) Login(addr, username, password string) (token string, sess Session, err error) {
	if err := s.locked(addr); err != nil {
		return "", Session{}, err
	}
	if len(password) > maxPassword {
		s.fail(addr)
		return "", Session{}, ErrInvalid
	}
	var name, stored string
	err = s.db.QueryRow(`SELECT name, pwhash FROM users WHERE name = ?`, strings.TrimSpace(username)).Scan(&name, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		// Do the same work as for a real user so timing does not reveal
		// which usernames exist.
		s.verify(s.dummyHash(), password)
		s.fail(addr)
		return "", Session{}, ErrInvalid
	}
	if err != nil {
		return "", Session{}, err
	}
	if !s.verify(stored, password) {
		s.fail(addr)
		return "", Session{}, ErrInvalid
	}
	s.mu.Lock()
	delete(s.attempts, addr)
	s.mu.Unlock()

	token, csrf := randomToken(), randomToken()
	now := s.now()
	s.db.Exec(`DELETE FROM sessions WHERE created < ? OR seen < ?`, now.Add(-maxLifetime).Unix(), now.Add(-idleTimeout).Unix())
	if _, err := s.db.Exec(`INSERT INTO sessions(token, user, csrf, created, seen) VALUES(?,?,?,?,?)`,
		tokenHash(token), name, csrf, now.Unix(), now.Unix()); err != nil {
		return "", Session{}, err
	}
	return token, Session{User: name, CSRF: csrf, Created: now}, nil
}

// Authenticate returns the session for a cookie value.
func (s *Service) Authenticate(token string) (Session, error) {
	if token == "" {
		return Session{}, ErrNoSession
	}
	key := tokenHash(token)
	var sess Session
	var created, seen int64
	err := s.db.QueryRow(`SELECT user, csrf, created, seen FROM sessions WHERE token = ?`, key).Scan(&sess.User, &sess.CSRF, &created, &seen)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNoSession
	}
	if err != nil {
		return Session{}, err
	}
	now := s.now()
	sess.Created = time.Unix(created, 0)
	if now.Sub(sess.Created) > maxLifetime || now.Sub(time.Unix(seen, 0)) > idleTimeout {
		s.db.Exec(`DELETE FROM sessions WHERE token = ?`, key)
		return Session{}, ErrNoSession
	}
	if now.Unix()-seen > 60 {
		s.db.Exec(`UPDATE sessions SET seen = ? WHERE token = ?`, now.Unix(), key)
	}
	return sess, nil
}

func (s *Service) Logout(token string) error {
	_, err := s.db.Exec(`DELETE FROM sessions WHERE token = ?`, tokenHash(token))
	return err
}

// Reset removes the admin account and every session, so the next start goes
// through setup again. It is the recovery path for a forgotten password and
// is only reachable from the command line.
func (s *Service) Reset() error {
	if _, err := s.db.Exec(`DELETE FROM sessions`); err != nil {
		return err
	}
	_, err := s.db.Exec(`DELETE FROM users`)
	return err
}

func (s *Service) locked(addr string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if a := s.attempts[addr]; a != nil {
		if wait := a.until.Sub(s.now()); wait > 0 {
			return &LockedError{RetryAfter: wait}
		}
	}
	return nil
}

// fail records a failed attempt. The first few are free; after that each one
// doubles the lockout, up to a cap.
func (s *Service) fail(addr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	if len(s.attempts) > 4096 {
		for k, a := range s.attempts {
			if now.Sub(a.last) > forgetAfter {
				delete(s.attempts, k)
			}
		}
	}
	a := s.attempts[addr]
	if a == nil || now.Sub(a.last) > forgetAfter {
		a = &attempt{}
		s.attempts[addr] = a
	}
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

func (s *Service) dummyHash() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dummy == "" {
		s.dummy = s.hashLocked(randomToken())
	}
	return s.dummy
}

func (s *Service) hash(password string) string {
	s.hashing <- struct{}{}
	defer func() { <-s.hashing }()
	return s.hashLocked(password)
}

func (s *Service) hashLocked(password string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	p := s.params
	key := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, 32)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, p.MemoryKiB, p.Time, p.Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key))
}

// verify checks a password against a stored hash, using the cost settings
// recorded in the hash rather than the current defaults.
func (s *Service) verify(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var version int
	var p Params
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil || version != argon2.Version {
		return false
	}
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.MemoryKiB, &p.Time, &p.Threads); err != nil {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[4])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[5])
	if err1 != nil || err2 != nil || len(want) == 0 || p.Time == 0 || p.Threads == 0 || p.MemoryKiB > 1024*1024 {
		return false
	}
	s.hashing <- struct{}{}
	defer func() { <-s.hashing }()
	got := argon2.IDKey([]byte(password), salt, p.Time, p.MemoryKiB, p.Threads, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func randomToken() string {
	raw := make([]byte, 32)
	rand.Read(raw)
	return base64.RawURLEncoding.EncodeToString(raw)
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
