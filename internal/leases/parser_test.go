// SPDX-License-Identifier: GPL-3.0-or-later

package leases

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// What dhcpd 4.4.3 writes: the head of a fresh file, and lease blocks in
// the format of dhcpd.leases(5).
const sample = `# The format of this file is documented in the dhcpd.leases(5) manual page.
# This lease file was written by isc-dhcp-4.4.3-P1

# authoring-byte-order entry is generated, DO NOT DELETE
authoring-byte-order little-endian;

server-duid "\000\001\000\0012\211\025O\274$\021\367\374q";

lease 192.0.2.21 {
  starts 5 2026/11/13 01:30:00;
  ends 5 2026/11/13 03:30:00;
  cltt 5 2026/11/13 01:30:00;
  binding state active;
  next binding state free;
  rewind binding state free;
  hardware ethernet 3C:22:fb:aa:bb:01;
  uid "\001<\"\373\252\273\001";
  set vendor-class-identifier = "MSFT 5.0";
  option agent.circuit-id "ge-0/0/12.0:office";
  option agent.remote-id 0:6:0:1e:be:ef:0:1;
  client-hostname "tech-laptop \"7\"";
}
failover peer "dhcp" state {
  my state normal at 5 2026/11/13 01:00:00;
}
lease 192.0.2.22 {
  starts 5 2026/11/13 01:00:00;
  ends 5 2026/11/13 01:20:00;
  tstp 5 2026/11/13 01:20:00;
  binding state free;
  hardware ethernet 3c:22:fb:aa:bb:02;
}
lease 10.0.8.50 {
  starts epoch 1794533400; # Fri Nov 13 02:30:00 2026
  ends never;
  binding state active;
  hardware ethernet 7c:c2:55:92:43:95;
  client-hostname "bmc\303\245";
}
lease 192.0.2.23 {
  starts 5 2026/11/13 01:40:00;
  ends 5 2026/11/13 01:40:00;
  binding state abandoned;
}
`

func parse(t *testing.T, text string) ([]Lease, int64, []string) {
	t.Helper()
	var got []Lease
	var bad []string
	n, err := Parse(strings.NewReader(text), func(l Lease) { got = append(got, l) },
		func(line int, err error) { bad = append(bad, err.Error()) })
	if err != nil {
		t.Fatal(err)
	}
	return got, n, bad
}

func TestParseWhatDhcpdWrites(t *testing.T) {
	got, n, bad := parse(t, sample)
	if len(bad) != 0 {
		t.Fatalf("blocks refused: %v", bad)
	}
	if n != int64(len(sample)) {
		t.Fatalf("consumed %d of %d bytes", n, len(sample))
	}
	if len(got) != 4 {
		t.Fatalf("got %d leases, want 4 (the failover block is not one)", len(got))
	}
	a := got[0]
	if a.IP.String() != "192.0.2.21" || a.State != "active" {
		t.Errorf("first lease: %+v", a)
	}
	if want := time.Date(2026, 11, 13, 1, 30, 0, 0, time.UTC); !a.Starts.Equal(want) {
		t.Errorf("starts %v, want %v (the file's times are UTC)", a.Starts, want)
	}
	if want := time.Date(2026, 11, 13, 3, 30, 0, 0, time.UTC); !a.Ends.Equal(want) {
		t.Errorf("ends %v", a.Ends)
	}
	if a.MAC.String() != "3c:22:fb:aa:bb:01" {
		t.Errorf("mac %v", a.MAC)
	}
	if a.Hostname != `tech-laptop "7"` {
		t.Errorf("hostname %q", a.Hostname)
	}
	// option 82 is bytes: the quoted spelling as its text, the hex
	// spelling decoded
	if a.CircuitID != "ge-0/0/12.0:office" || a.RemoteID != "\x00\x06\x00\x1e\xbe\xef\x00\x01" {
		t.Errorf("option 82: %q / %q", a.CircuitID, a.RemoteID)
	}
	if a.VendorClass != "MSFT 5.0" {
		t.Errorf("vendor class %q", a.VendorClass)
	}
	if got[1].State != "free" || got[1].MAC.String() != "3c:22:fb:aa:bb:02" {
		t.Errorf("second lease: %+v", got[1])
	}
	// db-time-format local, and a lease that never ends
	c := got[2]
	if c.Starts.Unix() != 1794533400 || !c.Ends.IsZero() {
		t.Errorf("epoch/never: starts %v ends %v", c.Starts, c.Ends)
	}
	// octal escapes are bytes; the text is utf-8
	if c.Hostname != "bmcå" {
		t.Errorf("hostname %q", c.Hostname)
	}
	if got[3].State != "abandoned" || got[3].MAC != nil {
		t.Errorf("abandoned lease: %+v", got[3])
	}
}

