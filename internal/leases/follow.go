// SPDX-License-Identifier: GPL-3.0-or-later

package leases

import (
	"io"
	"os"
	"syscall"
)

// Follower reads a lease file as it grows, and starts over from the top
// when dhcpd has replaced it with a rewritten one (dhcpd writes a new
// file and renames it into place) or when the file got shorter.
type Follower struct {
	Path string

	offset int64
	dev    uint64
	ino    uint64
	opened bool
}

// Poll hands every lease block that has become complete since the last
// call to fn. restarted says that the file was read from its beginning
// (first call, or a rewritten file): the blocks then repeat what was seen
// before, and the receiver must cope with that.
func (f *Follower) Poll(fn func(Lease), bad func(line int, err error)) (blocks int, restarted bool, err error) {
	file, err := os.Open(f.Path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, false, err
	}
	var dev, ino uint64
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		dev, ino = uint64(st.Dev), uint64(st.Ino)
	}
	if !f.opened || dev != f.dev || ino != f.ino || info.Size() < f.offset {
		f.offset, f.dev, f.ino, f.opened = 0, dev, ino, true
		restarted = true
	}
	if info.Size() == f.offset {
		return 0, restarted, nil
	}
	if _, err := file.Seek(f.offset, io.SeekStart); err != nil {
		return 0, restarted, err
	}
	consumed, err := Parse(file, func(l Lease) {
		blocks++
		fn(l)
	}, bad)
	f.offset += consumed
	return blocks, restarted, err
}
