// Package usage provides usage tracking and persistence functionality for the CLI Proxy API server.
// It includes SQLite storage for request statistics to enable long-term usage analysis.
package usage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
	coreusage "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	// DefaultUsageDBRetentionDays is the default number of days to retain usage history.
	DefaultUsageDBRetentionDays = 90

	// usageDBSchemaVersion is the database schema version.
	usageDBSchemaVersion = 1
)

// UsageRecord represents a single usage record for SQLite storage.
type UsageRecord struct {
	ID              int64     `json:"id"`
	APIKey          string    `json:"api_key"`
	Model           string    `json:"model"`
	Source          string    `json:"source"`
	AuthIndex       string    `json:"auth_index"`
	InputTokens     int64     `json:"input_tokens"`
	OutputTokens    int64     `json:"output_tokens"`
	ReasoningTokens int64     `json:"reasoning_tokens"`
	CachedTokens    int64     `json:"cached_tokens"`
	TotalTokens     int64     `json:"total_tokens"`
	Failed          bool      `json:"failed"`
	RequestedAt     time.Time `json:"requested_at"`
	CreatedAt       time.Time `json:"created_at"`
}

// UsageSQLiteStore manages persistent storage of usage records in SQLite.
type UsageSQLiteStore struct {
	mu            sync.RWMutex
	db            *sql.DB
	dbPath        string
	retentionDays int
}

// NewUsageSQLiteStore creates a new SQLite store for usage history.
func NewUsageSQLiteStore(dbPath string, retentionDays int) (*UsageSQLiteStore, error) {
	if retentionDays <= 0 {
		retentionDays = DefaultUsageDBRetentionDays
	}

	log.Infof("[UsageSQLite] Creating usage database store, path: %s, retention: %d days", dbPath, retentionDays)

	// Ensure directory exists
	dir := filepath.Dir(dbPath)
	log.Debugf("[UsageSQLite] Ensuring directory exists: %s", dir)
	if err := os.MkdirAll(dir, 0700); err != nil {
		log.Errorf("[UsageSQLite] Failed to create database directory %s: %v", dir, err)
		return nil, fmt.Errorf("failed to create database directory: %w", err)
	}

	log.Debugf("[UsageSQLite] Opening SQLite database: %s", dbPath)
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		log.Errorf("[UsageSQLite] Failed to open database %s: %v", dbPath, err)
		return nil, fmt.Errorf("failed to open database: %w", err)
	}

	store := &UsageSQLiteStore{
		db:            db,
		dbPath:        dbPath,
		retentionDays: retentionDays,
	}

	log.Debugf("[UsageSQLite] Initializing database schema...")
	if err := store.initDB(); err != nil {
		db.Close()
		log.Errorf("[UsageSQLite] Failed to initialize database schema: %v", err)
		return nil, err
	}

	log.Infof("[UsageSQLite] Usage database initialized successfully at %s", dbPath)
	return store, nil
}

// initDB initializes the database schema.
func (s *UsageSQLiteStore) initDB() error {
	log.Debugf("[UsageSQLite] Creating database schema...")
	schema := `
	CREATE TABLE IF NOT EXISTS usage_records (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		api_key TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT 'unknown',
		source TEXT,
		auth_index TEXT,
		input_tokens INTEGER DEFAULT 0,
		output_tokens INTEGER DEFAULT 0,
		reasoning_tokens INTEGER DEFAULT 0,
		cached_tokens INTEGER DEFAULT 0,
		total_tokens INTEGER DEFAULT 0,
		failed INTEGER DEFAULT 0,
		requested_at INTEGER NOT NULL,
		created_at INTEGER NOT NULL DEFAULT (strftime('%s', 'now'))
	);

	CREATE INDEX IF NOT EXISTS idx_usage_records_api_key ON usage_records(api_key);
	CREATE INDEX IF NOT EXISTS idx_usage_records_model ON usage_records(model);
	CREATE INDEX IF NOT EXISTS idx_usage_records_auth_index ON usage_records(auth_index);
	CREATE INDEX IF NOT EXISTS idx_usage_records_requested_at ON usage_records(requested_at);
	CREATE INDEX IF NOT EXISTS idx_usage_records_created_at ON usage_records(created_at);

	CREATE TABLE IF NOT EXISTS schema_version (
		version INTEGER PRIMARY KEY
	);

	INSERT OR IGNORE INTO schema_version (version) VALUES (?);
	`

	log.Debugf("[UsageSQLite] Executing schema SQL")
	_, err := s.db.Exec(schema, usageDBSchemaVersion)
	if err != nil {
		log.Errorf("[UsageSQLite] Schema execution failed: %v", err)
		return fmt.Errorf("failed to create schema: %w", err)
	}

	log.Debugf("[UsageSQLite] Schema created successfully")
	return nil
}

