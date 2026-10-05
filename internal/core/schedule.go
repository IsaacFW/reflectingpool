package core

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/IsaacFW/reflectingpool/internal/scan"
)

// Schedule is the optional daily scan. It is off unless the user turns it on:
// the server never scans by itself otherwise.
type Schedule struct {
	Enabled bool `json:"enabled"`
	// Time is the hour and minute to start, "HH:MM", in the server's time zone.
	Time string `json:"time"`
	// Intensity is "aggressive", "balanced" or "low".
	Intensity string `json:"intensity"`
}

// Settings are the choices the user makes in the interface that the server
// has to act on.
type Settings struct {
	Schedule Schedule `json:"scan_schedule"`
}

const (
	keySettings      = "settings"
	keyLastChoice    = "last_choice"
	keyLastScheduled = "last_scheduled"

	// A scheduled scan that could not start on time, because the server was
	// off or busy, is skipped once it is this late. Starting it hours later
	// would put the load exactly where the user chose not to have it.
	scheduleGrace = time.Hour
)

func defaultSettings() Settings {
	return Settings{Schedule: Schedule{Enabled: false, Time: "03:00", Intensity: scan.LowImpact.String()}}
}

func (a *App) setting(key string) string {
	var v string
	a.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	return v
}

func (a *App) setSetting(key, value string) error {
	_, err := a.db.Exec(`INSERT INTO settings(key, value) VALUES(?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

func (a *App) Settings() (Settings, error) {
	s := defaultSettings()
	var blob string
	err := a.db.QueryRow(`SELECT value FROM settings WHERE key = ?`, keySettings).Scan(&blob)
	if errors.Is(err, sql.ErrNoRows) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal([]byte(blob), &s); err != nil {
		return defaultSettings(), nil // unreadable settings fall back to the defaults
	}
	return s, nil
}

func (a *App) SaveSettings(s Settings) error {
	if _, err := time.Parse("15:04", s.Schedule.Time); err != nil {
		return InputError("the scan time must look like 03:00")
	}
	if _, err := scan.ParseIntensity(s.Schedule.Intensity); err != nil {
		return InputError(err.Error())
	}
	blob, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return a.setSetting(keySettings, string(blob))
}

// at returns the schedule's start time on the day of now.
func (s Schedule) at(now time.Time) (time.Time, bool) {
	t, err := time.Parse("15:04", s.Time)
	if err != nil {
		return time.Time{}, false
	}
	return time.Date(now.Year(), now.Month(), now.Day(), t.Hour(), t.Minute(), 0, 0, now.Location()), true
}

// due reports whether a scheduled scan should start now, given the day the
// last one started ("2006-01-02", or "" if never).
func (s Schedule) due(now time.Time, lastDay string) bool {
	if !s.Enabled {
		return false
	}
	start, ok := s.at(now)
	if !ok || now.Before(start) || now.Sub(start) >= scheduleGrace {
		return false
	}
	return lastDay != now.Format("2006-01-02")
}

// next returns when the next scheduled scan will start, or nil when the
// schedule is off.
func (s Schedule) next(now time.Time) *time.Time {
	if !s.Enabled {
		return nil
	}
	start, ok := s.at(now)
	if !ok {
		return nil
	}
	if !now.Before(start) {
		start = start.AddDate(0, 0, 1)
	}
	return &start
}

// RunScheduler starts scheduled scans and picks up indexes built by another
// process. It returns when ctx ends.
func (a *App) RunScheduler(ctx context.Context) {
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-tick.C:
			a.adoptLatest()
			settings, err := a.Settings()
			if err != nil || !settings.Schedule.due(now, a.setting(keyLastScheduled)) {
				continue
			}
			// Recorded before the scan, so a scan that fails or is stopped
			// is not retried every tick for the rest of the hour.
			a.setSetting(keyLastScheduled, now.Format("2006-01-02"))
			intensity, _ := scan.ParseIntensity(settings.Schedule.Intensity)
			if _, err := a.scanAs(ctx, intensity, "scheduled"); errors.Is(err, ErrScanRunning) {
				log.Printf("scheduled scan skipped: a scan was already running")
			}
		}
	}
}

// ScanRecord is one finished scan.
type ScanRecord struct {
	IndexID   string    `json:"index_id"`
	Started   time.Time `json:"started"`
	Intensity string    `json:"intensity"`
	Trigger   string    `json:"trigger"` // "manual" or "scheduled"
	Seconds   float64   `json:"seconds"`
	Entries   int64     `json:"entries"`
	Errors    int64     `json:"errors"`
}

func (a *App) recordScan(r ScanRecord) {
	_, err := a.db.Exec(`INSERT INTO scan_history(index_id, started, intensity, trigger, seconds, entries, errors) VALUES(?,?,?,?,?,?,?)`,
		r.IndexID, r.Started.Unix(), r.Intensity, r.Trigger, r.Seconds, r.Entries, r.Errors)
	if err != nil {
		log.Printf("the scan was not added to the history: %v", err)
		return
	}
	a.db.Exec(`DELETE FROM scan_history WHERE id NOT IN (SELECT id FROM scan_history ORDER BY id DESC LIMIT 200)`)
}

func (a *App) scanHistory(limit int) ([]ScanRecord, error) {
	rows, err := a.db.Query(`SELECT index_id, started, intensity, trigger, seconds, entries, errors FROM scan_history ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ScanRecord{}
	for rows.Next() {
		var r ScanRecord
		var started int64
		if err := rows.Scan(&r.IndexID, &started, &r.Intensity, &r.Trigger, &r.Seconds, &r.Entries, &r.Errors); err != nil {
			return nil, err
		}
		r.Started = time.Unix(started, 0)
		out = append(out, r)
	}
	return out, rows.Err()
}
