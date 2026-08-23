package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/ace-foundry/argus-testing/argus/internal/domain"
	_ "modernc.org/sqlite"
)

const interruptedMessage = "Run interrupted by server restart"

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Initialize() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS runs (
    id TEXT PRIMARY KEY, url TEXT NOT NULL, instructions TEXT NOT NULL,
    status TEXT NOT NULL, created_at TEXT NOT NULL, updated_at TEXT NOT NULL,
    report_json TEXT, error TEXT,
    provider TEXT NOT NULL DEFAULT 'gemini',
    allow_mutations INTEGER NOT NULL DEFAULT 0,
    allow_destructive INTEGER NOT NULL DEFAULT 0,
    allowed_origins_json TEXT NOT NULL DEFAULT '[]'
);
CREATE TABLE IF NOT EXISTS events (
    id INTEGER PRIMARY KEY AUTOINCREMENT, run_id TEXT NOT NULL,
    type TEXT NOT NULL, data_json TEXT NOT NULL, created_at TEXT NOT NULL,
    FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS events_run_id_id ON events(run_id, id);
CREATE TABLE IF NOT EXISTS screenshots (
    id TEXT PRIMARY KEY, run_id TEXT NOT NULL, path TEXT NOT NULL,
    created_at TEXT NOT NULL,
    FOREIGN KEY(run_id) REFERENCES runs(id) ON DELETE CASCADE
);`)
	if err != nil {
		return err
	}
	migrations := []struct {
		column    string
		statement string
	}{
		{"provider", `ALTER TABLE runs ADD COLUMN provider TEXT NOT NULL DEFAULT 'gemini'`},
		{"allow_mutations", `ALTER TABLE runs ADD COLUMN allow_mutations INTEGER NOT NULL DEFAULT 0`},
		{"allow_destructive", `ALTER TABLE runs ADD COLUMN allow_destructive INTEGER NOT NULL DEFAULT 0`},
		{"allowed_origins_json", `ALTER TABLE runs ADD COLUMN allowed_origins_json TEXT NOT NULL DEFAULT '[]'`},
	}
	for _, migration := range migrations {
		exists, err := s.hasRunColumn(migration.column)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := s.db.Exec(migration.statement); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) hasRunColumn(name string) (bool, error) {
	rows, err := s.db.Query(`PRAGMA table_info(runs)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var index int
		var column, dataType string
		var notNull, primaryKey int
		var defaultValue sql.NullString
		if err := rows.Scan(&index, &column, &dataType, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if column == name {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	return false, nil
}

func (s *Store) CreateRun(url, instructions string, policy domain.RunPolicy) (*domain.Run, error) {
	return s.CreateRunWithProvider(url, instructions, domain.ProviderGemini, policy)
}

func (s *Store) CreateRunWithProvider(url, instructions string, provider domain.ProviderID, policy domain.RunPolicy) (*domain.Run, error) {
	if !domain.ValidProvider(provider) {
		return nil, errors.New("invalid provider")
	}
	id, err := newID()
	if err != nil {
		return nil, err
	}
	now := timestamp()
	if policy.AllowedOrigins == nil {
		policy.AllowedOrigins = []string{}
	}
	encodedOrigins, err := json.Marshal(policy.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT INTO runs (
id, url, instructions, provider, status, created_at, updated_at, report_json, error,
allow_mutations, allow_destructive, allowed_origins_json
) VALUES (?, ?, ?, ?, ?, ?, ?, NULL, NULL, ?, ?, ?)`, id, url, instructions, provider, domain.RunStatusQueued, now, now, policy.AllowMutations, policy.AllowDestructive, string(encodedOrigins)); err != nil {
		return nil, err
	}
	return s.GetRun(id, false)
}

func (s *Store) ListRuns(limit int) ([]domain.Run, error) {
	rows, err := s.db.Query(`SELECT id, url, instructions, provider, status, created_at, updated_at,
report_json, error, allow_mutations, allow_destructive, allowed_origins_json
FROM runs ORDER BY created_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	runs := []domain.Run{}
	for rows.Next() {
		run, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		runs = append(runs, *run)
	}
	return runs, rows.Err()
}

func (s *Store) GetRun(id string, includeEvents bool) (*domain.Run, error) {
	run, err := scanRun(s.db.QueryRow(`SELECT id, url, instructions, provider, status, created_at, updated_at,
report_json, error, allow_mutations, allow_destructive, allowed_origins_json
FROM runs WHERE id = ?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if includeEvents {
		events, err := s.EventsAfter(id, 0)
		if err != nil {
			return nil, err
		}
		run.Events = events
	}
	return run, nil
}

func (s *Store) AddEvent(runID string, eventType domain.EventType, data map[string]any) (*domain.RunEvent, error) {
	return addEvent(s.db, runID, eventType, data, timestamp())
}

func (s *Store) EventsAfter(runID string, eventID int64) ([]domain.RunEvent, error) {
	rows, err := s.db.Query(`SELECT * FROM events WHERE run_id = ? AND id > ? ORDER BY id`, runID, eventID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	events := []domain.RunEvent{}
	for rows.Next() {
		event, err := scanEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, *event)
	}
	return events, rows.Err()
}

func (s *Store) Transition(runID string, expected []domain.RunStatus, status domain.RunStatus, eventType domain.EventType, eventData map[string]any, report *domain.RunReport, runError *string) (*domain.RunEvent, error) {
	if len(expected) == 0 || len(expected) > 5 || isTerminal(status) && containsTerminal(expected) {
		return nil, nil
	}
	now := timestamp()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	expectedValues := [5]domain.RunStatus{}
	copy(expectedValues[:], expected)
	args := []any{status, now, reportJSON(report), runError, runID, expectedValues[0], expectedValues[1], expectedValues[2], expectedValues[3], expectedValues[4], domain.RunStatusPassed, domain.RunStatusFailed, domain.RunStatusCancelled}
	result, err := tx.Exec(`UPDATE runs SET status = ?, updated_at = ?, report_json = COALESCE(?, report_json), error = ? WHERE id = ? AND status IN (?, ?, ?, ?, ?) AND status NOT IN (?, ?, ?)`, args...)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, tx.Commit()
	}
	event, err := addEvent(tx, runID, eventType, eventData, now)
	if err != nil {
		return nil, err
	}
	return event, tx.Commit()
}

func (s *Store) ReconcileInterrupted() ([]domain.RunEvent, error) {
	now := timestamp()
	data := map[string]any{"kind": "interrupted", "message": interruptedMessage}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT id FROM runs WHERE status IN (?, ?)`, domain.RunStatusQueued, domain.RunStatusRunning)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	events := make([]domain.RunEvent, 0, len(ids))
	for _, id := range ids {
		result, err := tx.Exec(`UPDATE runs SET status = ?, updated_at = ?, error = ? WHERE id = ? AND status IN (?, ?)`, domain.RunStatusFailed, now, interruptedMessage, id, domain.RunStatusQueued, domain.RunStatusRunning)
		if err != nil {
			return nil, err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return nil, err
		}
		if changed != 1 {
			continue
		}
		event, err := addEvent(tx, id, domain.EventRunFailed, data, now)
		if err != nil {
			return nil, err
		}
		events = append(events, *event)
	}
	return events, tx.Commit()
}

func (s *Store) AddScreenshot(runID, path string) (string, error) {
	id, err := newID()
	if err != nil {
		return "", err
	}
	_, err = s.db.Exec(`INSERT INTO screenshots VALUES (?, ?, ?, ?)`, id, runID, path, timestamp())
	return id, err
}

type rowScanner interface{ Scan(...any) error }
type eventExecutor interface {
	Exec(string, ...any) (sql.Result, error)
}

func scanRun(row rowScanner) (*domain.Run, error) {
	var run domain.Run
	var report sql.NullString
	var runError sql.NullString
	var allowMutations bool
	var allowDestructive bool
	var allowedOrigins string
	if err := row.Scan(&run.ID, &run.URL, &run.Instructions, &run.Provider, &run.Status, &run.CreatedAt, &run.UpdatedAt, &report, &runError, &allowMutations, &allowDestructive, &allowedOrigins); err != nil {
		return nil, err
	}
	policy := domain.RunPolicy{AllowMutations: allowMutations, AllowDestructive: allowDestructive, AllowedOrigins: []string{}}
	if err := json.Unmarshal([]byte(allowedOrigins), &policy.AllowedOrigins); err != nil {
		return nil, err
	}
	run.Policy = &policy
	if report.Valid {
		var value domain.RunReport
		if err := json.Unmarshal([]byte(report.String), &value); err != nil {
			return nil, err
		}
		run.Report = &value
	}
	if runError.Valid {
		value := runError.String
		run.Error = &value
	}
	return &run, nil
}

func scanEvent(row rowScanner) (*domain.RunEvent, error) {
	var event domain.RunEvent
	var data string
	if err := row.Scan(&event.ID, &event.RunID, &event.Type, &data, &event.CreatedAt); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(data), &event.Data); err != nil {
		return nil, err
	}
	return &event, nil
}

func addEvent(executor eventExecutor, runID string, eventType domain.EventType, data map[string]any, now string) (*domain.RunEvent, error) {
	if data == nil {
		data = map[string]any{}
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}
	result, err := executor.Exec(`INSERT INTO events(run_id, type, data_json, created_at) VALUES (?, ?, ?, ?)`, runID, eventType, encoded, now)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return &domain.RunEvent{ID: id, RunID: runID, Type: eventType, Data: data, CreatedAt: now}, nil
}

func reportJSON(report *domain.RunReport) any {
	if report == nil {
		return nil
	}
	encoded, _ := json.Marshal(report)
	return string(encoded)
}

func isTerminal(status domain.RunStatus) bool {
	return status == domain.RunStatusPassed || status == domain.RunStatusFailed || status == domain.RunStatusCancelled
}
func containsTerminal(statuses []domain.RunStatus) bool {
	for _, status := range statuses {
		if isTerminal(status) {
			return true
		}
	}
	return false
}
func timestamp() string { return time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00") }
func newID() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}