// Close closes the database connection.
func (s *UsageSQLiteStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// InsertFromRecord inserts a usage record from coreusage.Record.
func (s *UsageSQLiteStore) InsertFromRecord(record coreusage.Record) error {
	usageRecord := &UsageRecord{
		APIKey:          record.APIKey,
		Model:           record.Model,
		Source:          record.Source,
		AuthIndex:       record.AuthIndex,
		InputTokens:     record.Detail.InputTokens,
		OutputTokens:    record.Detail.OutputTokens,
		ReasoningTokens: record.Detail.ReasoningTokens,
		CachedTokens:    record.Detail.CachedTokens,
		TotalTokens:     record.Detail.TotalTokens,
		Failed:          record.Failed,
		RequestedAt:     record.RequestedAt,
	}

	if usageRecord.TotalTokens == 0 {
		usageRecord.TotalTokens = usageRecord.InputTokens + usageRecord.OutputTokens + usageRecord.ReasoningTokens
	}

	if usageRecord.RequestedAt.IsZero() {
		usageRecord.RequestedAt = time.Now()
	}

	_, err := s.Insert(usageRecord)
	return err
}

// Insert inserts a usage record into the database.
func (s *UsageSQLiteStore) Insert(record *UsageRecord) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if record.RequestedAt.IsZero() {
		record.RequestedAt = time.Now()
	}

	query := `
	INSERT INTO usage_records (
		api_key, model, source, auth_index,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		failed, requested_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`

	result, err := s.db.Exec(query,
		record.APIKey,
		record.Model,
		record.Source,
		record.AuthIndex,
		record.InputTokens,
		record.OutputTokens,
		record.ReasoningTokens,
		record.CachedTokens,
		record.TotalTokens,
		boolToInt(record.Failed),
		record.RequestedAt.Unix(),
	)
	if err != nil {
		return 0, fmt.Errorf("failed to insert usage record: %w", err)
	}

	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to get last insert id: %w", err)
	}

	return id, nil
}

// GetByAPIKey retrieves usage records for a specific API key.
func (s *UsageSQLiteStore) GetByAPIKey(apiKey string, limit, offset int) ([]UsageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 100
	}

	query := `
	SELECT id, api_key, model, source, auth_index,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		failed, requested_at, created_at
	FROM usage_records
	WHERE api_key = ?
	ORDER BY requested_at DESC
	LIMIT ? OFFSET ?
	`

	rows, err := s.db.Query(query, apiKey, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage records: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	return s.scanRows(rows)
}

