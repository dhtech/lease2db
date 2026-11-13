// SPDX-License-Identifier: GPL-3.0-or-later

// lease2db follows an ISC dhcpd lease file and keeps the history of who
// held which address, and when, in a PostgreSQL database.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"

	"github.com/dhtech/lease2db/internal/leases"
	"github.com/dhtech/lease2db/internal/scopes"
	"github.com/dhtech/lease2db/internal/store"
	"github.com/dhtech/lease2db/internal/track"
)

// version is set by the release build (-ldflags -X main.version=...).
var version = "dev"

type options struct {
	leases   string
	scopes   string
	dsn      string
	interval time.Duration
	metrics  string
}

// counters are what the program has done since it started.
type counters struct {
	blocks, changes, refused, errors, restarts int64
	lastSuccess                                time.Time
	connected                                  bool
	tracked                                    int
	// usage is the leases-per-network part of the metrics, ready made
	usage string
}

func main() {
	var o options
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.StringVar(&o.leases, "leases", "/var/lib/dhcp/dhcpd.leases", "dhcpd's lease file")
	flag.StringVar(&o.scopes, "scopes", "", "dhcpd configuration file with the subnet declarations (optional)")
	flag.StringVar(&o.dsn, "dsn", os.Getenv("LEASE2DB_DSN"),
		"PostgreSQL connection string or URL (default: $LEASE2DB_DSN)")
	flag.DurationVar(&o.interval, "interval", time.Second, "how often to look for new leases")
	flag.StringVar(&o.metrics, "metrics", "",
		"write Prometheus metrics to this file (node exporter textfile collector; optional)")
	flag.Parse()
	if *showVersion {
		fmt.Printf("lease2db %s (%s)\n", version, runtime.Version())
		return
	}
	log.SetFlags(0) // the journal has the time
	if o.dsn == "" {
		log.Fatal("no database given: -dsn or LEASE2DB_DSN")
	}
	if o.interval < 100*time.Millisecond {
		log.Fatal("-interval is too short")
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	log.Printf("lease2db %s: following %s", version, o.leases)

	var c counters
	wait := time.Second
	for ctx.Err() == nil {
		err := run(ctx, o, &c)
		if ctx.Err() != nil {
			break
		}
		// The database went away, or never was there. The lease file
		// stays where it is, so nothing is lost: try again, ever slower.
		c.errors++
		c.connected = false
		writeMetrics(o.metrics, &c)
		log.Printf("%v - again in %s", err, wait)
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
		if wait < time.Minute {
			wait *= 2
		}
	}
	log.Print("stopped")
}

// run connects and follows the file until something fails. Everything it
// needs to know is in the database and the file, so it can be started
// again at any time.
func run(ctx context.Context, o options, c *counters) error {
	db, err := store.Open(ctx, driver, o.dsn)
	if err != nil {
		return fmt.Errorf("database: %w", err)
	}
	defer db.Close()
	schema, err := db.Migrate(ctx)
	if err != nil {
		return fmt.Errorf("database tables: %w", err)
	}
	current, err := db.Current(ctx)
	if err != nil {
		return fmt.Errorf("reading the current leases: %w", err)
	}
	tracker := track.New(db, current)
	follower := &leases.Follower{Path: o.leases}
	c.connected = true
	log.Printf("database ready: schema version %d, %d addresses known", schema, len(current))

	var scopeList []scopes.Scope
	var scopesSeen time.Time
	var lastMetrics time.Time
	ticker := time.NewTicker(o.interval)
	defer ticker.Stop()
	for {
		if o.scopes != "" {
			if info, err := os.Stat(o.scopes); err != nil {
				log.Printf("scopes: %v", err)
			} else if !info.ModTime().Equal(scopesSeen) {
				list, err := loadScopes(ctx, db, o.scopes)
				var unreadable *fileError
				switch {
				case err == nil:
					scopeList = list
					log.Printf("scopes: %d networks from %s", len(list), o.scopes)
				case errors.As(err, &unreadable):
					// a file dhcpd would not take either: keep what we have
					log.Printf("scopes: %v", err)
				default:
					return fmt.Errorf("writing the scopes: %w", err)
				}
				scopesSeen = info.ModTime()
			}
		}

		if err := db.Begin(ctx); err != nil {
			return fmt.Errorf("database: %w", err)
		}
		var applyErr error
		var changes int64
		blocks, restarted, err := follower.Poll(func(l leases.Lease) {
			if applyErr != nil {
				return
			}
			changed, err := tracker.Apply(l)
			if err != nil {
				applyErr = err
			} else if changed {
				changes++
			}
		}, func(line int, err error) {
			c.refused++
			log.Printf("lease file: block at line %d skipped: %v", line, err)
		})
		switch {
		case applyErr != nil:
			db.Rollback()
			return fmt.Errorf("writing a lease: %w", applyErr)
		case err != nil:
			// the file is not there (dhcpd not installed yet, or the
			// moment between its two renames): not the database's fault
			db.Rollback()
			if blocks > 0 {
				// read failed half way: start over from what the
				// database has, rather than guess what was kept
				return fmt.Errorf("lease file: %w", err)
			}
			log.Printf("lease file: %v", err)
		default:
			if err := db.Commit(); err != nil {
				return fmt.Errorf("database: %w", err)
			}
			c.blocks += int64(blocks)
			c.changes += changes
			c.lastSuccess = time.Now()
			if restarted {
				c.restarts++
				log.Printf("lease file read from the top: %d blocks, %d changes", blocks, changes)
			}
		}
		c.tracked = tracker.Len()
		if time.Since(lastMetrics) >= 15*time.Second {
			c.usage = usageMetrics(tracker, scopeList, time.Now())
			writeMetrics(o.metrics, c)
			lastMetrics = time.Now()
		}
		select {
		case <-ctx.Done():
			writeMetrics(o.metrics, c)
			return nil
		case <-ticker.C:
		}
	}
}

// fileError is a scopes file that could not be read or understood - as
// opposed to a database that could not be written.
type fileError struct{ err error }

func (e *fileError) Error() string { return e.err.Error() }

func loadScopes(ctx context.Context, db *store.Store, path string) ([]scopes.Scope, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &fileError{err}
	}
	defer f.Close()
	list, err := scopes.Parse(f)
	if err != nil {
		return nil, &fileError{err}
	}
	if err := db.Begin(ctx); err != nil {
		return nil, err
	}
	if err := db.ReplaceScopes(list); err != nil {
		db.Rollback()
		return nil, err
	}
	return list, db.Commit()
}

