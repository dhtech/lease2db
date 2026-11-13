// SPDX-License-Identifier: GPL-3.0-or-later

// Package scopes reads the networks a dhcpd hands addresses out in from
// its configuration: the subnet declarations with their ranges and
// routers. A network gets a name and a VLAN number when the line before
// its declaration says so, in this form (a comment to dhcpd):
//
//	#@ NET <name> <cidr> - VLAN <n>
//
// A subnet without that line is read too, without a name. include
// statements are not followed: point it at the file that holds the
// subnets.
package scopes

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
)

// Scope is one subnet declaration.
type Scope struct {
	Network netip.Prefix
	Name    string
	VLAN    int        // 0 = not given
	Gateway netip.Addr // the zero Addr = none
	// Addresses is the number of addresses in its dynamic ranges.
	Addresses int
}

// Parse reads subnet declarations; shared-network blocks around them and
// host declarations are passed over.
func Parse(r io.Reader) ([]Scope, error) {
	var out []Scope
	var cur *Scope
	name, vlan := "", 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		text := strings.TrimSpace(sc.Text())
		fields := strings.Fields(strings.TrimSuffix(text, ";"))
		switch {
		case strings.HasPrefix(text, "#@ NET "):
			// #@ NET OFFICE 198.51.100.0/25 - VLAN 950
			name, vlan = "", 0
			if len(fields) >= 3 {
				name = fields[2]
			}
			if len(fields) >= 7 && fields[5] == "VLAN" {
				vlan, _ = strconv.Atoi(fields[6])
			}
		case len(fields) == 5 && fields[0] == "subnet" && fields[2] == "netmask" && fields[4] == "{":
			addr, err := netip.ParseAddr(fields[1])
			mask := net.ParseIP(fields[3]).To4()
			if err != nil || mask == nil {
				return nil, fmt.Errorf("line %d: %q is not a subnet", lineNo, text)
			}
			ones, bits := net.IPMask(mask).Size()
			if bits == 0 {
				return nil, fmt.Errorf("line %d: %q is not a netmask", lineNo, fields[3])
			}
			cur = &Scope{Network: netip.PrefixFrom(addr, ones).Masked(), Name: name, VLAN: vlan}
			name, vlan = "", 0
		case cur != nil && len(fields) == 3 && fields[0] == "range":
			from, err1 := netip.ParseAddr(fields[1])
			to, err2 := netip.ParseAddr(fields[2])
			if err1 != nil || err2 != nil || !from.Is4() || !to.Is4() || to.Less(from) {
				return nil, fmt.Errorf("line %d: %q is not a range", lineNo, text)
			}
			cur.Addresses += int(v4(to)-v4(from)) + 1
		case cur != nil && len(fields) == 3 && fields[0] == "option" && fields[1] == "routers":
			if gw, err := netip.ParseAddr(strings.TrimSuffix(fields[2], ",")); err == nil {
				cur.Gateway = gw
			}
		case cur != nil && text == "}":
			out = append(out, *cur)
			cur = nil
		}
	}
	return out, sc.Err()
}

func v4(a netip.Addr) uint32 {
	b := a.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}
