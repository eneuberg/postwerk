package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// genStats fills s with days of made-up mail, perDay mails a day, ending at
// end. It writes the rows with SQL directly, a day per transaction, with the
// shape of real traffic: one recipient (every tenth mail two), the required
// target "mailjet" and for every third mail the optional "capture"; about
// 97 % handed off after 0.3–4.3 s (1 % after ten minutes), 2 % failed (half
// of them rejected), 0.3 % unknown, 0.5 % skipped, 2 % with a retry; the
// last hour partly queued. Events as the relay writes them.
func genStats(tb testing.TB, s *Store, end time.Time, days, perDay int) {
	tb.Helper()
	endMS := ms(end)
	for d := days; d >= 1; d-- {
		dayStart := endMS - int64(d)*86_400_000
		err := s.tx(ctx, func(tx *sql.Tx) error {
			stmts := []string{
				`CREATE TEMP TABLE IF NOT EXISTS gen (id TEXT, at INTEGER, r INTEGER)`,
				`DELETE FROM gen`,
				fmt.Sprintf(`INSERT INTO gen WITH RECURSIVE c(i) AS (SELECT 0 UNION ALL SELECT i + 1 FROM c WHERE i < %d)
					SELECT printf('pw_bench%%04d%%08d', %d, i), %d + i * 86400000 / %d + (i * 7919) %% 997, (i * 7919 + %d) %% 1000 FROM c`,
					perDay-1, d, dayStart, perDay, d),
				`INSERT INTO mails (id, received_at, sender, envelope_from, size, raw_purged_at)
					SELECT id, at, CASE r % 5 WHEN 0 THEN 'auth-service' WHEN 1 THEN 'billing' WHEN 4 THEN 'newsletter' ELSE 'shop' END,
					'noreply@example.test', 2048 + r, at + 3000 FROM gen`,
				`INSERT INTO recipients (mail_id, position, address) SELECT id, 0, 'c' || r || '@example.test' FROM gen`,
				`INSERT INTO recipients (mail_id, position, address) SELECT id, 1, 'd' || r || '@example.test' FROM gen WHERE r % 10 = 3`,
				`INSERT INTO deliveries (mail_id, target, attempts, updated_at) SELECT id, 'mailjet', 1 + (r % 50 = 7), at FROM gen`,
				`INSERT INTO deliveries (mail_id, target, attempts, updated_at) SELECT id, 'capture', 1, at FROM gen WHERE r % 3 = 0`,
				// The outcome of each recipient: q is a number from 0 to 999.
				fmt.Sprintf(`INSERT INTO delivery_recipients (mail_id, target, position, status, last_code, updated_at)
					SELECT d.mail_id, d.target, rc.position,
						CASE WHEN g.at > %d AND q < 300 THEN 'queued' WHEN q < 20 THEN 'failed' WHEN q < 23 THEN 'unknown'
							WHEN q < 28 THEN 'skipped' ELSE 'handed_off' END,
						CASE WHEN q < 20 THEN 550 ELSE 250 END,
						g.at + 300 + (q * 37) %% 4000 + CASE WHEN q %% 100 = 99 THEN 600000 ELSE 0 END
					FROM (SELECT g.*, (g.r + rc0.position * 389) %% 1000 AS q, rc0.position AS pos FROM gen g
						JOIN recipients rc0 ON rc0.mail_id = g.id) g
					JOIN deliveries d ON d.mail_id = g.id
					JOIN recipients rc ON rc.mail_id = g.id AND rc.position = g.pos`, endMS-3_600_000),
				`INSERT INTO events (time, mail_id, kind, level, sender) SELECT at, id, 'mail.accepted', 'info', 'shop' FROM gen`,
				`INSERT INTO events (time, mail_id, target, kind, level, attempt, sender)
					SELECT g.at + 200, g.id, d.target, 'delivery.attempt', 'info', 1, 'shop' FROM gen g JOIN deliveries d ON d.mail_id = g.id`,
				`INSERT INTO events (time, mail_id, target, recipient, kind, level, attempt, code, sender)
					SELECT dr.updated_at, dr.mail_id, dr.target, dr.position,
						CASE dr.status WHEN 'handed_off' THEN 'delivery.handed_off' WHEN 'unknown' THEN 'delivery.unknown'
							WHEN 'skipped' THEN 'delivery.skipped'
							WHEN 'failed' THEN CASE WHEN g.r % 2 = 0 THEN 'delivery.rejected' ELSE 'delivery.failed' END END,
						'info', 1, dr.last_code, 'shop'
					FROM gen g JOIN delivery_recipients dr ON dr.mail_id = g.id WHERE dr.status != 'queued'`,
				`INSERT INTO events (time, mail_id, target, recipient, kind, level, attempt, code, sender)
					SELECT at + 1000, id, 'mailjet', 0, 'delivery.retry', 'warn', 1, 451, 'shop' FROM gen WHERE r % 50 = 7`,
			}
			for _, q := range stmts {
				if _, err := tx.ExecContext(ctx, q); err != nil {
					return fmt.Errorf("%w\n%s", err, q)
				}
			}
			return nil
		})
		if err != nil {
			tb.Fatal(err)
		}
	}
	// A few refusals and an outage.
	err := s.tx(ctx, func(tx *sql.Tx) error {
		for i := range 200 {
			at := endMS - int64(i)*3_600_000*7
			if _, err := tx.ExecContext(ctx, `INSERT INTO events (time, kind, level, text, sender)
				VALUES (?, 'ingress.auth_failed', 'warn', ?, 'shop')`, at, "failures="+strconv.Itoa(i%9+1)); err != nil {
				return err
			}
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO events (time, target, kind, level, text) VALUES
			(?, 'mailjet', 'target.down', 'error', 'auth: 535'), (?, 'mailjet', 'target.recovered', 'info', 'auth, down for 20m')`,
			endMS-30*3_600_000, endMS-30*3_600_000+20*60_000)
		return err
	})
	if err != nil {
		tb.Fatal(err)
	}
}

var benchEvents = map[string]string{
	KindDeliveryRetry: "", KindDeliveryRejected: "", KindDeliveryNotAllowed: "",
	"ingress.auth_failed": "failures", "ingress.rejected": "rejected",
}

// TestStatsPerformance measures Stats on a generated database:
//
//	STATS_BENCH_PER_DAY=50000 STATS_BENCH_DAYS=90 go test ./internal/store -run TestStatsPerformance -v -timeout 2h
//
// STATS_BENCH_DIR keeps the database between runs (it is generated once).
func TestStatsPerformance(t *testing.T) {
	perDay, _ := strconv.Atoi(os.Getenv("STATS_BENCH_PER_DAY"))
	if perDay <= 0 {
		t.Skip("set STATS_BENCH_PER_DAY to measure the statistics on a generated database")
	}
	days := 90
	if v, _ := strconv.Atoi(os.Getenv("STATS_BENCH_DAYS")); v > 0 {
		days = v
	}
	dir := os.Getenv("STATS_BENCH_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	end := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(dir, fmt.Sprintf("bench-%d-%d.db", perDay, days))
	if _, err := os.Stat(path); err != nil {
		// Generated under another name and renamed when complete, so an
		// interrupted run starts over.
		tmp := path + ".part"
		for _, p := range []string{tmp, tmp + "-wal", tmp + "-shm", tmp + ".lock"} {
			os.Remove(p)
		}
		g, err := Open(ctx, tmp, Options{InsecureDelete: true})
		if err != nil {
			t.Fatal(err)
		}
		began := time.Now()
		genStats(t, g, end, days, perDay)
		if _, err := g.db.ExecContext(ctx, `ANALYZE`); err != nil {
			t.Fatal(err)
		}
		g.Close()
		if err := os.Rename(tmp, path); err != nil {
			t.Fatal(err)
		}
		os.Remove(tmp + ".lock")
		t.Logf("generated %d days × %d mails in %s", days, perDay, time.Since(began).Round(time.Second))
	}
	s, err := Open(ctx, path, Options{InsecureDelete: true})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if fi, err := os.Stat(path); err == nil {
		t.Logf("database: %d MiB", fi.Size()>>20)
	}
	for _, c := range []struct {
		name   string
		span   time.Duration
		width  time.Duration
		target string
		sender string
	}{
		{"1h", time.Hour, time.Minute, "", ""},
		{"24h", 24 * time.Hour, 30 * time.Minute, "", ""},
		{"7d", 7 * 24 * time.Hour, 3 * time.Hour, "", ""},
		{"30d", 30 * 24 * time.Hour, 24 * time.Hour, "", ""},
		{"30d target=mailjet", 30 * 24 * time.Hour, 24 * time.Hour, "mailjet", ""},
		{"30d sender=billing", 30 * 24 * time.Hour, 24 * time.Hour, "", "billing"},
		{"90d", 90 * 24 * time.Hour, 24 * time.Hour, "", ""},
	} {
		now := end.Add(-time.Minute)
		f := StatsFilter{From: now.Add(-c.span).Truncate(c.width), To: now, Width: c.width, Target: c.target,
			Sender: c.sender, Optional: []string{"capture"}, Events: benchEvents, Now: now, Retained: end.Add(-90 * 24 * time.Hour)}
		// First without the cache, then with the hours that are over in the
		// cache, as on a running system; then the cache after a change to a
		// mail of every hour of the last day.
		s.stats.mu.Lock()
		s.stats.drop(func(statsKey) bool { return true })
		s.stats.mu.Unlock()
		began := time.Now()
		rows, err := s.Stats(ctx, f)
		if err != nil {
			t.Fatal(err)
		}
		cold := time.Since(began)
		began = time.Now()
		if _, err := s.Stats(ctx, f); err != nil {
			t.Fatal(err)
		}
		warm := time.Since(began)
		// Traffic goes on: an event about a mail of each of the last 24
		// hours makes those hours count again.
		if err := s.tx(ctx, func(tx *sql.Tx) error {
			_, err := tx.ExecContext(ctx, `INSERT INTO events (time, mail_id, kind, level)
				SELECT ?, (SELECT id FROM mails WHERE received_at >= h.t ORDER BY received_at LIMIT 1), 'delivery.retry', 'warn'
				FROM (WITH RECURSIVE c(t) AS (SELECT ? UNION ALL SELECT t + 3600000 FROM c WHERE t < ?) SELECT t FROM c) h`,
				ms(end), ms(end.Add(-24*time.Hour)), ms(end.Add(-2*time.Hour)))
			return err
		}); err != nil {
			t.Fatal(err)
		}
		began = time.Now()
		if _, err := s.Stats(ctx, f); err != nil {
			t.Fatal(err)
		}
		changed := time.Since(began)
		// The longest single transaction: the busiest hour.
		began = time.Now()
		if _, err := s.readChunk(ctx, &s.stats, newStatsReader(f, min(f.Width, StatsChunk).Milliseconds()),
			ms(end.Add(-2*time.Hour)), ms(end.Add(-2*time.Hour)), ms(end.Add(-time.Hour))); err != nil {
			t.Fatal(err)
		}
		hour := time.Since(began)
		n := 0
		for _, r := range rows.Mails {
			n += r.Count
		}
		t.Logf("%-20s %9d mails  uncached %8s  cached %6s  after changes in 24 h %6s  one hour %5s", c.name, n,
			cold.Round(time.Millisecond), warm.Round(time.Millisecond), changed.Round(time.Millisecond), hour.Round(time.Millisecond))
	}
	plan(t, s)
}

// plan logs the query plan of the statistics, to check the indexes.
func plan(t *testing.T, s *Store) {
	f := StatsFilter{Optional: []string{"capture"}, Events: benchEvents}
	r := newStatsReader(f, time.Minute.Milliseconds())
	for name, sq := range map[string]struct {
		q    string
		args []any
	}{
		"recipients": {r.recipients, r.rcptArgs(0, 1)}, "events": {r.events, r.eventArgs(0, 1)},
	} {
		rows, err := s.db.QueryContext(ctx, "EXPLAIN QUERY PLAN "+sq.q, sq.args...)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			lines = append(lines, detail)
		}
		rows.Close()
		t.Logf("plan %s:\n  %s", name, strings.Join(lines, "\n  "))
	}
}