// writeMetrics replaces the metrics file in one move, so that the
// collector never reads half of it.
func writeMetrics(path string, c *counters) {
	if path == "" {
		return
	}
	connected := 0
	if c.connected {
		connected = 1
	}
	var last int64
	if !c.lastSuccess.IsZero() {
		last = c.lastSuccess.Unix()
	}
	text := fmt.Sprintf(`# HELP lease2db_connected 1 = connected to the database.
# TYPE lease2db_connected gauge
lease2db_connected %d
# HELP lease2db_last_success_timestamp_seconds When the lease file was last read and the database written.
# TYPE lease2db_last_success_timestamp_seconds gauge
lease2db_last_success_timestamp_seconds %d
# HELP lease2db_blocks_total Lease blocks read from the lease file.
# TYPE lease2db_blocks_total counter
lease2db_blocks_total %d
# HELP lease2db_changes_total Blocks that changed a row in the database.
# TYPE lease2db_changes_total counter
lease2db_changes_total %d
# HELP lease2db_blocks_refused_total Lease blocks that could not be understood.
# TYPE lease2db_blocks_refused_total counter
lease2db_blocks_refused_total %d
# HELP lease2db_errors_total Times the database connection was lost or could not be made.
# TYPE lease2db_errors_total counter
lease2db_errors_total %d
# HELP lease2db_file_restarts_total Times the lease file was read from the top (start, rewrite by dhcpd).
# TYPE lease2db_file_restarts_total counter
lease2db_file_restarts_total %d
# HELP lease2db_addresses Addresses with a row in the history.
# TYPE lease2db_addresses gauge
lease2db_addresses %d
`, connected, last, c.blocks, c.changes, c.refused, c.errors, c.restarts, c.tracked)
	// while there is no connection the lease counts are not known:
	// better absent than a number from before
	if c.connected {
		text += c.usage
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lease2db.")
	if err != nil {
		return
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return
	}
	tmp.Chmod(0o644)
	if tmp.Close() == nil {
		os.Rename(tmp.Name(), path)
	}
}
