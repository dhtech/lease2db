// SPDX-License-Identifier: GPL-3.0-or-later

package track

import (
	"errors"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/dhtech/lease2db/internal/leases"
)

// memory is a Store that keeps the rows in a slice, as the database would.
type memory struct {
	rows    []Row
	current map[netip.Addr]int // index into rows
	writes  int
	fail    error
}

func newMemory() *memory { return &memory{current: map[netip.Addr]int{}} }

func (m *memory) Insert(r *Row) (int64, error) {
	if m.fail != nil {
		return 0, m.fail
	}
	m.writes++
	if i, ok := m.current[r.IP]; ok {
		old := &m.rows[i]
		if old.State == "active" && (old.Ends.IsZero() || old.Ends.After(r.Starts)) {
			old.Ends = r.Starts
		}
	}
	row := *r
	row.ID = int64(len(m.rows) + 1)
	m.rows = append(m.rows, row)
	m.current[r.IP] = len(m.rows) - 1
	return row.ID, nil
}

func (m *memory) Update(r *Row) error {
	if m.fail != nil {
		return m.fail
	}
	m.writes++
	m.rows[r.ID-1] = *r
	return nil
}

func (m *memory) currentRows() []Row {
	var out []Row
	for _, i := range m.current {
		out = append(out, m.rows[i])
	}
	return out
}

var t0 = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

func at(minutes int) time.Time { return t0.Add(time.Duration(minutes) * time.Minute) }

func block(ip, mac, state string, starts, ends int) leases.Lease {
	l := leases.Lease{IP: netip.MustParseAddr(ip), State: state, Starts: at(starts)}
	if ends >= 0 {
		l.Ends = at(ends)
	}
	if mac != "" {
		l.MAC, _ = net.ParseMAC(mac)
	}
	return l
}

const (
	alice = "00:00:5e:00:53:01"
	bob   = "00:00:5e:00:53:02"
)

func apply(t *testing.T, tr *Tracker, blocks ...leases.Lease) int {
	t.Helper()
	changed := 0
	for _, b := range blocks {
		did, err := tr.Apply(b)
		if err != nil {
			t.Fatal(err)
		}
		if did {
			changed++
		}
	}
	return changed
}

func TestRenewalsAreOneRow(t *testing.T) {
	m := newMemory()
	tr := New(m, nil)
	first := block("192.0.2.10", alice, "active", 0, 10)
	first.Hostname = "laptop"
	first.CircuitID = "port-7"
	apply(t, tr, first,
		block("192.0.2.10", alice, "active", 5, 15),
		block("192.0.2.10", alice, "active", 10, 20))
	if len(m.rows) != 1 {
		t.Fatalf("%d rows, want 1: %+v", len(m.rows), m.rows)
	}
	r := m.rows[0]
	if !r.FirstSeen.Equal(at(0)) || !r.Starts.Equal(at(10)) || !r.Ends.Equal(at(20)) || r.State != "active" {
		t.Errorf("row %+v", r)
	}
	// a renewal that does not mention the name or the port keeps them
	if r.Hostname != "laptop" || r.CircuitID != "port-7" {
		t.Errorf("client details lost: %+v", r)
	}
}

func TestTheEndOfALeaseClosesItsRow(t *testing.T) {
	m := newMemory()
	tr := New(m, nil)
	apply(t, tr,
		block("192.0.2.10", alice, "active", 0, 10),
		block("192.0.2.10", alice, "released", 0, 4))
	if len(m.rows) != 1 || m.rows[0].State != "released" || !m.rows[0].Ends.Equal(at(4)) {
		t.Fatalf("rows %+v", m.rows)
	}
	// the same client comes back: a new holding, a new row
	apply(t, tr, block("192.0.2.10", alice, "active", 30, 40))
	if len(m.rows) != 2 || !m.rows[1].FirstSeen.Equal(at(30)) {
		t.Fatalf("rows %+v", m.rows)
	}
	// a free block that names no client ends what is known, too
	apply(t, tr, block("192.0.2.10", "", "free", 30, 40))
	if len(m.rows) != 2 || m.rows[1].State != "free" {
		t.Fatalf("rows %+v", m.rows)
	}
}

