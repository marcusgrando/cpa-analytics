// Package store keeps usage rows in SQLite.
package store

import (
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Row is one usage record. Times are unix milliseconds.
type Row struct {
	RequestID            string
	TraceID              string
	RequestedAt          int64
	Provider             string
	ExecutorType         string
	AuthType             string
	AuthID               string
	Model                string
	Alias                string
	ResponseModel        string
	APIKeyHash           string
	AccessProvider       string
	Source               string
	SessionID            string
	ParentSessionID      string
	ReasoningEffort      string
	ServiceTier          string
	EffectiveServiceTier string
	ResponseServiceTier  string
	Stream               bool
	Generate             bool
	Failed               bool
	StatusCode           int
	LatencyMs            int64
	TTFTMs               int64

	// Raw counters as CPA reported them.
	InputTokens         int64
	OutputTokens        int64
	ReasoningTokens     int64
	CachedTokens        int64
	CacheReadTokens     int64
	CacheCreationTokens int64
	TotalTokens         int64

	// Normalized, non-overlapping buckets from internal/accounting.
	UncachedInput      int64
	CacheRead          int64
	CacheWrite         int64
	OutputNonReasoning int64
	OutputReasoning    int64
	Unclassified       int64
	AccountingQuality  string

	UserAgent     string
	ClientName    string
	ClientVersion string
}

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open creates the parent directory if needed, opens the database in WAL
// mode and applies pending migrations.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create database directory: %w", err)
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(ON)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("open database %s: %w", path, err)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func migrate(db *sql.DB) error {
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(name, ".sql")
		var exists int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists > 0 {
			continue
		}
		body, err := migrations.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := db.Begin()
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", version, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", version, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES (?)`, version); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", version, err)
		}
	}
	return nil
}

// DB exposes the database for read queries.
func (s *Store) DB() *sql.DB { return s.db }

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

const insertSQL = `INSERT OR IGNORE INTO requests (
	request_id, trace_id, requested_at, provider, executor_type, auth_type, auth_id,
	model, alias, response_model, api_key_hash, access_provider, source, session_id, parent_session_id,
	reasoning_effort, service_tier, effective_service_tier, response_service_tier,
	stream, generate, failed, status_code, latency_ms, ttft_ms,
	input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens,
	uncached_input, cache_read, cache_write, output_non_reasoning, output_reasoning, unclassified, accounting_quality,
	user_agent, client_name, client_version
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

// InsertBatch writes rows in one transaction. A row whose request_id already
// exists is skipped, so replayed records are harmless.
func (s *Store) InsertBatch(rows []Row) (err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin insert: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	stmt, err := tx.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err = stmt.Exec(
			r.RequestID, r.TraceID, r.RequestedAt, r.Provider, r.ExecutorType, r.AuthType, r.AuthID,
			r.Model, r.Alias, r.ResponseModel, r.APIKeyHash, r.AccessProvider, r.Source, r.SessionID, r.ParentSessionID,
			r.ReasoningEffort, r.ServiceTier, r.EffectiveServiceTier, r.ResponseServiceTier,
			r.Stream, r.Generate, r.Failed, r.StatusCode, r.LatencyMs, r.TTFTMs,
			r.InputTokens, r.OutputTokens, r.ReasoningTokens, r.CachedTokens, r.CacheReadTokens, r.CacheCreationTokens, r.TotalTokens,
			r.UncachedInput, r.CacheRead, r.CacheWrite, r.OutputNonReasoning, r.OutputReasoning, r.Unclassified, r.AccountingQuality,
			r.UserAgent, r.ClientName, r.ClientVersion,
		); err != nil {
			return fmt.Errorf("insert request %s: %w", r.RequestID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit insert: %w", err)
	}
	return nil
}

// Prune deletes rows requested before the cutoff.
func (s *Store) Prune(before time.Time) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM requests WHERE requested_at < ?`, before.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("prune requests: %w", err)
	}
	return res.RowsAffected()
}

// SaveKV stores a value under key, replacing any previous value.
func (s *Store) SaveKV(key string, value []byte) error {
	if _, err := s.db.Exec(`INSERT INTO kv (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("save %s: %w", key, err)
	}
	return nil
}

// LoadKV returns the value for key, or nil when the key does not exist.
func (s *Store) LoadKV(key string) ([]byte, error) {
	var value []byte
	err := s.db.QueryRow(`SELECT value FROM kv WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load %s: %w", key, err)
	}
	return value, nil
}
