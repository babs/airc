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

	"airc/pkg/proto"
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
	Nick      string `json:"nick"`
	Provider  string `json:"provider"`
	Channel   string `json:"channel"`
	SessionID string `json:"session_id"`
	Cwd       string `json:"cwd"`
	// Home overrides the CLI config dir (CLAUDE_CONFIG_DIR / CODEX_HOME); empty inherits ours.
	Home    string `json:"home,omitempty"`
	Sandbox string `json:"sandbox"`
	// Model overrides the CLI's default model; empty keeps the CLI's own choice.
	Model string `json:"model,omitempty"`
	// Status is what the agent last declared it is doing ("[status: ...]").
	Status    string   `json:"status,omitempty"`
	Tools     []string `json:"tools"`
	Token     string   `json:"-"`
	Enabled   bool     `json:"enabled"`
	LastMsgID int64    `json:"last_msg_id"`
	Turns     int      `json:"turns"`
	// Gen counts operator mutations (spawn, stop, session attach); a turn claim must match it.
	Gen       int       `json:"gen"`
	CreatedAt time.Time `json:"created_at"`
}

// Channel summarises one channel for listings.
type Channel struct {
	Name   string `json:"name"`
	Topic  string `json:"topic"`
	LastID int64  `json:"last_id"`
	// Cwd is the channel's workspace (setting "cwd:<name>"), the default for spawned agents.
	Cwd string `json:"cwd,omitempty"`
	// LastFrom and LastBody describe the most recent message (first line, truncated).
	LastFrom string `json:"last_from,omitempty"`
	LastBody string `json:"last_body,omitempty"`
	LastTS   string `json:"last_ts,omitempty"`
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
	// Columns added after the first release; SQLite has no ADD COLUMN IF NOT EXISTS.
	// inflight_prev: the cursor before the running turn, -1 when idle. Survives a crash so the
	// interrupted wake is rolled back and replayed at startup.
	for _, col := range []string{"home TEXT NOT NULL DEFAULT ''", "inflight_prev INTEGER NOT NULL DEFAULT -1", "gen INTEGER NOT NULL DEFAULT 0", "status TEXT NOT NULL DEFAULT ''", "model TEXT NOT NULL DEFAULT ''"} {
		if _, err := db.ExecContext(context.Background(), "ALTER TABLE agents ADD COLUMN "+col); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			_ = db.Close()
			return nil, fmt.Errorf("migrate column: %w", err)
		}
	}
	return &Store{db: db}, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// Insert stores m and fills its ID and TS.
