// Package store persists messages and agents in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver: keeps CGO_ENABLED=0 builds
)

// Message is one line on the bus.
type Message struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	Channel string    `json:"channel"`
	From    string    `json:"from"`
	To      string    `json:"to,omitempty"`
	Body    string    `json:"body"`
	Tags    []string  `json:"tags,omitempty"`
}

// Agent is a driven CLI session bound to one channel.
type Agent struct {
	Nick      string    `json:"nick"`
	Provider  string    `json:"provider"`
	Channel   string    `json:"channel"`
	SessionID string    `json:"session_id"`
	Cwd       string    `json:"cwd"`
	Sandbox   string    `json:"sandbox"`
	Tools     []string  `json:"tools"`
	Token     string    `json:"-"`
	Enabled   bool      `json:"enabled"`
	LastMsgID int64     `json:"last_msg_id"`
	Turns     int       `json:"turns"`
	CreatedAt time.Time `json:"created_at"`
}

// Channel summarises one channel for listings.
type Channel struct {
	Name   string `json:"name"`
	Topic  string `json:"topic"`
	LastID int64  `json:"last_id"`
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite handle.
type Store struct{ db *sql.DB }

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts TEXT NOT NULL,
	channel TEXT NOT NULL,
	sender TEXT NOT NULL,
	recipient TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL,
	tags TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS messages_channel_id ON messages(channel, id);
CREATE TABLE IF NOT EXISTS agents (
	nick TEXT PRIMARY KEY,
	provider TEXT NOT NULL,
	channel TEXT NOT NULL,
	session_id TEXT NOT NULL DEFAULT '',
	cwd TEXT NOT NULL,
	sandbox TEXT NOT NULL,
	tools TEXT NOT NULL DEFAULT '',
	token TEXT NOT NULL UNIQUE,
	enabled INTEGER NOT NULL DEFAULT 1,
	last_msg_id INTEGER NOT NULL DEFAULT 0,
	turns INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);`

// Open opens (and migrates) the database at path. ":memory:" works for tests.
func Open(path string) (*Store, error) {
	// busy_timeout: the driver and HTTP handlers write concurrently; WAL keeps readers unblocked.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)", path)
	if path == ":memory:" {
		dsn = "file::memory:?cache=shared"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// A single connection serialises writes; SQLite has no row-level locking anyway.
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(context.Background(), schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert stores m and fills its ID and TS.
func (s *Store) Insert(ctx context.Context, m *Message) error {
	m.TS = time.Now().UTC()
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(ts, channel, sender, recipient, body, tags) VALUES(?,?,?,?,?,?)`,
		m.TS.Format(time.RFC3339Nano), m.Channel, m.From, m.To, m.Body, strings.Join(m.Tags, ","))
	if err != nil {
		return fmt.Errorf("insert message: %w", err)
	}
	m.ID, err = res.LastInsertId()
	if err != nil {
		return fmt.Errorf("insert message id: %w", err)
	}
	return nil
}

// Messages returns messages of channel with afterID < id < beforeID (beforeID 0 = no bound),
// oldest first, at most limit rows (limit 0 = 1000). Empty channel = all channels.
func (s *Store) Messages(ctx context.Context, channel string, afterID, beforeID int64, limit int) ([]Message, error) {
	if limit <= 0 {
		limit = 1000
	}
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	q := `SELECT id, ts, channel, sender, recipient, body, tags FROM messages WHERE id > ? AND id < ?`
	args := []any{afterID, beforeID}
	if channel != "" {
		q += ` AND channel = ?`
		args = append(args, channel)
	}
	q += ` ORDER BY id ASC LIMIT ?`
	args = append(args, limit)
	return s.queryMessages(ctx, q, args...)
}

// Tail returns the last n messages of channel, oldest first.
func (s *Store) Tail(ctx context.Context, channel string, n int) ([]Message, error) {
	msgs, err := s.queryMessages(ctx,
		`SELECT id, ts, channel, sender, recipient, body, tags FROM messages WHERE channel = ? ORDER BY id DESC LIMIT ?`,
		channel, n)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
}

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("query messages: %w", err)
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		var m Message
		var ts, tags string
		if err := rows.Scan(&m.ID, &ts, &m.Channel, &m.From, &m.To, &m.Body, &tags); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		m.TS, _ = time.Parse(time.RFC3339Nano, ts)
		m.Tags = splitTags(tags)
		out = append(out, m)
	}
	return out, rows.Err()
}

// Channels lists channels with their topic (first message body, first line).
func (s *Store) Channels(ctx context.Context) ([]Channel, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.channel, MAX(m.id), (SELECT body FROM messages f WHERE f.channel = m.channel ORDER BY id ASC LIMIT 1)
		FROM messages m GROUP BY m.channel ORDER BY MAX(m.id) DESC`)
	if err != nil {
		return nil, fmt.Errorf("query channels: %w", err)
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var topic string
		if err := rows.Scan(&c.Name, &c.LastID, &topic); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		c.Topic, _, _ = strings.Cut(topic, "\n")
		out = append(out, c)
	}
	return out, rows.Err()
}

// PutAgent inserts or replaces an agent.
func (s *Store) PutAgent(ctx context.Context, a *Agent) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents(nick, provider, channel, session_id, cwd, sandbox, tools, token, enabled, last_msg_id, turns, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(nick) DO UPDATE SET provider=excluded.provider, channel=excluded.channel, session_id=excluded.session_id,
		cwd=excluded.cwd, sandbox=excluded.sandbox, tools=excluded.tools, token=excluded.token, enabled=excluded.enabled,
		last_msg_id=excluded.last_msg_id, turns=excluded.turns`,
		a.Nick, a.Provider, a.Channel, a.SessionID, a.Cwd, a.Sandbox, strings.Join(a.Tools, ","), a.Token,
		boolInt(a.Enabled), a.LastMsgID, a.Turns, a.CreatedAt.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("put agent %s: %w", a.Nick, err)
	}
	return nil
}

