package integrity

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// defaultPassRetention prunes pass rows after 30 days.
	defaultPassRetention = 30 * 24 * time.Hour
	// defaultFailRetention drops whole partitions only once older than ~1 year,
	// so failures survive for forensics.
	defaultFailRetention = 365 * 24 * time.Hour
	// partitionLookahead is how many months ahead of the current month the
	// Maintainer keeps partitions provisioned.
	partitionLookahead = 2
	// maintainInterval is the housekeeping cadence.
	maintainInterval = 24 * time.Hour
)

// partitionMonthRe matches the monthly child partitions (integrity_audits_YYYY_MM)
// and deliberately excludes integrity_audits_default.
var partitionMonthRe = regexp.MustCompile(`^integrity_audits_(\d{4})_(\d{2})$`)

// Maintainer keeps integrity_audits' monthly partitions provisioned ahead of
// time and enforces retention (pass rows pruned after passRet; whole partitions
// dropped once older than failRet). The committed partitions stop at
// 2026-07-01, so create-ahead is required for inserts to keep working.
type Maintainer struct {
	pool    *pgxpool.Pool
	passRet time.Duration
	failRet time.Duration
	now     func() time.Time
	log     *slog.Logger
}

// MaintainerOption tunes a Maintainer (primarily for tests).
type MaintainerOption func(*Maintainer)

// WithMaintClock overrides the Maintainer's time source.
func WithMaintClock(now func() time.Time) MaintainerOption {
	return func(m *Maintainer) {
		if now != nil {
			m.now = now
		}
	}
}

// NewMaintainer builds a Maintainer. Non-positive retentions take the defaults.
func NewMaintainer(pool *pgxpool.Pool, passRet, failRet time.Duration, log *slog.Logger, opts ...MaintainerOption) *Maintainer {
	if passRet <= 0 {
		passRet = defaultPassRetention
	}
	if failRet <= 0 {
		failRet = defaultFailRetention
	}
	if log == nil {
		log = slog.Default()
	}
	m := &Maintainer{pool: pool, passRet: passRet, failRet: failRet, now: time.Now, log: log}
	for _, o := range opts {
		o(m)
	}
	return m
}

// Run performs one maintenance cycle immediately (so a fresh node provisions the
// next partition before the scheduler inserts into it), then every 24h until
// ctx is cancelled.
func (m *Maintainer) Run(ctx context.Context) {
	m.maintain(ctx)
	t := time.NewTicker(maintainInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.maintain(ctx)
		}
	}
}

func (m *Maintainer) maintain(ctx context.Context) {
	if err := m.ensurePartitions(ctx); err != nil {
		m.log.WarnContext(ctx, "integrity: ensure partitions", "err", err)
	}
	// audit_log is also monthly-partitioned (0003_partitions.sql); M9 provisions
	// its partitions ahead too, but never prunes it — operator-action history is
	// retained for years (legal hold), so there is no prune/drop pass for it.
	if err := m.ensureMonthlyPartitions(ctx, "audit_log"); err != nil {
		m.log.WarnContext(ctx, "audit_log: ensure partitions", "err", err)
	}
	if err := m.prunePasses(ctx); err != nil {
		m.log.WarnContext(ctx, "integrity: prune passes", "err", err)
	}
	if err := m.dropAgedPartitions(ctx); err != nil {
		m.log.WarnContext(ctx, "integrity: drop aged partitions", "err", err)
	}
}

// ensureMonthlyPartitions creates the current month plus partitionLookahead
// months ahead of parent, idempotently. Names are month-derived (no user input)
// so the DDL is assembled with fmt.Sprintf; bounds are explicit-UTC to match
// 0003_partitions.sql. Used for both integrity_audits and audit_log (M9).
func (m *Maintainer) ensureMonthlyPartitions(ctx context.Context, parent string) error {
	base := monthStart(m.now())
	for i := 0; i <= partitionLookahead; i++ {
		start := base.AddDate(0, i, 0)
		end := start.AddDate(0, 1, 0)
		name := fmt.Sprintf("%s_%04d_%02d", parent, start.Year(), int(start.Month()))
		ddl := fmt.Sprintf(
			`CREATE TABLE IF NOT EXISTS %s PARTITION OF %s FOR VALUES FROM ('%s') TO ('%s')`,
			name, parent, boundLiteral(start), boundLiteral(end))
		if _, err := m.pool.Exec(ctx, ddl); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
	}
	return nil
}

// ensurePartitions provisions integrity_audits' partitions (the retention test
// drives this directly).
func (m *Maintainer) ensurePartitions(ctx context.Context) error {
	return m.ensureMonthlyPartitions(ctx, "integrity_audits")
}

// prunePasses deletes pass rows older than passRet. Failures are retained.
func (m *Maintainer) prunePasses(ctx context.Context) error {
	_, err := m.pool.Exec(ctx,
		`DELETE FROM integrity_audits WHERE result = 'pass' AND audited_at < $1`,
		m.now().Add(-m.passRet))
	return err
}

// dropAgedPartitions drops monthly partitions whose entire range is older than
// failRet, reclaiming long-retained failures in one metadata operation. The
// integrity_audits_default catch-all is left untouched.
func (m *Maintainer) dropAgedPartitions(ctx context.Context) error {
	rows, err := m.pool.Query(ctx, `
		SELECT c.relname
		FROM pg_inherits i
		JOIN pg_class c ON c.oid = i.inhrelid
		JOIN pg_class p ON p.oid = i.inhparent
		WHERE p.relname = 'integrity_audits'`)
	if err != nil {
		return fmt.Errorf("list partitions: %w", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return fmt.Errorf("scan partition: %w", err)
		}
		names = append(names, n)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	cutoff := m.now().Add(-m.failRet)
	for _, name := range names {
		match := partitionMonthRe.FindStringSubmatch(name)
		if match == nil {
			continue // skip integrity_audits_default and any non-monthly partition
		}
		year, _ := strconv.Atoi(match[1])
		month, _ := strconv.Atoi(match[2])
		upper := time.Date(year, time.Month(month), 1, 0, 0, 0, 0, time.UTC).AddDate(0, 1, 0)
		if upper.Before(cutoff) {
			if _, err := m.pool.Exec(ctx, fmt.Sprintf(`DROP TABLE IF EXISTS %s`, name)); err != nil {
				return fmt.Errorf("drop %s: %w", name, err)
			}
			m.log.InfoContext(ctx, "integrity: dropped aged partition", "partition", name)
		}
	}
	return nil
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

func boundLiteral(t time.Time) string {
	return t.UTC().Format("2006-01-02") + " 00:00:00+00"
}
