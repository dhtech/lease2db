// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/dhtech/lease2db/internal/leases"
	"github.com/dhtech/lease2db/internal/scopes"
	"github.com/dhtech/lease2db/internal/track"
)

type nowhere struct{ n int64 }

func (s *nowhere) Insert(*track.Row) (int64, error) { s.n++; return s.n, nil }
func (s *nowhere) Update(*track.Row) error          { return nil }

func TestUsageMetrics(t *testing.T) {
	now := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	tr := track.New(&nowhere{}, nil)
	lease := func(ip, mac, state string, endsIn time.Duration) {
		l := leases.Lease{IP: netip.MustParseAddr(ip), State: state,
			Starts: now.Add(-time.Hour), Ends: now.Add(endsIn)}
		l.MAC, _ = net.ParseMAC(mac)
		if _, err := tr.Apply(l); err != nil {
			t.Fatal(err)
		}
	}
	lease("192.0.2.10", "00:00:5e:00:53:01", "active", time.Hour)
	lease("192.0.2.11", "00:00:5e:00:53:02", "active", time.Hour)
	lease("198.51.100.5", "00:00:5e:00:53:02", "active", time.Hour)  // the same client, another network
	lease("192.0.2.12", "00:00:5e:00:53:03", "active", -time.Minute) // ran out
	lease("192.0.2.13", "00:00:5e:00:53:04", "free", -time.Minute)
	lease("203.0.113.9", "00:00:5e:00:53:05", "active", time.Hour) // in no network we know
	list := []scopes.Scope{
		{Network: netip.MustParsePrefix("192.0.2.0/24"), Name: `OFFICE "A"`, VLAN: 10, Addresses: 200},
		{Network: netip.MustParsePrefix("198.51.100.0/24"), Addresses: 100},
		{Network: netip.MustParsePrefix("10.9.0.0/28"), Name: "EMPTY", VLAN: 421},
	}
	got := usageMetrics(tr, list, now)
	for _, want := range []string{
		"lease2db_leases_active 4\n",
		"lease2db_clients_active 3\n",
		`lease2db_scope_addresses{network="192.0.2.0/24",name="OFFICE \"A\"",vlan="10"} 200` + "\n",
		`lease2db_scope_leases_active{network="192.0.2.0/24",name="OFFICE \"A\"",vlan="10"} 2` + "\n",
		`lease2db_scope_leases_active{network="198.51.100.0/24",name="",vlan=""} 1` + "\n",
		`lease2db_scope_leases_active{network="10.9.0.0/28",name="EMPTY",vlan="421"} 0` + "\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// without scopes: the totals only
	if got := usageMetrics(tr, nil, now); strings.Contains(got, "lease2db_scope_") ||
		!strings.Contains(got, "lease2db_leases_active 4\n") {
		t.Errorf("without scopes:\n%s", got)
	}
}
