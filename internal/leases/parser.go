// SPDX-License-Identifier: GPL-3.0-or-later

// Package leases reads ISC dhcpd's lease file, dhcpd.leases(5).
//
// The file is a journal: dhcpd appends a "lease <ip> { ... }" block every
// time something happens to a lease, and now and then rewrites the whole
// file with one block per lease. The last block for an address is what is
// true now.
package leases

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Lease is one block of the lease file.
type Lease struct {
	IP     netip.Addr
	Starts time.Time
	// Ends is the zero time for "ends never" and for a block without one.
	Ends  time.Time
	State string // active, free, expired, released, abandoned, backup, ...
	// MAC is nil when the block names no hardware address (abandoned
	// leases, some free ones).
	MAC      net.HardwareAddr
	Hostname string
	// CircuitID and RemoteID are the relay agent's option 82, as the
	// BYTES the relay sent (a Go string used as a byte string). dhcpd
	// writes them as quoted text when every byte is printable and as
	// colon-separated hex otherwise; both forms come out as the same
	// bytes here, so nothing depends on how dhcpd chose to print them.
	CircuitID   string
	RemoteID    string
	VendorClass string
}

// Parse reads complete lease blocks from r and calls fn for each, in file
// order. Everything that is not a lease block (comments, server-duid,
// failover state) is skipped. It returns the number of bytes consumed up
// to and including the last COMPLETE block or skipped line: a block still
// being written at the end of the file is left for the next call. A block
// that cannot be understood is reported through bad and skipped - one odd
// lease must not stop the rest.
func Parse(r io.Reader, fn func(Lease), bad func(line int, err error)) (int64, error) {
	br := bufio.NewReaderSize(r, 1<<16)
	var consumed, pending int64
	var cur *Lease
	var curErr error
	depth := 0
	lineNo, startLine := 0, 0
	for {
		line, err := br.ReadString('\n')
		if err != nil && err != io.EOF {
			return consumed, err
		}
		complete := strings.HasSuffix(line, "\n")
		if !complete {
			// a line without its end: dhcpd is still writing it
			return consumed, nil
		}
		lineNo++
		pending += int64(len(line))
		text := strings.TrimSpace(line)
		switch {
		case cur == nil && depth == 0:
			if strings.HasPrefix(text, "lease ") && strings.HasSuffix(text, "{") {
				fields := strings.Fields(text)
				cur = &Lease{}
				curErr = nil
				startLine = lineNo
				depth = 1
				if len(fields) != 3 {
					curErr = fmt.Errorf("odd lease line %q", text)
				} else if ip, perr := netip.ParseAddr(fields[1]); perr != nil {
					curErr = fmt.Errorf("lease address %q: %v", fields[1], perr)
				} else {
					cur.IP = ip
				}
			} else if strings.HasSuffix(text, "{") {
				// another kind of block (failover peer ... state): skip it
				depth = 1
			} else {
				consumed += pending
				pending = 0
			}
		default:
			if text == "}" {
				depth--
			} else if strings.HasSuffix(text, "{") {
				depth++
			} else if cur != nil && depth == 1 && curErr == nil {
				curErr = statement(cur, text)
			}
			if depth == 0 {
				if cur != nil {
					if curErr == nil && cur.Starts.IsZero() && cur.State == "" {
						curErr = fmt.Errorf("lease %s has neither starts nor a binding state", cur.IP)
					}
					if curErr != nil {
						if bad != nil {
							bad(startLine, curErr)
						}
					} else {
						fn(*cur)
					}
					cur = nil
				}
				consumed += pending
				pending = 0
			}
		}
	}
}

