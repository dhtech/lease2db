// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"fmt"
	"net/netip"
	"strings"
	"time"

	"github.com/dhtech/lease2db/internal/scopes"
	"github.com/dhtech/lease2db/internal/track"
)

// usageMetrics says, in the Prometheus text format, how many addresses
// are leased right now - in all and in every network - and how many
// clients hold them. This is what a graph of "leases per network" is
// drawn from; the database has the same in its view scopes_usage.
func usageMetrics(tr *track.Tracker, list []scopes.Scope, now time.Time) string {
	perScope := make([]int, len(list))
	clients := map[string]struct{}{}
	leased := 0
	tr.Active(now, func(ip netip.Addr, mac string) {
		leased++
		if mac != "" {
			clients[mac] = struct{}{}
		}
		for i := range list {
			if list[i].Network.Contains(ip) {
				perScope[i]++
			}
		}
	})
	var b strings.Builder
	fmt.Fprintf(&b, `# HELP lease2db_leases_active Addresses leased right now.
# TYPE lease2db_leases_active gauge
lease2db_leases_active %d
# HELP lease2db_clients_active Clients (hardware addresses) holding a lease right now.
# TYPE lease2db_clients_active gauge
lease2db_clients_active %d
`, leased, len(clients))
	if len(list) == 0 {
		return b.String()
	}
	b.WriteString("# HELP lease2db_scope_addresses Addresses in the network's dynamic ranges.\n" +
		"# TYPE lease2db_scope_addresses gauge\n")
	for _, s := range list {
		fmt.Fprintf(&b, "lease2db_scope_addresses{%s} %d\n", scopeLabels(s), s.Addresses)
	}
	b.WriteString("# HELP lease2db_scope_leases_active Addresses of the network leased right now.\n" +
		"# TYPE lease2db_scope_leases_active gauge\n")
	for i, s := range list {
		fmt.Fprintf(&b, "lease2db_scope_leases_active{%s} %d\n", scopeLabels(s), perScope[i])
	}
	return b.String()
}

func scopeLabels(s scopes.Scope) string {
	vlan := ""
	if s.VLAN != 0 {
		vlan = fmt.Sprint(s.VLAN)
	}
	return fmt.Sprintf(`network="%s",name="%s",vlan="%s"`, s.Network, labelValue(s.Name), vlan)
}

// labelValue escapes a label value for the text format.
func labelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}