func TestAnotherClientGetsItsOwnRow(t *testing.T) {
	m := newMemory()
	tr := New(m, nil)
	apply(t, tr,
		block("192.0.2.10", alice, "active", 0, 60),
		block("192.0.2.10", bob, "active", 20, 80))
	if len(m.rows) != 2 {
		t.Fatalf("rows %+v", m.rows)
	}
	// the first holding ends where the second begins: no two clients on
	// one address at the same time in the history
	if !m.rows[0].Ends.Equal(at(20)) || m.rows[1].MAC != bob {
		t.Errorf("rows %+v", m.rows)
	}
	// a client whose lease ran out and who asks again after a gap
	apply(t, tr, block("192.0.2.10", bob, "active", 200, 260))
	if len(m.rows) != 3 || !m.rows[2].FirstSeen.Equal(at(200)) {
		t.Errorf("rows %+v", m.rows)
	}
}

func TestReadingTheFileAgainChangesNothing(t *testing.T) {
	journal := []leases.Lease{
		block("192.0.2.10", alice, "active", 0, 10),
		block("192.0.2.11", bob, "active", 1, 11),
		block("192.0.2.10", alice, "active", 5, 15),
		block("192.0.2.10", alice, "free", 5, 15),
		block("192.0.2.11", bob, "active", 6, 16),
		block("192.0.2.12", "", "abandoned", 7, 7),
		block("192.0.2.10", bob, "active", 30, 40),
		block("192.0.2.13", "", "free", 0, 1), // names nobody: nothing to record
	}
	m := newMemory()
	tr := New(m, nil)
	apply(t, tr, journal...)
	want := append([]Row(nil), m.rows...)
	if len(want) != 4 {
		t.Fatalf("%d rows, want 4: %+v", len(want), want)
	}
	writes := m.writes
	// the whole journal once more (a restart of ours), then what dhcpd
	// writes when it rewrites the file: one block per address
	if n := apply(t, tr, journal...); n != 0 {
		t.Errorf("reading again changed %d rows", n)
	}
	rewritten := []leases.Lease{
		block("192.0.2.11", bob, "active", 6, 16),
		block("192.0.2.12", "", "abandoned", 7, 7),
		block("192.0.2.10", bob, "active", 30, 40),
	}
	if n := apply(t, tr, rewritten...); n != 0 {
		t.Errorf("the rewritten file changed %d rows", n)
	}
	// and after a restart: the tracker starts from the store's current rows
	restarted := New(m, m.currentRows())
	if n := apply(t, restarted, journal...); n != 0 {
		t.Errorf("after a restart the journal changed %d rows", n)
	}
	if m.writes != writes || len(m.rows) != len(want) {
		t.Fatalf("writes %d -> %d, rows %d -> %d", writes, m.writes, len(want), len(m.rows))
	}
	for i := range want {
		if m.rows[i] != want[i] {
			t.Errorf("row %d changed: %+v -> %+v", i, want[i], m.rows[i])
		}
	}
}

func TestHistoryFromBeforeWeStarted(t *testing.T) {
	m := newMemory()
	tr := New(m, nil)
	// the file's first word about an address is that it is free again:
	// who had it is still worth knowing
	apply(t, tr, block("192.0.2.20", alice, "free", 0, 10))
	if len(m.rows) != 1 || m.rows[0].State != "free" || m.rows[0].MAC != alice {
		t.Fatalf("rows %+v", m.rows)
	}
	// an active block without a hardware address cannot be anybody's
	if n := apply(t, tr, block("192.0.2.21", "", "active", 0, 10)); n != 0 {
		t.Fatalf("recorded a lease of nobody")
	}
}

func TestAStoreThatFailsLeavesTheTrackerAsItWas(t *testing.T) {
	m := newMemory()
	tr := New(m, nil)
	m.fail = errors.New("connection lost")
	if _, err := tr.Apply(block("192.0.2.10", alice, "active", 0, 10)); err == nil {
		t.Fatal("no error")
	}
	if tr.Len() != 0 {
		t.Fatal("the tracker remembers a row the store never took")
	}
	m.fail = nil
	if n := apply(t, tr, block("192.0.2.10", alice, "active", 0, 10)); n != 1 || len(m.rows) != 1 {
		t.Fatalf("after the store came back: changed %d, rows %+v", n, m.rows)
	}
}