func TestABlockStillBeingWrittenIsLeftForLater(t *testing.T) {
	whole := "lease 10.0.0.1 {\n  starts 5 2026/11/13 01:00:00;\n  binding state active;\n  hardware ethernet 00:00:00:00:00:01;\n}\n"
	half := "lease 10.0.0.2 {\n  starts 5 2026/11/13 01:00:00;\n  binding st"
	got, n, _ := parse(t, whole+half)
	if len(got) != 1 || n != int64(len(whole)) {
		t.Fatalf("got %d leases, consumed %d; want 1 and %d", len(got), n, len(whole))
	}
	// the same when the cut is exactly between two lines of the block
	got, n, _ = parse(t, whole+"lease 10.0.0.2 {\n  starts 5 2026/11/13 01:00:00;\n")
	if len(got) != 1 || n != int64(len(whole)) {
		t.Fatalf("got %d leases, consumed %d", len(got), n)
	}
}

func TestOneOddBlockDoesNotStopTheRest(t *testing.T) {
	text := "lease not-an-address {\n  binding state active;\n}\n" +
		"lease 10.0.0.1 {\n  starts 9 never-a-date;\n  binding state active;\n}\n" +
		"lease 10.0.0.2 {\n  hardware ethernet zz:zz;\n  binding state active;\n}\n" +
		"lease 10.0.0.3 {\n  starts 5 2026/11/13 01:00:00;\n  binding state active;\n  hardware ethernet 00:00:00:00:00:03;\n}\n"
	got, n, bad := parse(t, text)
	if len(got) != 1 || got[0].IP.String() != "10.0.0.3" {
		t.Fatalf("got %+v", got)
	}
	if len(bad) != 3 {
		t.Fatalf("refused %d blocks, want 3: %v", len(bad), bad)
	}
	if n != int64(len(text)) {
		t.Fatalf("consumed %d of %d", n, len(text))
	}
}

func TestFollowAGrowingAndARewrittenFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dhcpd.leases")
	block := func(ip string) string {
		return "lease " + ip + " {\n  starts 5 2026/11/13 01:00:00;\n  binding state active;\n  hardware ethernet 00:00:00:00:00:01;\n}\n"
	}
	write := func(text string, appendTo bool) {
		t.Helper()
		flags := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if appendTo {
			flags = os.O_WRONLY | os.O_APPEND
		}
		f, err := os.OpenFile(path, flags, 0o644)
		if err != nil {
			t.Fatal(err)
		}
		f.WriteString(text)
		f.Close()
	}
	poll := func(f *Follower) ([]string, bool) {
		t.Helper()
		var ips []string
		_, restarted, err := f.Poll(func(l Lease) { ips = append(ips, l.IP.String()) }, nil)
		if err != nil {
			t.Fatal(err)
		}
		return ips, restarted
	}
	f := &Follower{Path: path}
	if _, _, err := f.Poll(func(Lease) {}, nil); err == nil {
		t.Fatal("a missing file must be an error, not silence")
	}
	write("# header\n"+block("10.0.0.1"), false)
	if ips, restarted := poll(f); len(ips) != 1 || !restarted {
		t.Fatalf("first read: %v restarted=%v", ips, restarted)
	}
	if ips, restarted := poll(f); len(ips) != 0 || restarted {
		t.Fatalf("nothing new: %v restarted=%v", ips, restarted)
	}
	// dhcpd appends: half a block first, the rest later
	half := block("10.0.0.2")
	write(half[:30], true)
	if ips, _ := poll(f); len(ips) != 0 {
		t.Fatalf("half a block was handed out: %v", ips)
	}
	write(half[30:]+block("10.0.0.3"), true)
	if ips, restarted := poll(f); strings.Join(ips, ",") != "10.0.0.2,10.0.0.3" || restarted {
		t.Fatalf("appended: %v restarted=%v", ips, restarted)
	}
	// dhcpd rewrites: a new file renamed into place
	tmp := path + ".new"
	if err := os.WriteFile(tmp, []byte("# header\n"+block("10.0.0.3")+block("10.0.0.9")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
	if ips, restarted := poll(f); strings.Join(ips, ",") != "10.0.0.3,10.0.0.9" || !restarted {
		t.Fatalf("rewritten: %v restarted=%v", ips, restarted)
	}
	// a file that got shorter in place is read from the top as well
	write("# header\n"+block("10.0.0.4"), false)
	if ips, restarted := poll(f); strings.Join(ips, ",") != "10.0.0.4" || !restarted {
		t.Fatalf("truncated: %v restarted=%v", ips, restarted)
	}
}

func TestOptionBytesInBothSpellings(t *testing.T) {
	for in, want := range map[string]string{
		`"ge-0/0/7.0:guests"`: "ge-0/0/7.0:guests",
		`0:4:0:64:1:c`:        "\x00\x04\x00\x64\x01\x0c",
		`"aa:bb:cc:dd:ee:ff"`: "aa:bb:cc:dd:ee:ff", // text that looks like hex stays text
		`aa:bb:cc:dd:ee:ff`:   "\xaa\xbb\xcc\xdd\xee\xff",
		`"sw\"1\\\001"`:       "sw\"1\\\x01",
		`7f`:                  "\x7f",
		`not hex at all`:      "not hex at all",
		`1:2:zz`:              "1:2:zz",
		`123:4`:               "123:4",
	} {
		if got := optionBytes(in); got != want {
			t.Errorf("optionBytes(%s) = %q, want %q", in, got, want)
		}
	}
}
