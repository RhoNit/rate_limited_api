package limiter

// MySQLLimiter is the production database backend.
//
// Concurrency model — why a lock row?
//
// A naïve "SELECT COUNT(*) … INSERT" sequence in separate statements is
// not atomic: two concurrent goroutines can both read count=4 (under a
// 5-request cap) and both proceed to insert, yielding count=6.
//
// We solve this with a per-user "lock row" in rate_limit_user_lock.
// Inside a transaction every request does:
//   1. INSERT IGNORE … lock row (auto-committed, idempotent)
//   2. SELECT … FOR UPDATE on the lock row — InnoDB serialises all
//      transactions that want to touch the same row.
//   3. DELETE expired timestamps, COUNT active, INSERT if under cap.
//   4. COMMIT — releasing the row lock.
//
// Different users never contend because their lock rows are different
// primary-key values. Same user serialises, so the window count is
// always accurate even under thousands of concurrent requests.
//
// Limitations (noted in README):
//   - Each request makes 2–4 round-trips to MySQL; keep the DB close.
//   - The rate_limit_log table grows until rows are evicted. A
//     periodic cleanup job should also prune rows for long-idle users.

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	_ "github.com/go-sql-driver/mysql" // register "mysql" driver
)

// Schema is run on startup (CREATE TABLE IF NOT EXISTS is idempotent).
const schema = `
CREATE TABLE IF NOT EXISTS rate_limit_user_lock (
    user_id VARCHAR(128) NOT NULL,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS rate_limit_log (
    id           BIGINT      NOT NULL AUTO_INCREMENT,
    user_id      VARCHAR(128) NOT NULL,
    requested_at DATETIME(3) NOT NULL,
    PRIMARY KEY (id),
    INDEX idx_user_time (user_id, requested_at)
) ENGINE=InnoDB;

CREATE TABLE IF NOT EXISTS rate_limit_stats (
    user_id           VARCHAR(128) NOT NULL,
    total_requests    BIGINT       NOT NULL DEFAULT 0,
    allowed_requests  BIGINT       NOT NULL DEFAULT 0,
    rejected_requests BIGINT       NOT NULL DEFAULT 0,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB;
`

const upsertStats = `
INSERT INTO rate_limit_stats (user_id, total_requests, allowed_requests, rejected_requests)
VALUES (?, ?, ?, ?)
ON DUPLICATE KEY UPDATE
    total_requests    = total_requests    + VALUES(total_requests),
    allowed_requests  = allowed_requests  + VALUES(allowed_requests),
    rejected_requests = rejected_requests + VALUES(rejected_requests)`

// MySQLLimiter implements the Limiter interface with MySQL as storage.
type MySQLLimiter struct {
	db     *sql.DB
	limit  int
	window time.Duration
}

// NewMySQL opens the database, runs migrations, and returns a ready limiter.
func NewMySQL(ctx context.Context, dsn string, limit int, window time.Duration) (*MySQLLimiter, error) {
	if limit <= 0 || window <= 0 {
		return nil, fmt.Errorf("limit and window must be positive")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(25)
	db.SetMaxIdleConns(10)
	db.SetConnMaxLifetime(5 * time.Minute)

	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping db: %w", err)
	}
	m := &MySQLLimiter{db: db, limit: limit, window: window}
	if err := m.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return m, nil
}