// GetByDateRange retrieves usage records within a date range.
func (s *UsageSQLiteStore) GetByDateRange(startTime, endTime time.Time, limit, offset int) ([]UsageRecord, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	if limit <= 0 {
		limit = 1000
	}

	query := `
	SELECT id, api_key, model, source, auth_index,
		input_tokens, output_tokens, reasoning_tokens, cached_tokens, total_tokens,
		failed, requested_at, created_at
	FROM usage_records
	WHERE requested_at >= ? AND requested_at <= ?
	ORDER BY requested_at DESC
	LIMIT ? OFFSET ?
	`

	rows, err := s.db.Query(query, startTime.Unix(), endTime.Unix(), limit, offset)
	if err != nil {
		return nil, fmt.Errorf("failed to query usage records: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	return s.scanRows(rows)
}

// GetStatistics retrieves usage statistics for a specific time range.
func (s *UsageSQLiteStore) GetStatistics(startTime, endTime time.Time) (*UsageStatistics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT
		COUNT(*) as total_records,
		COUNT(DISTINCT api_key) as unique_api_keys,
		COUNT(DISTINCT model) as unique_models,
		SUM(total_tokens) as total_tokens,
		SUM(input_tokens) as total_input_tokens,
		SUM(output_tokens) as total_output_tokens,
		SUM(reasoning_tokens) as total_reasoning_tokens,
		SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) as failed_count
	FROM usage_records
	WHERE requested_at >= ? AND requested_at <= ?
	`

	var stats UsageStatistics
	row := s.db.QueryRow(query, startTime.Unix(), endTime.Unix())

	err := row.Scan(
		&stats.TotalRecords,
		&stats.UniqueAPIKeys,
		&stats.UniqueModels,
		&stats.TotalTokens,
		&stats.TotalInputTokens,
		&stats.TotalOutputTokens,
		&stats.TotalReasoningTokens,
		&stats.FailedCount,
	)
	if err != nil {
		return nil, fmt.Errorf("failed to query statistics: %w", err)
	}

	stats.StartTime = startTime
	stats.EndTime = endTime

	return &stats, nil
}

// GetDailyStatistics retrieves daily usage statistics for a specific time range.
func (s *UsageSQLiteStore) GetDailyStatistics(startTime, endTime time.Time) ([]DailyUsageStatistics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT
		date(requested_at, 'unixepoch', 'localtime') as stat_date,
		COUNT(*) as total_records,
		COUNT(DISTINCT api_key) as unique_api_keys,
		COUNT(DISTINCT model) as unique_models,
		SUM(total_tokens) as total_tokens,
		SUM(input_tokens) as total_input_tokens,
		SUM(output_tokens) as total_output_tokens,
		SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) as failed_count
	FROM usage_records
	WHERE requested_at >= ? AND requested_at <= ?
	GROUP BY date(requested_at, 'unixepoch', 'localtime')
	ORDER BY stat_date
	`

	rows, err := s.db.Query(query, startTime.Unix(), endTime.Unix())
	if err != nil {
		return nil, fmt.Errorf("failed to query daily statistics: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var results []DailyUsageStatistics
	for rows.Next() {
		var stat DailyUsageStatistics
		var statDate string
		if err := rows.Scan(&statDate, &stat.TotalRecords, &stat.UniqueAPIKeys,
			&stat.UniqueModels, &stat.TotalTokens, &stat.TotalInputTokens,
			&stat.TotalOutputTokens, &stat.FailedCount); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		stat.Date = statDate
		results = append(results, stat)
	}

	return results, rows.Err()
}

// GetModelStatistics retrieves model-specific usage statistics.
func (s *UsageSQLiteStore) GetModelStatistics(startTime, endTime time.Time) ([]ModelUsageStatistics, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `
	SELECT
		model,
		COUNT(*) as total_records,
		SUM(total_tokens) as total_tokens,
		SUM(input_tokens) as total_input_tokens,
		SUM(output_tokens) as total_output_tokens,
		SUM(CASE WHEN failed = 1 THEN 1 ELSE 0 END) as failed_count
	FROM usage_records
	WHERE requested_at >= ? AND requested_at <= ?
	GROUP BY model
	ORDER BY total_tokens DESC
	`

	rows, err := s.db.Query(query, startTime.Unix(), endTime.Unix())
	if err != nil {
		return nil, fmt.Errorf("failed to query model statistics: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var results []ModelUsageStatistics
	for rows.Next() {
		var stat ModelUsageStatistics
		if err := rows.Scan(&stat.Model, &stat.TotalRecords, &stat.TotalTokens,
			&stat.TotalInputTokens, &stat.TotalOutputTokens, &stat.FailedCount); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		results = append(results, stat)
	}

	return results, rows.Err()
}

// CleanupOldEntries removes entries older than the retention period.
func (s *UsageSQLiteStore) CleanupOldEntries() (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cutoffTime := time.Now().AddDate(0, 0, -s.retentionDays)

	query := `DELETE FROM usage_records WHERE requested_at < ?`
	result, err := s.db.Exec(query, cutoffTime.Unix())
	if err != nil {
		return 0, fmt.Errorf("failed to delete old entries: %w", err)
	}

	affected, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("failed to get affected rows: %w", err)
	}

	if affected > 0 {
		log.Infof("Cleaned up %d old usage records (older than %d days)", affected, s.retentionDays)
	}

	return affected, nil
}

// GetAllAPIKeys returns all unique API keys in the database.
func (s *UsageSQLiteStore) GetAllAPIKeys() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT DISTINCT api_key FROM usage_records ORDER BY api_key`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query API keys: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var keys []string
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		keys = append(keys, key)
	}

	return keys, rows.Err()
}

// GetAllModels returns all unique models in the database.
func (s *UsageSQLiteStore) GetAllModels() ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	query := `SELECT DISTINCT model FROM usage_records ORDER BY model`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, fmt.Errorf("failed to query models: %w", err)
	}
	defer func() {
		_ = rows.Close()
	}()

	var models []string
	for rows.Next() {
		var model string
		if err := rows.Scan(&model); err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}
		models = append(models, model)
	}

	return models, rows.Err()
}

func (s *UsageSQLiteStore) scanRows(rows *sql.Rows) ([]UsageRecord, error) {
	var records []UsageRecord

	for rows.Next() {
		var record UsageRecord
		var requestedAtUnix, createdAtUnix int64

		err := rows.Scan(
			&record.ID,
			&record.APIKey,
			&record.Model,
			&record.Source,
			&record.AuthIndex,
			&record.InputTokens,
			&record.OutputTokens,
			&record.ReasoningTokens,
			&record.CachedTokens,
			&record.TotalTokens,
			&record.Failed,
			&requestedAtUnix,
			&createdAtUnix,
		)
		if err != nil {
			return nil, fmt.Errorf("failed to scan row: %w", err)
		}

		record.RequestedAt = time.Unix(requestedAtUnix, 0)
		record.CreatedAt = time.Unix(createdAtUnix, 0)
		records = append(records, record)
	}

	return records, rows.Err()
}

// UsageStatistics represents aggregated usage statistics.
type UsageStatistics struct {
	StartTime           time.Time `json:"start_time"`
	EndTime             time.Time `json:"end_time"`
	TotalRecords        int64     `json:"total_records"`
	UniqueAPIKeys       int64     `json:"unique_api_keys"`
	UniqueModels        int64     `json:"unique_models"`
	TotalTokens         int64     `json:"total_tokens"`
	TotalInputTokens    int64     `json:"total_input_tokens"`
	TotalOutputTokens   int64     `json:"total_output_tokens"`
	TotalReasoningTokens int64    `json:"total_reasoning_tokens"`
	FailedCount         int64     `json:"failed_count"`
}

// DailyUsageStatistics represents daily aggregated usage statistics.
type DailyUsageStatistics struct {
	Date             string `json:"date"`
	TotalRecords     int64  `json:"total_records"`
	UniqueAPIKeys    int64  `json:"unique_api_keys"`
	UniqueModels     int64  `json:"unique_models"`
	TotalTokens      int64  `json:"total_tokens"`
	TotalInputTokens int64  `json:"total_input_tokens"`
	TotalOutputTokens int64 `json:"total_output_tokens"`
	FailedCount      int64  `json:"failed_count"`
}

// ModelUsageStatistics represents model-specific aggregated usage statistics.
type ModelUsageStatistics struct {
	Model             string `json:"model"`
	TotalRecords      int64  `json:"total_records"`
	TotalTokens       int64  `json:"total_tokens"`
	TotalInputTokens  int64  `json:"total_input_tokens"`
	TotalOutputTokens int64  `json:"total_output_tokens"`
	FailedCount       int64  `json:"failed_count"`
}

// ToJSON converts a UsageRecord to JSON.
func (r *UsageRecord) ToJSON() string {
	data, err := json.Marshal(r)
	if err != nil {
		return "{}"
	}
	return string(data)
}

// boolToInt converts a boolean to SQLite integer (0 or 1).
func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ResolveUsageDBPath resolves the database path, supporting ~ for home directory.
func ResolveUsageDBPath(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("database path is empty")
	}

	// Expand ~ to home directory
	if strings.HasPrefix(path, "~/") || path == "~" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("failed to get home directory: %w", err)
		}
		path = filepath.Join(homeDir, strings.TrimPrefix(path, "~/"))
	}

	// Make absolute if not already
	if !filepath.IsAbs(path) {
		absPath, err := filepath.Abs(path)
		if err != nil {
			return "", fmt.Errorf("failed to resolve absolute path: %w", err)
		}
		path = absPath
	}

	return path, nil
}
