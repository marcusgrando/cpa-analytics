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

	"github.com/marcusgrando/cpa-analytics/internal/accounting"
	"github.com/marcusgrando/cpa-analytics/internal/client"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
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

	// AuthIndex identifies the credential as CPA's management API lists it.
	AuthIndex string
	// Limit usage the response reported, in percent, and the reset in Unix
	// milliseconds. Invalid when the response did not report it.
	LimitSessionPct   sql.NullFloat64
	LimitSessionReset sql.NullInt64
	LimitWeeklyPct    sql.NullFloat64
	LimitWeeklyReset  sql.NullInt64
}

// QuotaSnapshot is one polled reading of a subscription quota window for a
// provider that does not return limit headers on generation responses.
type QuotaSnapshot struct {
	RecordedAt int64
	Provider   string
	AuthIndex  string
	GroupID    string
	GroupLabel string
	WindowKind string
	UsedPct    float64
	ResetAt    sql.NullInt64
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
	if err := runGoMigration(db, "004_reparse_clients", reparseClients); err != nil {
		return err
	}
	if err := runGoMigration(db, "005_meta_devin_accounting", reclassifyMetaDevin); err != nil {
		return err
	}
	return nil
}

func runGoMigration(db *sql.DB, version string, fn func(*sql.Tx) error) error {
	var exists int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = ?`, version).Scan(&exists); err != nil {
		return fmt.Errorf("check migration %s: %w", version, err)
	}
	if exists > 0 {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin migration %s: %w", version, err)
	}
	if err := fn(tx); err != nil {
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
	return nil
}

// reparseClients rebuilds client_name and client_version from the stored raw
// user_agent so previously collapsed "other" agents regain their real names and
// versions across historical rows.
func reparseClients(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT DISTINCT user_agent FROM requests WHERE user_agent <> ''`)
	if err != nil {
		return err
	}
	var agents []string
	for rows.Next() {
		var ua string
		if err := rows.Scan(&ua); err != nil {
			rows.Close()
			return err
		}
		agents = append(agents, ua)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(agents) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`UPDATE requests SET client_name = ?, client_version = ? WHERE user_agent = ? AND (client_name <> ? OR client_version <> ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, ua := range agents {
		name, version := client.Parse(ua)
		if _, err := stmt.Exec(name, version, ua, name, version); err != nil {
			return err
		}
	}
	return nil
}

// reclassifyMetaDevin rebuilds token buckets for historical meta and devin rows
// that were stored as unclassified before accounting recognized those providers.
func reclassifyMetaDevin(tx *sql.Tx) error {
	rows, err := tx.Query(`SELECT id, provider, executor_type, input_tokens, output_tokens, reasoning_tokens, cached_tokens, cache_read_tokens, cache_creation_tokens, total_tokens
		FROM requests WHERE accounting_quality = 'unclassified'
		AND (lower(provider || ' ' || executor_type) LIKE '%meta%' OR lower(provider || ' ' || executor_type) LIKE '%devin%')`)
	if err != nil {
		return err
	}
	type item struct {
		id                 int64
		provider, executor string
		detail             pluginapi.UsageDetail
	}
	var pending []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.provider, &it.executor, &it.detail.InputTokens, &it.detail.OutputTokens,
			&it.detail.ReasoningTokens, &it.detail.CachedTokens, &it.detail.CacheReadTokens, &it.detail.CacheCreationTokens, &it.detail.TotalTokens); err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, it)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if len(pending) == 0 {
		return nil
	}
	stmt, err := tx.Prepare(`UPDATE requests SET uncached_input = ?, cache_read = ?, cache_write = ?, output_non_reasoning = ?, output_reasoning = ?, unclassified = ?, accounting_quality = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, it := range pending {
		b := accounting.Normalize(it.provider, it.executor, it.detail)
		if b.Quality == accounting.QualityUnclassified {
			continue
		}
		if _, err := stmt.Exec(b.Uncached, b.CacheRead, b.CacheWrite, b.OutputNonReasoning, b.Reasoning, b.Unclassified, b.Quality, it.id); err != nil {
			return err
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
	user_agent, client_name, client_version,
	auth_index, limit_session_pct, limit_session_reset, limit_weekly_pct, limit_weekly_reset
) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`

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
			r.AuthIndex, r.LimitSessionPct, r.LimitSessionReset, r.LimitWeeklyPct, r.LimitWeeklyReset,
		); err != nil {
			return fmt.Errorf("insert request %s: %w", r.RequestID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit insert: %w", err)
	}
	return nil
}

// InsertQuotaSnapshots writes polled quota snapshots in one transaction.
func (s *Store) InsertQuotaSnapshots(snaps []QuotaSnapshot) (err error) {
	if len(snaps) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("begin insert quota snapshots: %w", err)
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, tx.Rollback())
		}
	}()
	stmt, err := tx.Prepare(`INSERT INTO quota_snapshots (
		recorded_at, provider, auth_index, group_id, group_label, window_kind, used_pct, reset_at
	) VALUES (?,?,?,?,?,?,?,?)`)
	if err != nil {
		return fmt.Errorf("prepare insert quota snapshots: %w", err)
	}
	defer stmt.Close()
	for _, sn := range snaps {
		if _, err = stmt.Exec(
			sn.RecordedAt, sn.Provider, sn.AuthIndex, sn.GroupID, sn.GroupLabel, sn.WindowKind, sn.UsedPct, sn.ResetAt,
		); err != nil {
			return fmt.Errorf("insert quota snapshot %s/%s: %w", sn.AuthIndex, sn.GroupID, err)
		}
	}
	if err = tx.Commit(); err != nil {
		return fmt.Errorf("commit insert quota snapshots: %w", err)
	}
	return nil
}

// Prune deletes rows requested before the cutoff and polled quota snapshots
// recorded before it.
func (s *Store) Prune(before time.Time) (int64, error) {
	cutoff := before.UnixMilli()
	res, err := s.db.Exec(`DELETE FROM requests WHERE requested_at < ?`, cutoff)
	if err != nil {
		return 0, fmt.Errorf("prune requests: %w", err)
	}
	if _, err := s.db.Exec(`DELETE FROM quota_snapshots WHERE recorded_at < ?`, cutoff); err != nil {
		return 0, fmt.Errorf("prune quota snapshots: %w", err)
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
