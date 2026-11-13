// SPDX-License-Identifier: GPL-3.0-or-later

package scopes

import (
	"strings"
	"testing"
)

// A configuration with the "#@ NET" lines, a shared network, a subnet
// without a name and a host declaration.
const sample = `# generated file

#@ NET SITE@SERVERS-IPMI 10.0.8.0/24 - VLAN 658
subnet 10.0.8.0 netmask 255.255.255.0 {
	range 10.0.8.11 10.0.8.254;
	option routers 10.0.8.1;
}

#@ NET SITE@OFFICE 198.51.100.0/25 - VLAN 951
subnet 198.51.100.0 netmask 255.255.255.128 {
	range 198.51.100.21 198.51.100.59;
	range 198.51.100.61 198.51.100.126;
	option routers 198.51.100.1;
	option domain-name-servers 1.1.1.3, 1.0.0.3;
	max-lease-time 900;
}

#@ SHARED-NET stage
shared-network shared-net-stage {
	#@ NET SITE@STAGE-A 10.9.0.0/28 - VLAN 421
	subnet 10.9.0.0 netmask 255.255.255.240 {
		# resv=20 leaves no address to hand out
		option routers 10.9.0.1;
	}
}

subnet 192.168.7.0 netmask 255.255.255.0 {
	range 192.168.7.10 192.168.7.10;
}

#@ HOST printer.example.net - 198.51.100.5 - MAC 94:dd:f8:9d:ec:ea
host printer.example.net-0 {
	hardware ethernet 94:dd:f8:9d:ec:ea;
	fixed-address 198.51.100.5;
}
`

func TestParse(t *testing.T) {
	got, err := Parse(strings.NewReader(sample))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d scopes, want 4 (a host is not one): %+v", len(got), got)
	}
	a := got[0]
	if a.Network.String() != "10.0.8.0/24" || a.Name != "SITE@SERVERS-IPMI" || a.VLAN != 658 ||
		a.Gateway.String() != "10.0.8.1" || a.Addresses != 244 {
		t.Errorf("first scope: %+v", a)
	}
	// two ranges (a fixed address cut out between them) add up
	if b := got[1]; b.Addresses != 39+66 || b.VLAN != 951 {
		t.Errorf("second scope: %+v", b)
	}
	// inside a shared network, and without a range
	if c := got[2]; c.Name != "SITE@STAGE-A" || c.Addresses != 0 || c.Network.String() != "10.9.0.0/28" {
		t.Errorf("third scope: %+v", c)
	}
	// no "#@ NET" line: no name, and the name before it is not inherited
	if d := got[3]; d.Name != "" || d.VLAN != 0 || d.Addresses != 1 || d.Gateway.IsValid() {
		t.Errorf("fourth scope: %+v", d)
	}
}

func TestBrokenDeclarationsAreErrors(t *testing.T) {
	for _, text := range []string{
		"subnet 10.0.0.0 netmask banana {\n}\n",
		"subnet 10.0.0.0 netmask 255.255.255.0 {\n range 10.0.0.9 10.0.0.2;\n}\n",
		"subnet 10.0.0.0 netmask 255.0.255.0 {\n}\n",
	} {
		if _, err := Parse(strings.NewReader(text)); err == nil {
			t.Errorf("no error for %q", text)
		}
	}
}