func (m *MySQLLimiter) migrate(ctx context.Context) error {
	// Split schema into individual statements — sql.Exec doesn't support multi-stmt.
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS rate_limit_user_lock (
            user_id VARCHAR(128) NOT NULL,
            PRIMARY KEY (user_id)
        ) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS rate_limit_log (
            id           BIGINT       NOT NULL AUTO_INCREMENT,
            user_id      VARCHAR(128) NOT NULL,
            requested_at DATETIME(3)  NOT NULL,
            PRIMARY KEY (id),
            INDEX idx_user_time (user_id, requested_at)
        ) ENGINE=InnoDB`,
		`CREATE TABLE IF NOT EXISTS rate_limit_stats (
            user_id           VARCHAR(128) NOT NULL,
            total_requests    BIGINT       NOT NULL DEFAULT 0,
            allowed_requests  BIGINT       NOT NULL DEFAULT 0,
            rejected_requests BIGINT       NOT NULL DEFAULT 0,
            PRIMARY KEY (user_id)
        ) ENGINE=InnoDB`,
	}
	for _, s := range stmts {
		if _, err := m.db.ExecContext(ctx, s); err != nil {
			return err
		}
	}
	return nil
}

func (m *MySQLLimiter) Limit() int            { return m.limit }
func (m *MySQLLimiter) Window() time.Duration { return m.window }
func (m *MySQLLimiter) Close() error          { return m.db.Close() }

// CheckAndRecord is the critical section: it atomically decides whether
// the user is under the cap and records the request if so.
func (m *MySQLLimiter) CheckAndRecord(ctx context.Context, userID string) (Result, error) {
	// Step 1: ensure the per-user lock row exists. This is auto-committed
	// and idempotent — safe to run outside the transaction.
	if _, err := m.db.ExecContext(ctx,
		`INSERT IGNORE INTO rate_limit_user_lock (user_id) VALUES (?)`, userID); err != nil {
		return Result{}, fmt.Errorf("ensure lock row: %w", err)
	}

	// Step 2: open a transaction and grab an exclusive lock on the row.
	// Every other goroutine (or process) wanting to touch this user will
	// block here until we COMMIT or ROLLBACK.
	tx, err := m.db.BeginTx(ctx, nil)
	if err != nil {
		return Result{}, fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	var lockHolder string
	if err := tx.QueryRowContext(ctx,
		`SELECT user_id FROM rate_limit_user_lock WHERE user_id = ? FOR UPDATE`, userID,
	).Scan(&lockHolder); err != nil {
		return Result{}, fmt.Errorf("acquire row lock: %w", err)
	}

	now := time.Now().UTC()
	windowStart := now.Add(-m.window)

	// Step 3: evict timestamps that have fallen outside the sliding window.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM rate_limit_log WHERE user_id = ? AND requested_at < ?`,
		userID, windowStart); err != nil {
		return Result{}, fmt.Errorf("evict: %w", err)
	}

	// Step 4: count remaining active timestamps and find the oldest one
	// (needed to compute retry_after when the limit is hit).
	var count int
	var oldest sql.NullTime
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*), MIN(requested_at) FROM rate_limit_log WHERE user_id = ?`,
		userID).Scan(&count, &oldest); err != nil {
		return Result{}, fmt.Errorf("count: %w", err)
	}

	// Step 5: decision.
	if count >= m.limit {
		// Record the rejection in stats, then commit to release the lock.
		if _, err := tx.ExecContext(ctx, upsertStats, userID, 1, 0, 1); err != nil {
			return Result{}, fmt.Errorf("upsert rejected stats: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return Result{}, fmt.Errorf("commit rejected: %w", err)
		}

		var retryAfter time.Duration
		var resetAt time.Time
		if oldest.Valid {
			resetAt = oldest.Time.Add(m.window)
			retryAfter = resetAt.Sub(now)
			if retryAfter < 0 {
				retryAfter = 0
			}
		}
		return Result{
			Allowed:    false,
			Remaining:  0,
			Limit:      m.limit,
			RetryAfter: retryAfter,
			ResetAt:    resetAt,
		}, nil
	}

	// Step 6: record the request and update allowed stats.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO rate_limit_log (user_id, requested_at) VALUES (?, ?)`, userID, now); err != nil {
		return Result{}, fmt.Errorf("insert log: %w", err)
	}
	if _, err := tx.ExecContext(ctx, upsertStats, userID, 1, 1, 0); err != nil {
		return Result{}, fmt.Errorf("upsert allowed stats: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return Result{}, fmt.Errorf("commit allowed: %w", err)
	}

	newCount := count + 1
	resetAt := now.Add(m.window)
	if oldest.Valid {
		resetAt = oldest.Time.Add(m.window) // true oldest is before now
	}
	return Result{
		Allowed:    true,
		Remaining:  m.limit - newCount,
		Limit:      m.limit,
		RetryAfter: 0,
		ResetAt:    resetAt,
	}, nil
}

func (m *MySQLLimiter) Stats(ctx context.Context, userID string) (UserStats, error) {
	windowStart := time.Now().UTC().Add(-m.window)

	// Count active window entries for "current_window_count".
	var current int
	if err := m.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM rate_limit_log WHERE user_id = ? AND requested_at >= ?`,
		userID, windowStart).Scan(&current); err != nil {
		return UserStats{}, err
	}

	var s UserStats
	s.UserID = userID
	s.Limit = m.limit
	s.WindowSeconds = int(m.window.Seconds())
	s.CurrentWindowCount = current

	row := m.db.QueryRowContext(ctx,
		`SELECT total_requests, allowed_requests, rejected_requests
		   FROM rate_limit_stats WHERE user_id = ?`, userID)
	err := row.Scan(&s.TotalRequests, &s.AllowedRequests, &s.RejectedRequests)
	if err == sql.ErrNoRows {
		return s, nil
	}
	return s, err
}

func (m *MySQLLimiter) AllStats(ctx context.Context) ([]UserStats, error) {
	rows, err := m.db.QueryContext(ctx,
		`SELECT user_id FROM rate_limit_stats ORDER BY user_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]UserStats, 0, len(ids))
	for _, id := range ids {
		s, err := m.Stats(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

func (m *MySQLLimiter) Reset(ctx context.Context, userID string) error {
	if userID == "" {
		if _, err := m.db.ExecContext(ctx, `DELETE FROM rate_limit_log`); err != nil {
			return err
		}
		if _, err := m.db.ExecContext(ctx, `DELETE FROM rate_limit_stats`); err != nil {
			return err
		}
		_, err := m.db.ExecContext(ctx, `DELETE FROM rate_limit_user_lock`)
		return err
	}
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rate_limit_log WHERE user_id = ?`, userID); err != nil {
		return err
	}
	if _, err := m.db.ExecContext(ctx,
		`DELETE FROM rate_limit_stats WHERE user_id = ?`, userID); err != nil {
		return err
	}
	_, err := m.db.ExecContext(ctx,
		`DELETE FROM rate_limit_user_lock WHERE user_id = ?`, userID)
	return err
}
