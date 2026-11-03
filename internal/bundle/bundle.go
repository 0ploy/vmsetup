// Package bundle writes the init bundle as a reproducible newc cpio
// (SPEC.md §10.2). It implements just the subset of the format the kernel's
// initramfs unpacker needs: directories, regular files and one device node.
package bundle

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path"
	"sort"
	"strings"
)

// Mode bits, as cpio carries them (the same as st_mode).
const (
	modeDir  = 0o040000
	modeReg  = 0o100000
	modeChar = 0o020000
)

// Dirs are the directories every bundle carries: mount points vmsetup uses
// in the initramfs, and the parents of the files.
var Dirs = []string{"bin", "dev", "newroot", "proc", "run"}

type entry struct {
	name      string
	mode      uint32
	rdevMajor uint32
	rdevMinor uint32
	data      []byte
}

// Write builds the bundle at out. files maps a path inside the bundle (no
// leading slash) to a host file whose contents it takes. Only the contents
// are taken: mode is 0755 for every file, owner root, mtime 0.
func Write(out string, files map[string]string) error {
	var entries []entry
	for _, d := range Dirs {
		entries = append(entries, entry{name: d, mode: modeDir | 0o755})
	}
	entries = append(entries, entry{name: "dev/console", mode: modeChar | 0o600, rdevMajor: 5, rdevMinor: 1})
	for name, src := range files {
		if strings.HasPrefix(name, "/") || path.Clean(name) != name {
			return fmt.Errorf("bundle path %q must be clean and relative", name)
		}
		if dir := path.Dir(name); dir != "." && !contains(Dirs, dir) {
			return fmt.Errorf("bundle path %q is not in one of %v", name, Dirs)
		}
		data, err := os.ReadFile(src)
		if err != nil {
			return err
		}
		entries = append(entries, entry{name: name, mode: modeReg | 0o755, data: data})
	}
	// Directories sort before their contents because a parent is a prefix
	// of its children.
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var buf bytes.Buffer
	for i, e := range entries {
		writeEntry(&buf, uint32(i+1), e)
	}
	writeEntry(&buf, 0, entry{name: "TRAILER!!!"})

	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// writeEntry writes one newc header, the NUL-terminated name and the data,
// each padded to 4 bytes.
func writeEntry(w *bytes.Buffer, ino uint32, e entry) {
	nlink := uint32(1)
	if e.mode&modeDir != 0 {
		nlink = 2
	}
	name := e.name + "\x00"
	fmt.Fprintf(w, "070701%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X%08X",
		ino, e.mode, 0, 0, nlink, 0, len(e.data), 0, 0, e.rdevMajor, e.rdevMinor, len(name), 0)
	w.WriteString(name)
	pad(w)
	w.Write(e.data)
	pad(w)
}

func pad(w *bytes.Buffer) {
	for w.Len()%4 != 0 {
		w.WriteByte(0)
	}
}

// Entry is one decoded member, for tests and for inspecting a bundle.
type Entry struct {
	Name  string
	Mode  uint32
	Rdev  [2]uint32
	Data  []byte
	Inode uint32
}

// Read decodes a newc cpio written by Write.
func Read(r io.Reader) ([]Entry, error) {
	b, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var out []Entry
	off := 0
	field := func(i int) uint32 {
		var v uint32
		fmt.Sscanf(string(b[off+6+8*i:off+14+8*i]), "%08X", &v)
		return v
	}
	align := func(n int) int { return (n + 3) &^ 3 }
	for {
		if off+110 > len(b) || string(b[off:off+6]) != "070701" {
			return nil, fmt.Errorf("bad header at offset %d", off)
		}
		ino, mode, size, rmaj, rmin, nsize := field(0), field(1), int(field(6)), field(9), field(10), int(field(11))
		nameStart := off + 110
		name := string(b[nameStart : nameStart+nsize-1])
		dataStart := align(nameStart + nsize)
		if name == "TRAILER!!!" {
			return out, nil
		}
		out = append(out, Entry{Name: name, Mode: mode, Rdev: [2]uint32{rmaj, rmin}, Data: b[dataStart : dataStart+size], Inode: ino})
		off = align(dataStart + size)
	}
}
