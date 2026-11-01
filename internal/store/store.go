// Package store persists users, devices, subscriptions and episode actions in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, no CGO
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

var (
	// '@' allows email-style names, common with Authelia/LDAP.
	usernameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._@+-]{0,127}$`)
	deviceRE   = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)
)

// ValidUsername reports whether s is an acceptable username.
func ValidUsername(s string) bool { return usernameRE.MatchString(s) }

// ValidDeviceID reports whether s is an acceptable device ID.
func ValidDeviceID(s string) bool { return deviceRE.MatchString(s) }

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) and migrates the database at path.
func Open(path string) (*Store, error) {
	q := url.Values{}
	for _, p := range []string{"foreign_keys(1)", "journal_mode(WAL)", "busy_timeout(5000)", "synchronous(NORMAL)"} {
		q.Add("_pragma", p)
	}
	q.Set("_txlock", "immediate")
	db, err := sql.Open("sqlite", "file:"+path+"?"+q.Encode())
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection keeps things
	// simple and is plenty for a personal sync server.
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate %s: %w", path, err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Ping checks the database is reachable.
func (s *Store) Ping(ctx context.Context) error { return s.db.PingContext(ctx) }

var migrations = []string{
	`CREATE TABLE users (
		id            INTEGER PRIMARY KEY,
		username      TEXT    NOT NULL UNIQUE,
		created_at    INTEGER NOT NULL
	);
	CREATE TABLE devices (
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		device_id  TEXT    NOT NULL,
		caption    TEXT    NOT NULL DEFAULT '',
		type       TEXT    NOT NULL DEFAULT 'other',
		updated_at INTEGER NOT NULL,
		PRIMARY KEY (user_id, device_id)
	);
	CREATE TABLE subscriptions (
		user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		url        TEXT    NOT NULL,
		subscribed INTEGER NOT NULL,
		changed_at INTEGER NOT NULL,
		device_id  TEXT    NOT NULL DEFAULT '',
		PRIMARY KEY (user_id, url)
	);
	CREATE INDEX subscriptions_changed ON subscriptions (user_id, changed_at);
	CREATE TABLE episode_actions (
		id          INTEGER PRIMARY KEY,
		user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
		podcast     TEXT    NOT NULL,
		episode     TEXT    NOT NULL,
		guid        TEXT,
		device      TEXT,
		action      TEXT    NOT NULL,
		timestamp   TEXT    NOT NULL, -- UTC, 2006-01-02T15:04:05 (sorts chronologically)
		started     INTEGER,
		position    INTEGER,
		total       INTEGER,
		received_at INTEGER NOT NULL
	);
	CREATE INDEX episode_actions_received ON episode_actions (user_id, received_at);
	CREATE INDEX episode_actions_episode  ON episode_actions (user_id, podcast, episode);`,
}

func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this binary supports (%d)", version, len(migrations))
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// ---- users -----------------------------------------------------------------

// Users have no credentials here: the reverse proxy authenticates them, and
// a row is created the first time the proxy forwards a request for a name.

// EnsureUser returns the ID of username, creating the user if needed.
func (s *Store) EnsureUser(ctx context.Context, username string, now time.Time) (int64, error) {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, created_at) VALUES (?, ?)
		ON CONFLICT (username) DO NOTHING`, username, now.Unix()); err != nil {
		return 0, err
	}
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE username = ?`, username).Scan(&id)
	return id, err
}

// DeleteUser removes a user and all of their data.
func (s *Store) DeleteUser(ctx context.Context, username string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE username = ?`, username)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListUsers returns all usernames.
func (s *Store) ListUsers(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT username FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		names = append(names, n)
	}
	return names, rows.Err()
}

// ---- devices ---------------------------------------------------------------

// Device is a client device as exposed by the gpodder Device API.
type Device struct {
	ID            string `json:"id"`
	Caption       string `json:"caption"`
	Type          string `json:"type"`
	Subscriptions int    `json:"subscriptions"`
}

// ListDevices returns a user's devices. Subscriptions are shared by all of a
// user's devices, so every device reports the same subscription count.
func (s *Store) ListDevices(ctx context.Context, userID int64) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT device_id, caption, type,
		       (SELECT COUNT(*) FROM subscriptions WHERE user_id = d.user_id AND subscribed = 1)
		FROM devices d WHERE user_id = ? ORDER BY device_id`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	devices := []Device{}
	for rows.Next() {
		var d Device
		if err := rows.Scan(&d.ID, &d.Caption, &d.Type, &d.Subscriptions); err != nil {
			return nil, err
		}
		devices = append(devices, d)
	}
	return devices, rows.Err()
}

// UpdateDevice creates or updates a device. Nil fields are left unchanged.
func (s *Store) UpdateDevice(ctx context.Context, userID int64, deviceID string, caption, typ *string, now time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO devices (user_id, device_id, caption, type, updated_at)
		VALUES (?1, ?2, COALESCE(?3, ''), COALESCE(?4, 'other'), ?5)
		ON CONFLICT (user_id, device_id) DO UPDATE SET
			caption    = COALESCE(?3, caption),
			type       = COALESCE(?4, type),
			updated_at = ?5`,
		userID, deviceID, caption, typ, now.Unix())
	return err
}

