// SPDX-License-Identifier: GPL-3.0-or-later

package store

// These tests need a real PostgreSQL: they run when LEASE2DB_TEST_DSN
// names a database the test may fill and empty (CI starts one), and are
// skipped otherwise.

import (
	"context"
	"database/sql"
	"net"
	"net/netip"
	"os"
	"strings"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/dhtech/lease2db/internal/leases"
	"github.com/dhtech/lease2db/internal/scopes"
	"github.com/dhtech/lease2db/internal/track"
)

func open(t *testing.T) (*Store, *sql.DB) {
	t.Helper()
	dsn := os.Getenv("LEASE2DB_TEST_DSN")
	if dsn == "" {
		t.Skip("LEASE2DB_TEST_DSN not set: no database to test against")
	}
	ctx := context.Background()
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { raw.Close() })
	if _, err := raw.ExecContext(ctx, `DROP VIEW IF EXISTS leases_totals, scopes_usage, leases_active, leases_history;
		DROP FUNCTION IF EXISTS lease2db_show(bytea);
		DROP TABLE IF EXISTS leases, scopes, lease2db_schema`); err != nil {
		t.Fatal(err)
	}
	s, err := Open(ctx, "pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if v, err := s.Migrate(ctx); err != nil || v != len(migrations) {
		t.Fatalf("migrate: version %d, %v", v, err)
	}
	return s, raw
}

var t0 = time.Now().UTC().Truncate(time.Second)

func block(ip, mac, state string, startMin, endMin int) leases.Lease {
	l := leases.Lease{IP: netip.MustParseAddr(ip), State: state,
		Starts: t0.Add(time.Duration(startMin) * time.Minute),
		Ends:   t0.Add(time.Duration(endMin) * time.Minute)}
	if mac != "" {
		l.MAC, _ = net.ParseMAC(mac)
	}
	return l
}