// Agent fetches one agent by nick.
func (s *Store) Agent(ctx context.Context, nick string) (*Agent, error) {
	return s.queryAgent(ctx, `WHERE nick = ?`, nick)
}

// AgentByToken fetches the agent owning token.
func (s *Store) AgentByToken(ctx context.Context, token string) (*Agent, error) {
	return s.queryAgent(ctx, `WHERE token = ?`, token)
}

// Agents lists every agent, enabled or not.
func (s *Store) Agents(ctx context.Context) ([]Agent, error) {
	rows, err := s.db.QueryContext(ctx, agentSelect+` ORDER BY nick`)
	if err != nil {
		return nil, fmt.Errorf("query agents: %w", err)
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *a)
	}
	return out, rows.Err()
}

const agentSelect = `SELECT nick, provider, channel, session_id, cwd, sandbox, tools, token, enabled, last_msg_id, turns, created_at FROM agents `

func (s *Store) queryAgent(ctx context.Context, where string, arg any) (*Agent, error) {
	rows, err := s.db.QueryContext(ctx, agentSelect+where, arg)
	if err != nil {
		return nil, fmt.Errorf("query agent: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, ErrNotFound
	}
	return scanAgent(rows)
}

func scanAgent(rows *sql.Rows) (*Agent, error) {
	var a Agent
	var tools, created string
	var enabled int
	if err := rows.Scan(&a.Nick, &a.Provider, &a.Channel, &a.SessionID, &a.Cwd, &a.Sandbox, &tools, &a.Token,
		&enabled, &a.LastMsgID, &a.Turns, &created); err != nil {
		return nil, fmt.Errorf("scan agent: %w", err)
	}
	a.Enabled = enabled != 0
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if tools != "" {
		a.Tools = strings.Split(tools, ",")
	}
	return &a, nil
}

// BumpTurn advances the agent's cursor and turn counter without touching the rest of the row,
// so a concurrent !stop or !spawn is not overwritten.
func (s *Store) BumpTurn(ctx context.Context, nick string, lastMsgID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_msg_id = ?, turns = turns + 1 WHERE nick = ?`, lastMsgID, nick)
	if err != nil {
		return fmt.Errorf("bump turn %s: %w", nick, err)
	}
	return nil
}

// RollbackTurn undoes BumpTurn after a turn that did not run to completion.
func (s *Store) RollbackTurn(ctx context.Context, nick string, prevMsgID int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET last_msg_id = ?, turns = MAX(0, turns - 1) WHERE nick = ?`, prevMsgID, nick)
	if err != nil {
		return fmt.Errorf("rollback turn %s: %w", nick, err)
	}
	return nil
}

// UpdateAgentConfig rewrites the operator-set columns of an existing agent; reset clears the
// session and counters. The cursor is untouched unless reset, so a concurrent turn is safe.
func (s *Store) UpdateAgentConfig(ctx context.Context, a *Agent, reset bool) error {
	q := `UPDATE agents SET provider=?, channel=?, cwd=?, sandbox=?, tools=?, enabled=1`
	if reset {
		q += `, session_id='', last_msg_id=0, turns=0`
	}
	_, err := s.db.ExecContext(ctx, q+` WHERE nick=?`, a.Provider, a.Channel, a.Cwd, a.Sandbox, strings.Join(a.Tools, ","), a.Nick)
	if err != nil {
		return fmt.Errorf("update agent %s: %w", a.Nick, err)
	}
	return nil
}

// ConsumeGrant atomically takes one turn from the channel's release grant; false if none left.
func (s *Store) ConsumeGrant(ctx context.Context, channel string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE settings SET value = CAST(CAST(value AS INTEGER) - 1 AS TEXT) WHERE key = ? AND CAST(value AS INTEGER) > 0`, "grant:"+channel)
	if err != nil {
		return false, fmt.Errorf("consume grant %s: %w", channel, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// SetSession records the provider session id.
func (s *Store) SetSession(ctx context.Context, nick, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET session_id = ? WHERE nick = ?`, sessionID, nick)
	if err != nil {
		return fmt.Errorf("set session %s: %w", nick, err)
	}
	return nil
}

// SetEnabled flips the enabled flag.
func (s *Store) SetEnabled(ctx context.Context, nick string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET enabled = ? WHERE nick = ?`, boolInt(enabled), nick)
	if err != nil {
		return fmt.Errorf("set enabled %s: %w", nick, err)
	}
	return nil
}

// ReleaseTurns lowers the turn counters of every agent in channel by n, floored at zero.
func (s *Store) ReleaseTurns(ctx context.Context, channel string, n int) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET turns = MAX(0, turns - ?) WHERE channel = ?`, n, channel)
	if err != nil {
		return fmt.Errorf("release turns %s: %w", channel, err)
	}
	return nil
}

// ChannelTurns sums turns of every agent bound to channel.
func (s *Store) ChannelTurns(ctx context.Context, channel string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(turns),0) FROM agents WHERE channel = ?`, channel).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("channel turns: %w", err)
	}
	return n, nil
}

// Setting reads a key; empty string if absent.
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("setting %s: %w", key, err)
	}
	return v, nil
}

// SetSetting writes a key.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	if err != nil {
		return fmt.Errorf("set setting %s: %w", key, err)
	}
	return nil
}

// Tags are comma-joined: "waiting for human" contains spaces.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