func ensureDevice(ctx context.Context, tx *sql.Tx, userID int64, deviceID string, now time.Time) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO devices (user_id, device_id, updated_at) VALUES (?, ?, ?)
		ON CONFLICT (user_id, device_id) DO NOTHING`, userID, deviceID, now.Unix())
	return err
}

// ---- subscriptions ---------------------------------------------------------

// SubscriptionChanges returns URLs added and removed at or after since (unix
// seconds). Subscriptions are per user, shared across all devices. With
// since == 0 only current subscriptions are returned.
func (s *Store) SubscriptionChanges(ctx context.Context, userID, since int64) (add, remove []string, err error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT url, subscribed FROM subscriptions
		WHERE user_id = ? AND changed_at >= ? ORDER BY url`, userID, since)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	add, remove = []string{}, []string{}
	for rows.Next() {
		var u string
		var subscribed bool
		if err := rows.Scan(&u, &subscribed); err != nil {
			return nil, nil, err
		}
		switch {
		case subscribed:
			add = append(add, u)
		case since > 0:
			remove = append(remove, u)
		}
	}
	return add, remove, rows.Err()
}

// UpdateSubscriptions applies subscription changes uploaded by a device.
func (s *Store) UpdateSubscriptions(ctx context.Context, userID int64, deviceID string, add, remove []string, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		if err := ensureDevice(ctx, tx, userID, deviceID, now); err != nil {
			return err
		}
		for _, u := range add {
			// Re-adding an existing subscription is a no-op and must not bump changed_at.
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO subscriptions (user_id, url, subscribed, changed_at, device_id) VALUES (?1, ?2, 1, ?3, ?4)
				ON CONFLICT (user_id, url) DO UPDATE SET subscribed = 1, changed_at = ?3, device_id = ?4
				WHERE subscribed = 0`, userID, u, now.Unix(), deviceID); err != nil {
				return err
			}
		}
		for _, u := range remove {
			if _, err := tx.ExecContext(ctx, `
				UPDATE subscriptions SET subscribed = 0, changed_at = ?, device_id = ?
				WHERE user_id = ? AND url = ? AND subscribed = 1`, now.Unix(), deviceID, userID, u); err != nil {
				return err
			}
		}
		return nil
	})
}

// ---- episode actions -------------------------------------------------------

// EpisodeAction is a gpodder episode action.
type EpisodeAction struct {
	Podcast   string `json:"podcast"`
	Episode   string `json:"episode"`
	GUID      string `json:"guid,omitempty"`
	Device    string `json:"device,omitempty"`
	Action    string `json:"action"`
	Timestamp string `json:"timestamp"`
	Started   *int64 `json:"started,omitempty"`
	Position  *int64 `json:"position,omitempty"`
	Total     *int64 `json:"total,omitempty"`
}

// AddEpisodeActions stores already-validated episode actions.
func (s *Store) AddEpisodeActions(ctx context.Context, userID int64, actions []EpisodeAction, now time.Time) error {
	return s.withTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `
			INSERT INTO episode_actions
				(user_id, podcast, episode, guid, device, action, timestamp, started, position, total, received_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, a := range actions {
			if a.Device != "" {
				if err := ensureDevice(ctx, tx, userID, a.Device, now); err != nil {
					return err
				}
			}
			if _, err := stmt.ExecContext(ctx, userID, a.Podcast, a.Episode, nullString(a.GUID), nullString(a.Device),
				a.Action, a.Timestamp, a.Started, a.Position, a.Total, now.Unix()); err != nil {
				return err
			}
		}
		return nil
	})
}

// EpisodeFilter selects episode actions.
type EpisodeFilter struct {
	Since      int64  // unix seconds; actions received at or after this time
	Podcast    string // optional feed URL
	Device     string // optional device ID
	Aggregated bool   // only the latest action per episode
}

// EpisodeActions returns matching actions ordered by timestamp.
func (s *Store) EpisodeActions(ctx context.Context, userID int64, f EpisodeFilter) ([]EpisodeAction, error) {
	where := []string{"user_id = ?", "received_at >= ?"}
	args := []any{userID, f.Since}
	if f.Podcast != "" {
		where = append(where, "podcast = ?")
		args = append(args, f.Podcast)
	}
	if f.Device != "" {
		where = append(where, "device = ?")
		args = append(args, f.Device)
	}
	const cols = "podcast, episode, guid, device, action, timestamp, started, position, total"
	var query string
	if f.Aggregated {
		query = `SELECT ` + cols + ` FROM (
			SELECT *, ROW_NUMBER() OVER (PARTITION BY podcast, episode ORDER BY timestamp DESC, id DESC) AS rn
			FROM episode_actions WHERE ` + strings.Join(where, " AND ") + `
		) WHERE rn = 1 ORDER BY timestamp, id`
	} else {
		query = `SELECT ` + cols + ` FROM episode_actions WHERE ` + strings.Join(where, " AND ") + ` ORDER BY timestamp, id`
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	actions := []EpisodeAction{}
	for rows.Next() {
		var a EpisodeAction
		var guid, device sql.NullString
		var started, position, total sql.NullInt64
		if err := rows.Scan(&a.Podcast, &a.Episode, &guid, &device, &a.Action, &a.Timestamp,
			&started, &position, &total); err != nil {
			return nil, err
		}
		a.GUID, a.Device = guid.String, device.String
		a.Started, a.Position, a.Total = intPtr(started), intPtr(position), intPtr(total)
		actions = append(actions, a)
	}
	return actions, rows.Err()
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func intPtr(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	return &n.Int64
}