func (s *Store) Insert(ctx context.Context, m *Message) error {
	return insertMessage(ctx, s.db, m)
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func insertMessage(ctx context.Context, db execer, m *Message) error {
	m.TS = time.Now().UTC()
	res, err := db.ExecContext(ctx,
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

// Latest returns the newest limit messages of channel with afterID < id < beforeID, oldest first.
// Unlike Messages it keeps the newest rows, so the triggering message is never cut off.
func (s *Store) Latest(ctx context.Context, channel string, afterID, beforeID int64, limit int) ([]Message, error) {
	if beforeID <= 0 {
		beforeID = 1<<62 - 1
	}
	msgs, err := s.queryMessages(ctx,
		`SELECT id, ts, channel, sender, recipient, body, tags FROM messages WHERE channel = ? AND id > ? AND id < ? ORDER BY id DESC LIMIT ?`,
		channel, afterID, beforeID, limit)
	if err != nil {
		return nil, err
	}
	for i, j := 0, len(msgs)-1; i < j; i, j = i+1, j-1 {
		msgs[i], msgs[j] = msgs[j], msgs[i]
	}
	return msgs, nil
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
		SELECT m.channel, MAX(m.id),
		  COALESCE((SELECT body FROM messages f WHERE f.channel = m.channel AND substr(f.body, 1, 1) != '!' AND f.sender != 'airc' ORDER BY id ASC LIMIT 1), ''),
		  COALESCE((SELECT sender FROM messages l WHERE l.channel = m.channel ORDER BY id DESC LIMIT 1), ''),
		  COALESCE((SELECT body FROM messages l WHERE l.channel = m.channel ORDER BY id DESC LIMIT 1), ''),
		  COALESCE((SELECT ts FROM messages l WHERE l.channel = m.channel ORDER BY id DESC LIMIT 1), '')
		FROM messages m GROUP BY m.channel ORDER BY MAX(m.id) DESC`)
	if err != nil {
		return nil, fmt.Errorf("query channels: %w", err)
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var topic, lastBody string
		if err := rows.Scan(&c.Name, &c.LastID, &topic, &c.LastFrom, &lastBody, &c.LastTS); err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		c.Topic, _, _ = strings.Cut(topic, "\n")
		c.LastBody, _, _ = strings.Cut(strings.TrimSpace(lastBody), "\n")
		if len(c.LastBody) > 120 {
			c.LastBody = c.LastBody[:120] + "…"
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	kept := out[:0]
	for i := range out {
		if a, err := s.Setting(ctx, "archived:"+out[i].Name); err != nil {
			return nil, err
		} else if a == "1" {
			continue
		}
		if out[i].Cwd, err = s.Setting(ctx, "cwd:"+out[i].Name); err != nil {
			return nil, err
		}
		// An explicit !topic wins over the first message.
		if t, err := s.Setting(ctx, "topic:"+out[i].Name); err != nil {
			return nil, err
		} else if t != "" {
			out[i].Topic = t
		}
		kept = append(kept, out[i])
	}
	return kept, nil
}

// RenameChannel moves messages, agents and per-channel settings from old to new, atomically.
func (s *Store) RenameChannel(ctx context.Context, oldName, newName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("rename channel: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	stmts := []string{
		`UPDATE messages SET channel = ? WHERE channel = ?`,
		`UPDATE agents SET channel = ? WHERE channel = ?`,
	}
	for _, q := range stmts {
		if _, err := tx.ExecContext(ctx, q, newName, oldName); err != nil {
			return fmt.Errorf("rename channel: %w", err)
		}
	}
	for _, k := range []string{"cwd:", "topic:", "lock:", "grant:", "archived:"} {
		if _, err := tx.ExecContext(ctx, `UPDATE OR REPLACE settings SET key = ? WHERE key = ?`, k+newName, k+oldName); err != nil {
			return fmt.Errorf("rename channel setting %s: %w", k, err)
		}
	}
	return tx.Commit()
}

// PutAgent inserts or replaces an agent.
func (s *Store) PutAgent(ctx context.Context, a *Agent) error {
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO agents(nick, provider, channel, session_id, cwd, home, sandbox, model, status, tools, token, enabled, last_msg_id, turns, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(nick) DO UPDATE SET provider=excluded.provider, channel=excluded.channel, session_id=excluded.session_id,
		cwd=excluded.cwd, home=excluded.home, sandbox=excluded.sandbox, model=excluded.model, status=excluded.status, tools=excluded.tools, token=excluded.token, enabled=excluded.enabled,
		last_msg_id=excluded.last_msg_id, turns=excluded.turns`,
		a.Nick, a.Provider, a.Channel, a.SessionID, a.Cwd, a.Home, a.Sandbox, a.Model, a.Status, strings.Join(a.Tools, ","), a.Token,
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

const agentSelect = `SELECT nick, provider, channel, session_id, cwd, home, sandbox, model, status, tools, token, enabled, last_msg_id, turns, gen, created_at FROM agents `

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
	if err := rows.Scan(&a.Nick, &a.Provider, &a.Channel, &a.SessionID, &a.Cwd, &a.Home, &a.Sandbox, &a.Model, &a.Status, &tools, &a.Token,
		&enabled, &a.LastMsgID, &a.Turns, &a.Gen, &created); err != nil {
		return nil, fmt.Errorf("scan agent: %w", err)
	}
	a.Enabled = enabled != 0
	a.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if tools != "" {
		a.Tools = strings.Split(tools, ",")
	}
	return &a, nil
}

// ClaimTurn claims a turn in one conditional update: the agent must still be enabled, bound to
// channel, at configuration gen, idle and behind triggerID. False means the world moved between
// the read and the claim (a !stop, any respawn, a session attach, a duplicate wake) and the turn
// must not run with the configuration that was read.
func (s *Store) ClaimTurn(ctx context.Context, nick, channel string, gen int, triggerID int64) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE agents SET inflight_prev = last_msg_id, last_msg_id = ?, turns = turns + 1
		 WHERE nick = ? AND channel = ? AND gen = ? AND enabled = 1 AND inflight_prev = -1 AND last_msg_id < ?`,
		triggerID, nick, channel, gen, triggerID)
	if err != nil {
		return false, fmt.Errorf("claim turn %s: %w", nick, err)
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

// RollbackTurn undoes BumpTurn after a turn that did not run to completion. A no-op when the turn
// is no longer in flight (a reset !spawn cleared it), so a stale rollback cannot move the cursor.
func (s *Store) RollbackTurn(ctx context.Context, nick string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE agents SET last_msg_id = inflight_prev, turns = MAX(0, turns - 1), inflight_prev = -1 WHERE nick = ? AND inflight_prev >= 0`, nick)
	if err != nil {
		return fmt.Errorf("rollback turn %s: %w", nick, err)
	}
	return nil
}

// FinishTurn closes the in-flight state, records the session id the provider returned and stores
// the reply, all in one transaction: a crash can never leave a finished turn without its reply,
// nor a stored reply with a turn still marked in flight. The update is conditional on the
// configuration generation the turn was claimed under: if an operator mutated the agent meanwhile
// (a reset respawn on a first turn leaves session_id empty, which a CASE on the old session would
// match and overwrite with the previous provider's id), the turn is stale. The provider did run,
// so a stale turn consumes its trigger: the in-flight marker is cleared and the cursor kept, or
// the row is left as the reset left it. Rolling back instead would put the completed request in
// the next delta and duplicate its side effects. The reply is kept as a system audit line that
// addresses nobody, so the work stays visible without touching the replacement configuration or
// waking a peer. The reply's ID and TS are filled in; the caller broadcasts it. Returns whether
// the turn was stale.
func (s *Store) FinishTurn(ctx context.Context, nick string, gen int, newSession string, reply *Message) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("finish turn %s: %w", nick, err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after Commit
	res, err := tx.ExecContext(ctx,
		`UPDATE agents SET session_id = ?, inflight_prev = -1 WHERE nick = ? AND gen = ? AND inflight_prev >= 0`,
		newSession, nick, gen)
	if err != nil {
		return false, fmt.Errorf("finish turn %s: %w", nick, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("finish turn %s: %w", nick, err)
	}
	stale := n == 0
	if stale {
		if _, err := tx.ExecContext(ctx, `UPDATE agents SET inflight_prev = -1 WHERE nick = ? AND inflight_prev >= 0`, nick); err != nil {
			return false, fmt.Errorf("finish turn %s: %w", nick, err)
		}
		reply.Body = fmt.Sprintf("Stale reply from %s (its configuration changed during the turn; kept for the record, nobody woken):\n\n%s", nick, reply.Body)
		reply.From, reply.To, reply.Tags = proto.SystemNick, "", nil
	}
	if err := insertMessage(ctx, tx, reply); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("finish turn %s: %w", nick, err)
	}
	return stale, nil
}

// RecoverInflight rolls back every turn a crash left in flight, marks each agent as needing an
// operator decision (see Recovering) and returns the affected nicks.
func (s *Store) RecoverInflight(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT nick FROM agents WHERE inflight_prev >= 0`)
	if err != nil {
		return nil, fmt.Errorf("query inflight: %w", err)
	}
	var nicks []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			_ = rows.Close()
			return nil, fmt.Errorf("scan inflight: %w", err)
		}
		nicks = append(nicks, n)
	}
	// Closed before the updates: the single connection cannot write while a cursor is open.
	_ = rows.Close()
	for _, n := range nicks {
		// One transaction: a rolled-back cursor without its block would be an ordinary pending
		// wake that the next startup reruns on its own.
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return nil, fmt.Errorf("recover %s: %w", n, err)
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE agents SET last_msg_id = inflight_prev, turns = MAX(0, turns - 1), inflight_prev = -1 WHERE nick = ? AND inflight_prev >= 0`, n)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?, '1') ON CONFLICT(key) DO UPDATE SET value='1'`, "recover:"+n)
		}
		if err == nil {
			err = tx.Commit()
		}
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("recover %s: %w", n, err)
		}
	}
	return nicks, nil
}

// Recovering reports whether the agent's last turn was cut by a crash and no operator has released
// it since. Persisted: a second crash must not silently lift the block.
func (s *Store) Recovering(ctx context.Context, nick string) (bool, error) {
	v, err := s.Setting(ctx, "recover:"+nick)
	return v == "1", err
}

// SetRecovering sets or clears the crash-recovery block.
func (s *Store) SetRecovering(ctx context.Context, nick string, on bool) error {
	v := "0"
	if on {
		v = "1"
	}
	return s.SetSetting(ctx, "recover:"+nick, v)
}

// UpdateAgentConfig rewrites the operator-set columns of an existing agent; reset clears the
// session and counters. The cursor is untouched unless reset, so a concurrent turn is safe.
func (s *Store) UpdateAgentConfig(ctx context.Context, a *Agent, reset bool) error {
	q := `UPDATE agents SET provider=?, channel=?, cwd=?, home=?, sandbox=?, model=?, tools=?, enabled=1, gen=gen+1`
	if reset {
		q += `, session_id='', last_msg_id=0, turns=0, inflight_prev=-1`
	}
	_, err := s.db.ExecContext(ctx, q+` WHERE nick=?`, a.Provider, a.Channel, a.Cwd, a.Home, a.Sandbox, a.Model, strings.Join(a.Tools, ","), a.Nick)
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

// SetSession records an operator-attached session id.
func (s *Store) SetSession(ctx context.Context, nick, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET session_id = ?, gen = gen + 1 WHERE nick = ?`, sessionID, nick)
	if err != nil {
		return fmt.Errorf("set session %s: %w", nick, err)
	}
	return nil
}

// SetStatus records the agent's self-declared activity.
func (s *Store) SetStatus(ctx context.Context, nick, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET status = ? WHERE nick = ?`, status, nick)
	if err != nil {
		return fmt.Errorf("set status %s: %w", nick, err)
	}
	return nil
}

// SetEnabled flips the enabled flag.
func (s *Store) SetEnabled(ctx context.Context, nick string, enabled bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE agents SET enabled = ?, gen = gen + 1 WHERE nick = ?`, boolInt(enabled), nick)
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
