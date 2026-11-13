// SPDX-License-Identifier: GPL-3.0-or-later

// Package track turns the lease file's stream of blocks into the rows of
// the lease history: one row for every time a client held an address,
// from the first time it got it until it gave it up or lost it.
//
// The file repeats itself - dhcpd rewrites it regularly, and after a
// restart it is read from the top - so every block is compared with what
// is already known, and only a block that says something NEW changes
// anything. Reading the same file any number of times gives the same
// rows.
package track

import (
	"net/netip"
	"strings"
	"time"

	"github.com/dhtech/lease2db/internal/leases"
)

// Row is one holding of an address, as stored.
type Row struct {
	ID  int64
	IP  netip.Addr
	MAC string // lower case, "" = unknown
	// FirstSeen is when this client got the address; Starts is its
	// latest renewal; Ends is when the lease runs out or ended (zero =
	// never).
	FirstSeen, Starts, Ends time.Time
	State                   string
	Hostname                string
	// relay agent option 82, as bytes (see leases.Lease)
	CircuitID, RemoteID string
	VendorClass         string
}

// Store is where the rows are kept.
type Store interface {
	// Insert stores r as the CURRENT row of r.IP and returns its id; the
	// row that was current for that address stops being so (and, if it
	// was still active, ends when the new one starts).
	Insert(r *Row) (int64, error)
	// Update rewrites the row r.ID with r's values.
	Update(r *Row) error
}

// Tracker knows the current row of every address.
type Tracker struct {
	store Store
	cur   map[netip.Addr]*Row
}

// New makes a tracker that starts from the rows that are current in the
// store.
func New(store Store, current []Row) *Tracker {
	t := &Tracker{store: store, cur: make(map[netip.Addr]*Row, len(current))}
	for i := range current {
		row := current[i]
		t.cur[row.IP] = &row
	}
	return t
}

// Len is the number of addresses with a known row.
func (t *Tracker) Len() int { return len(t.cur) }

// Active calls fn for every address that is leased at the given time.
func (t *Tracker) Active(now time.Time, fn func(ip netip.Addr, mac string)) {
	for ip, row := range t.cur {
		if row.State == "active" && (row.Ends.IsZero() || row.Ends.After(now)) {
			fn(ip, row.MAC)
		}
	}
}

// Apply takes one block of the lease file. It reports whether the block
// changed anything in the store.
func (t *Tracker) Apply(l leases.Lease) (bool, error) {
	mac := ""
	if l.MAC != nil {
		mac = strings.ToLower(l.MAC.String())
	}
	active := l.State == "active"
	cur := t.cur[l.IP]

	if cur == nil {
		// nothing known about the address: a lease somebody holds, or
		// held - history from before we started reading. A block that
		// names nobody says nothing, except that an address was found
		// in use by a stranger (abandoned).
		if l.Starts.IsZero() || (mac == "" && l.State != "abandoned") {
			return false, nil
		}
		return t.insert(l, mac)
	}

	// Older than what is known: the file read again.
	if l.Starts.Before(cur.Starts) {
		return false, nil
	}
	sameClient := mac == cur.MAC || (mac == "" && !active)

	if active {
		if mac == "" {
			return false, nil
		}
		if sameClient && cur.State != "active" && l.Starts.Equal(cur.Starts) {
			// the lease we already know to be over, read again
			return false, nil
		}
		if sameClient && cur.State == "active" && (cur.Ends.IsZero() || !l.Starts.After(cur.Ends)) {
			// the same holding, renewed (or read again)
			next := *cur
			next.Starts, next.Ends = l.Starts, l.Ends
			fill(&next, l)
			if next == *cur {
				return false, nil
			}
			return t.update(&next)
		}
		// somebody else, or the same client back after its lease was over
		return t.insert(l, mac)
	}

	// The block says the lease is over (free, expired, released, ...).
	if !sameClient {
		// the end of a holding by a client we never saw active here
		return t.insert(l, mac)
	}
	next := *cur
	next.State = l.State
	next.Starts = l.Starts
	if !l.Ends.IsZero() {
		next.Ends = l.Ends
	}
	fill(&next, l)
	if next == *cur {
		return false, nil
	}
	return t.update(&next)
}

// fill takes over what the block knows about the client; a block that
// does not mention something does not erase it.
func fill(r *Row, l leases.Lease) {
	if l.Hostname != "" {
		r.Hostname = l.Hostname
	}
	if l.CircuitID != "" {
		r.CircuitID = l.CircuitID
	}
	if l.RemoteID != "" {
		r.RemoteID = l.RemoteID
	}
	if l.VendorClass != "" {
		r.VendorClass = l.VendorClass
	}
}

func (t *Tracker) insert(l leases.Lease, mac string) (bool, error) {
	row := &Row{IP: l.IP, MAC: mac, FirstSeen: l.Starts, Starts: l.Starts, Ends: l.Ends, State: l.State}
	fill(row, l)
	id, err := t.store.Insert(row)
	if err != nil {
		return false, err
	}
	row.ID = id
	t.cur[l.IP] = row
	return true, nil
}

func (t *Tracker) update(next *Row) (bool, error) {
	if err := t.store.Update(next); err != nil {
		return false, err
	}
	t.cur[next.IP] = next
	return true, nil
}