func batch(t *testing.T, s *Store, tr *track.Tracker, blocks ...leases.Lease) int {
	t.Helper()
	ctx := context.Background()
	if err := s.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, b := range blocks {
		did, err := tr.Apply(b)
		if err != nil {
			s.Rollback()
			t.Fatal(err)
		}
		if did {
			changed++
		}
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	return changed
}

func count(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return n
}

func TestMigrateTwiceIsHarmless(t *testing.T) {
	s, _ := open(t)
	if v, err := s.Migrate(context.Background()); err != nil || v != len(migrations) {
		t.Fatalf("second migrate: version %d, %v", v, err)
	}
}

func TestTheHistoryInTheDatabase(t *testing.T) {
	s, db := open(t)
	ctx := context.Background()
	const alice, bob = "00:00:5e:00:53:01", "00:00:5e:00:53:02"

	if err := s.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceScopes([]scopes.Scope{
		{Network: netip.MustParsePrefix("192.0.2.0/24"), Name: "OFFICE", VLAN: 10,
			Gateway: netip.MustParseAddr("192.0.2.1"), Addresses: 200},
		{Network: netip.MustParsePrefix("198.51.100.0/24"), Addresses: 100},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}

	tr := track.New(s, nil)
	first := block("192.0.2.10", alice, "active", -30, 30)
	first.Hostname = "laptop"
	first.CircuitID = "port-7"
	journal := []leases.Lease{
		first,
		block("192.0.2.10", alice, "active", -10, 50), // renewal
		block("192.0.2.11", bob, "active", -20, 40),
		block("192.0.2.11", bob, "released", -20, -5),
		block("192.0.2.12", bob, "active", -4, 56),
		block("192.0.2.10", bob, "active", -2, 58), // takes alice's address
	}
	// relay information that is not text: bytes, shown as hex
	journal[4].CircuitID = "\x00\x04\x00\x64\x01\x0c"
	journal[4].RemoteID = "aa:bb:cc:dd:ee:ff" // text that looks like hex stays text
	if n := batch(t, s, tr, journal...); n != 6 {
		t.Fatalf("%d changes, want 6", n)
	}
	if n := count(t, db, `SELECT count(*) FROM leases`); n != 4 {
		t.Fatalf("%d rows, want 4 (a renewal is not a new row)", n)
	}
	// one current row per address, enforced by the database
	if n := count(t, db, `SELECT count(*) FROM leases WHERE current`); n != 3 {
		t.Fatalf("%d current rows, want 3", n)
	}
	// alice's holding ended where bob's began, and kept its details
	var ends time.Time
	var host, circuit string
	if err := db.QueryRow(`SELECT ends, hostname, circuit_id_text FROM leases_history
		WHERE ip = '192.0.2.10' AND mac = $1 AND NOT active`, alice).Scan(&ends, &host, &circuit); err != nil {
		t.Fatal(err)
	}
	if !ends.Equal(t0.Add(-2*time.Minute)) || host != "laptop" || circuit != "port-7" {
		t.Errorf("alice's row: ends %v host %q circuit %q", ends, host, circuit)
	}
	// who has what now: bob on two addresses, with the scope's name
	if n := count(t, db, `SELECT count(*) FROM leases_active WHERE mac = $1 AND scope = 'OFFICE' AND vlan = 10`, bob); n != 2 {
		t.Errorf("%d active leases of bob in OFFICE, want 2", n)
	}
	if n := count(t, db, `SELECT leases FROM scopes_usage WHERE name = 'OFFICE'`); n != 2 {
		t.Errorf("scopes_usage says %d leases in OFFICE, want 2", n)
	}
	if n := count(t, db, `SELECT leases FROM scopes_usage WHERE network = '198.51.100.0/24'`); n != 0 {
		t.Errorf("scopes_usage says %d leases in the empty network", n)
	}
	var percent float64
	var gateway string
	if err := db.QueryRow(`SELECT percent_used, host(gateway) FROM scopes_usage WHERE name = 'OFFICE'`).Scan(&percent, &gateway); err != nil || percent != 1.0 || gateway != "192.0.2.1" {
		t.Errorf("scopes_usage of OFFICE: %v percent, gateway %q, %v", percent, gateway, err)
	}
	// where a client is plugged in: bytes in the table, readable in the view
	var shown, remote string
	var rawCircuit []byte
	if err := db.QueryRow(`SELECT circuit_id, circuit_id_text, remote_id_text FROM leases_active
		WHERE ip = '192.0.2.12'`).Scan(&rawCircuit, &shown, &remote); err != nil {
		t.Fatal(err)
	}
	if string(rawCircuit) != "\x00\x04\x00\x64\x01\x0c" || shown != "00:04:00:64:01:0c" || remote != "aa:bb:cc:dd:ee:ff" {
		t.Errorf("relay information: raw %x, shown %q, remote %q", rawCircuit, shown, remote)
	}
	var activeLeases, activeClients, clientsEver int
	if err := db.QueryRow(`SELECT active_leases, active_clients, clients_ever FROM leases_totals`).Scan(
		&activeLeases, &activeClients, &clientsEver); err != nil || activeLeases != 2 || activeClients != 1 || clientsEver != 2 {
		t.Errorf("totals: %d leases, %d clients now, %d ever, %v", activeLeases, activeClients, clientsEver, err)
	}

	// a restart: what Current returns is where the tracker goes on, and
	// the same journal changes nothing
	current, err := s.Current(ctx)
	if err != nil || len(current) != 3 {
		t.Fatalf("current: %d rows, %v", len(current), err)
	}
	if n := batch(t, s, track.New(s, current), journal...); n != 0 {
		t.Errorf("the journal read again changed %d rows", n)
	}
	if n := count(t, db, `SELECT count(*) FROM leases`); n != 4 {
		t.Errorf("%d rows after reading again, want 4", n)
	}

	// a network that left the configuration stays, inactive
	if err := s.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceScopes([]scopes.Scope{
		{Network: netip.MustParsePrefix("192.0.2.0/24"), Name: "OFFICE-NEW", Addresses: 150}}); err != nil {
		t.Fatal(err)
	}
	if err := s.Commit(); err != nil {
		t.Fatal(err)
	}
	if n := count(t, db, `SELECT count(*) FROM scopes WHERE active`); n != 1 {
		t.Errorf("%d active scopes, want 1", n)
	}
	if n := count(t, db, `SELECT count(*) FROM scopes`); n != 2 {
		t.Errorf("%d scopes kept, want 2", n)
	}
	if n := count(t, db, `SELECT addresses FROM scopes WHERE name = 'OFFICE-NEW' AND vlan IS NULL AND gateway IS NULL`); n != 150 {
		t.Errorf("the changed scope: addresses %d", n)
	}
}

func TestARolledBackBatchLeavesNothing(t *testing.T) {
	s, db := open(t)
	ctx := context.Background()
	tr := track.New(s, nil)
	if err := s.Begin(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tr.Apply(block("192.0.2.10", "00:00:5e:00:53:01", "active", 0, 60)); err != nil {
		t.Fatal(err)
	}
	s.Rollback()
	if n := count(t, db, `SELECT count(*) FROM leases`); n != 0 {
		t.Fatalf("%d rows after a rollback", n)
	}
	// writing outside a batch is refused, not done half
	if _, err := s.Insert(&track.Row{IP: netip.MustParseAddr("192.0.2.10")}); err == nil ||
		!strings.Contains(err.Error(), "no batch") {
		t.Fatalf("insert outside a batch: %v", err)
	}
}

func TestANewerDatabaseIsRefused(t *testing.T) {
	s, db := open(t)
	if _, err := db.Exec(`UPDATE lease2db_schema SET version = 999`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Migrate(context.Background()); err == nil {
		t.Fatal("an older program took a newer database")
	}
}
