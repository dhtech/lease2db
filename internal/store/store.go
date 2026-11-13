// SPDX-License-Identifier: GPL-3.0-or-later

// Package store keeps the lease history in PostgreSQL.
//
// The program owns its tables: it creates them on first start (and
// changes them when a newer version needs to), so the only thing the
// database has to provide is a role that may create tables in the
// database it is pointed at.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/netip"
	"time"

	"github.com/dhtech/lease2db/internal/scopes"
	"github.com/dhtech/lease2db/internal/track"
)

// migrations are applied in order, each in one transaction; the number
// of those applied is kept in lease2db_schema. Never edit one that has
// been released - add another.
var migrations = []string{
	`CREATE TABLE leases (
		id           bigserial PRIMARY KEY,
		ip           inet NOT NULL,
		mac          macaddr,
		first_seen   timestamptz NOT NULL,
		starts       timestamptz NOT NULL,
		ends         timestamptz,
		state        text NOT NULL,
		current      boolean NOT NULL DEFAULT true,
		hostname     text,
		circuit_id   text,
		remote_id    text,
		vendor_class text,
		updated      timestamptz NOT NULL DEFAULT now()
	);
	COMMENT ON TABLE leases IS 'One row for every time a client held an address (lease2db).';
	COMMENT ON COLUMN leases.first_seen IS 'when this client got the address';
	COMMENT ON COLUMN leases.starts IS 'its latest renewal';
	COMMENT ON COLUMN leases.ends IS 'when the lease runs out, or ended; NULL = never';
	COMMENT ON COLUMN leases.state IS 'dhcpd''s binding state: active, free, expired, released, abandoned, ...';
	COMMENT ON COLUMN leases.current IS 'the latest row of its address';
	COMMENT ON COLUMN leases.circuit_id IS 'relay agent option 82: where the client is plugged in';
	CREATE UNIQUE INDEX leases_current ON leases (ip) WHERE current;
	CREATE INDEX leases_ip_time ON leases (ip, first_seen);
	CREATE INDEX leases_mac ON leases (mac);
	CREATE INDEX leases_ends ON leases (ends);

	CREATE TABLE scopes (
		network   cidr PRIMARY KEY,
		name      text NOT NULL,
		vlan      integer,
		gateway   inet,
		addresses integer NOT NULL,
		active    boolean NOT NULL,
		updated   timestamptz NOT NULL DEFAULT now()
	);
	COMMENT ON TABLE scopes IS 'The networks the DHCP server hands addresses out in (lease2db).';
	COMMENT ON COLUMN scopes.addresses IS 'addresses in its dynamic ranges';
	COMMENT ON COLUMN scopes.active IS 'in the server''s configuration now';

	CREATE VIEW leases_active AS
		SELECT l.*, s.network, s.name AS scope, s.vlan
		FROM leases l
		LEFT JOIN scopes s ON s.active AND l.ip << s.network
		WHERE l.current AND l.state = 'active' AND (l.ends IS NULL OR l.ends > now());
	COMMENT ON VIEW leases_active IS 'Who holds which address right now.';

	CREATE VIEW scopes_usage AS
		SELECT s.network, s.name, s.vlan, s.addresses, count(l.id) AS leases
		FROM scopes s
		LEFT JOIN leases l ON l.current AND l.state = 'active'
			AND (l.ends IS NULL OR l.ends > now()) AND l.ip << s.network
		WHERE s.active
		GROUP BY s.network, s.name, s.vlan, s.addresses;
	COMMENT ON VIEW scopes_usage IS 'How full every network is right now.';`,
}

// Store is a connection to the database. Writes happen inside a batch:
// Begin, any number of Insert/Update/ReplaceScopes, then Commit or
// Rollback.
type Store struct {
	db *sql.DB
	tx *sql.Tx
}

// Open connects with the given driver ("pgx") and connection string.
func Open(ctx context.Context, driver, dsn string) (*Store, error) {
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, err
	}
	// one writer, one connection: the batches are transactions
	db.SetMaxOpenConns(2)
	db.SetConnMaxIdleTime(5 * time.Minute)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// Close ends the connection.
func (s *Store) Close() error { return s.db.Close() }

// Migrate creates or updates the tables; it returns the schema version.
func (s *Store) Migrate(ctx context.Context) (int, error) {
	if _, err := s.db.ExecContext(ctx,
		`CREATE TABLE IF NOT EXISTS lease2db_schema (version integer NOT NULL)`); err != nil {
		return 0, fmt.Errorf("schema table: %w", err)
	}
	var version int
	err := s.db.QueryRowContext(ctx, `SELECT version FROM lease2db_schema`).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		if _, err = s.db.ExecContext(ctx, `INSERT INTO lease2db_schema VALUES (0)`); err != nil {
			return 0, err
		}
	} else if err != nil {
		return 0, err
	}
	if version > len(migrations) {
		return version, fmt.Errorf("the database has schema version %d, this program knows %d: it is older than the database",
			version, len(migrations))
	}
	for version < len(migrations) {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return version, err
		}
		if _, err := tx.ExecContext(ctx, migrations[version]); err != nil {
			tx.Rollback()
			return version, fmt.Errorf("schema version %d: %w", version+1, err)
		}
		if _, err := tx.ExecContext(ctx, `UPDATE lease2db_schema SET version = $1`, version+1); err != nil {
			tx.Rollback()
			return version, err
		}
		if err := tx.Commit(); err != nil {
			return version, err
		}
		version++
	}
	return version, nil
}