// statement applies one "name value;" line of a lease block.
func statement(l *Lease, text string) error {
	text = strings.TrimSuffix(text, ";")
	name, rest, _ := strings.Cut(text, " ")
	rest = strings.TrimSpace(rest)
	switch name {
	case "starts":
		t, err := parseTime(rest)
		if err != nil {
			return fmt.Errorf("starts: %v", err)
		}
		l.Starts = t
	case "ends":
		t, err := parseTime(rest)
		if err != nil {
			return fmt.Errorf("ends: %v", err)
		}
		l.Ends = t
	case "binding":
		// binding state active
		if v, ok := strings.CutPrefix(rest, "state "); ok {
			l.State = strings.TrimSpace(v)
		}
	case "hardware":
		// hardware ethernet 00:11:22:33:44:55
		fields := strings.Fields(rest)
		if len(fields) == 2 && fields[0] == "ethernet" {
			mac, err := net.ParseMAC(fields[1])
			if err != nil {
				return fmt.Errorf("hardware ethernet %q: %v", fields[1], err)
			}
			l.MAC = mac
		}
	case "client-hostname":
		l.Hostname = unquote(rest)
	case "option":
		// option agent.circuit-id "text"  or  0:4:0:64:1:2
		opt, value, _ := strings.Cut(rest, " ")
		switch opt {
		case "agent.circuit-id":
			l.CircuitID = optionBytes(strings.TrimSpace(value))
		case "agent.remote-id":
			l.RemoteID = optionBytes(strings.TrimSpace(value))
		}
	case "set":
		// set vendor-class-identifier = "MSFT 5.0"
		variable, value, ok := strings.Cut(rest, "=")
		if ok && strings.TrimSpace(variable) == "vendor-class-identifier" {
			l.VendorClass = unquote(strings.TrimSpace(value))
		}
	}
	return nil
}

// parseTime reads dhcpd's two ways of writing a time: "5 2026/11/13
// 00:24:45" (weekday, then UTC), "epoch 1794530000" (db-time-format
// local), and "never".
func parseTime(s string) (time.Time, error) {
	// db-time-format local adds a comment after the value
	if i := strings.Index(s, ";"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimSpace(s)
	if s == "never" {
		return time.Time{}, nil
	}
	if v, ok := strings.CutPrefix(s, "epoch "); ok {
		secs, err := strconv.ParseInt(strings.TrimSpace(v), 10, 64)
		if err != nil {
			return time.Time{}, fmt.Errorf("epoch %q", v)
		}
		return time.Unix(secs, 0).UTC(), nil
	}
	_, date, ok := strings.Cut(s, " ")
	if !ok {
		return time.Time{}, fmt.Errorf("time %q", s)
	}
	t, err := time.ParseInLocation("2006/01/02 15:04:05", strings.TrimSpace(date), time.UTC)
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q", s)
	}
	return t, nil
}

// unquote turns dhcpd's quoted string - with \" \\ and \ooo octal escapes -
// into its text; a value that is not quoted is kept as it stands. Bytes
// that are not valid UTF-8 are dropped: the result goes into a text
// column.
func unquote(s string) string {
	raw, quoted := unquoteBytes(s)
	if !quoted {
		return s
	}
	return strings.ToValidUTF8(strings.ReplaceAll(raw, "\x00", ""), "")
}

// unquoteBytes returns the bytes of a quoted string and true, or s
// itself and false when s is not quoted.
func unquoteBytes(s string) (string, bool) {
	if len(s) < 2 || s[0] != '"' || s[len(s)-1] != '"' {
		return s, false
	}
	s = s[1 : len(s)-1]
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '\\' || i+1 >= len(s) {
			b.WriteByte(c)
			continue
		}
		if i+3 < len(s) && isOctal(s[i+1]) && isOctal(s[i+2]) && isOctal(s[i+3]) {
			v, _ := strconv.ParseUint(s[i+1:i+4], 8, 8)
			b.WriteByte(byte(v))
			i += 3
			continue
		}
		i++
		b.WriteByte(s[i])
	}
	return b.String(), true
}

// optionBytes returns the bytes of an option value in either of dhcpd's
// two spellings: "quoted text", or colon-separated hex (0:4:0:64:1:c).
// Anything else is kept as its own bytes.
func optionBytes(s string) string {
	if raw, quoted := unquoteBytes(s); quoted {
		return raw
	}
	parts := strings.Split(s, ":")
	out := make([]byte, 0, len(parts))
	for _, part := range parts {
		if len(part) < 1 || len(part) > 2 {
			return s
		}
		v, err := strconv.ParseUint(part, 16, 8)
		if err != nil {
			return s
		}
		out = append(out, byte(v))
	}
	return string(out)
}

func isOctal(c byte) bool { return c >= '0' && c <= '7' }