// Current returns the current row of every address.
func (s *Store) Current(ctx context.Context) ([]track.Row, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, host(ip), coalesce(mac::text, ''), first_seen, starts, ends, state,
		       coalesce(hostname, ''), coalesce(circuit_id, ''), coalesce(remote_id, ''),
		       coalesce(vendor_class, '')
		FROM leases WHERE current`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []track.Row
	for rows.Next() {
		var r track.Row
		var ip string
		var ends sql.NullTime
		if err := rows.Scan(&r.ID, &ip, &r.MAC, &r.FirstSeen, &r.Starts, &ends, &r.State,
			&r.Hostname, &r.CircuitID, &r.RemoteID, &r.VendorClass); err != nil {
			return nil, err
		}
		if r.IP, err = netip.ParseAddr(ip); err != nil {
			return nil, fmt.Errorf("lease %d: %w", r.ID, err)
		}
		r.FirstSeen, r.Starts = r.FirstSeen.UTC(), r.Starts.UTC()
		if ends.Valid {
			r.Ends = ends.Time.UTC()
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// Begin starts a batch.
func (s *Store) Begin(ctx context.Context) error {
	if s.tx != nil {
		return errors.New("a batch is already open")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	s.tx = tx
	return nil
}

// Commit ends the batch and makes it permanent.
func (s *Store) Commit() error {
	if s.tx == nil {
		return nil
	}
	tx := s.tx
	s.tx = nil
	return tx.Commit()
}

// Rollback ends the batch and forgets it.
func (s *Store) Rollback() {
	if s.tx != nil {
		s.tx.Rollback()
		s.tx = nil
	}
}

func nullTime(t time.Time) sql.NullTime { return sql.NullTime{Time: t, Valid: !t.IsZero()} }

// Insert implements track.Store.
func (s *Store) Insert(r *track.Row) (int64, error) {
	if s.tx == nil {
		return 0, errors.New("no batch open")
	}
	// the row that was current steps back; a holding that was still
	// running ends where the new one begins
	if _, err := s.tx.Exec(`
		UPDATE leases SET current = false, updated = now(),
		       ends = CASE WHEN state = 'active' AND (ends IS NULL OR ends > $2) THEN $2 ELSE ends END
		WHERE ip = $1::inet AND current`, r.IP.String(), r.Starts); err != nil {
		return 0, err
	}
	var id int64
	err := s.tx.QueryRow(`
		INSERT INTO leases (ip, mac, first_seen, starts, ends, state, current,
		                    hostname, circuit_id, remote_id, vendor_class)
		VALUES ($1::inet, nullif($2, '')::macaddr, $3, $4, $5, $6, true,
		        nullif($7, ''), nullif($8, ''), nullif($9, ''), nullif($10, ''))
		RETURNING id`,
		r.IP.String(), r.MAC, r.FirstSeen, r.Starts, nullTime(r.Ends), r.State,
		r.Hostname, r.CircuitID, r.RemoteID, r.VendorClass).Scan(&id)
	return id, err
}

// Update implements track.Store.
func (s *Store) Update(r *track.Row) error {
	if s.tx == nil {
		return errors.New("no batch open")
	}
	res, err := s.tx.Exec(`
		UPDATE leases SET starts = $2, ends = $3, state = $4, hostname = nullif($5, ''),
		       circuit_id = nullif($6, ''), remote_id = nullif($7, ''),
		       vendor_class = nullif($8, ''), updated = now()
		WHERE id = $1`,
		r.ID, r.Starts, nullTime(r.Ends), r.State,
		r.Hostname, r.CircuitID, r.RemoteID, r.VendorClass)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n != 1 {
		return fmt.Errorf("lease %d is gone from the database", r.ID)
	}
	return nil
}

// ReplaceScopes makes the given networks the active ones; a network that
// is no longer configured stays in the table, marked inactive, so that
// old leases can still be told where they were.
func (s *Store) ReplaceScopes(list []scopes.Scope) error {
	if s.tx == nil {
		return errors.New("no batch open")
	}
	if _, err := s.tx.Exec(`UPDATE scopes SET active = false, updated = now() WHERE active`); err != nil {
		return err
	}
	for _, sc := range list {
		var vlan sql.NullInt64
		if sc.VLAN != 0 {
			vlan = sql.NullInt64{Int64: int64(sc.VLAN), Valid: true}
		}
		var gateway sql.NullString
		if sc.Gateway.IsValid() {
			gateway = sql.NullString{String: sc.Gateway.String(), Valid: true}
		}
		if _, err := s.tx.Exec(`
			INSERT INTO scopes (network, name, vlan, gateway, addresses, active)
			VALUES ($1::cidr, $2, $3, $4::inet, $5, true)
			ON CONFLICT (network) DO UPDATE SET name = excluded.name, vlan = excluded.vlan,
			       gateway = excluded.gateway, addresses = excluded.addresses,
			       active = true, updated = now()`,
			sc.Network.String(), sc.Name, vlan, gateway, sc.Addresses); err != nil {
			return fmt.Errorf("scope %s: %w", sc.Network, err)
		}
	}
	return nil
}
