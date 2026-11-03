package config

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

// The config blob, PLAN.md §4.1:
//
//	offset  0   "FCTLCFG\0"   8 bytes, magic
//	offset  8   version       u32 little-endian
//	offset 12   length        u32 little-endian, bytes of JSON that follow
//	offset 16   JSON          NUL-padded to the end of the device
const (
	Magic       = "FCTLCFG\x00"
	Version     = 1
	HeaderSize  = 16
	DeviceSize  = 64 << 10
	MaxDocument = DeviceSize - HeaderSize

	// CmdlineParam names the config device on the kernel command line.
	// Deliberately undotted: a dotted parameter is a module parameter.
	CmdlineParam = "vmsetup_config"
)

// DeviceFromCmdline returns the device named by vmsetup_config= in a kernel
// command line. A missing or repeated parameter is an error: vmsetup never
// searches for its config, and never guesses between two.
func DeviceFromCmdline(cmdline string) (string, error) {
	var found []string
	for _, f := range strings.Fields(cmdline) {
		if f == "--" {
			// Everything after -- belongs to init's argv, not the kernel.
			break
		}
		if v, ok := strings.CutPrefix(f, CmdlineParam+"="); ok {
			found = append(found, v)
		}
	}
	switch {
	case len(found) == 0:
		return "", fmt.Errorf("no %s= on the kernel command line; nothing to read the config from", CmdlineParam)
	case len(found) > 1:
		return "", fmt.Errorf("%s= appears %d times on the kernel command line; refusing to choose", CmdlineParam, len(found))
	case !strings.HasPrefix(found[0], "/dev/"):
		return "", fmt.Errorf("%s=%q is not a /dev path", CmdlineParam, found[0])
	}
	return found[0], nil
}

// ReadBlob reads the JSON document out of the config device at path.
func ReadBlob(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config device: %w", err)
	}
	defer f.Close()
	doc, err := Decode(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return doc, nil
}

// Decode reads a blob header and the document it describes from r. A wrong
// magic is reported as "not the config device", because that is what a
// drive-ordering bug looks like from here.
func Decode(r io.Reader) ([]byte, error) {
	var hdr [HeaderSize]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}
	if !bytes.Equal(hdr[:8], []byte(Magic)) {
		return nil, fmt.Errorf("this is not the config device: magic %q, want %q", hdr[:8], Magic)
	}
	if v := binary.LittleEndian.Uint32(hdr[8:12]); v != Version {
		return nil, fmt.Errorf("config blob version %d, this vmsetup reads version %d", v, Version)
	}
	n := binary.LittleEndian.Uint32(hdr[12:16])
	if n == 0 {
		return nil, errors.New("config blob is empty")
	}
	if n > MaxDocument {
		return nil, fmt.Errorf("config blob length %d exceeds the %d-byte device", n, DeviceSize)
	}
	doc := make([]byte, n)
	if _, err := io.ReadFull(r, doc); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("config blob length %d runs past the end of the device", n)
		}
		return nil, fmt.Errorf("read document: %w", err)
	}
	return doc, nil
}

// Encode wraps a document in a blob header and pads it to DeviceSize. This
// is the writer's half; fctl's encoder must produce the same bytes.
func Encode(doc []byte) ([]byte, error) {
	if len(doc) == 0 {
		return nil, errors.New("empty document")
	}
	if len(doc) > MaxDocument {
		return nil, fmt.Errorf("document is %d bytes; the device holds at most %d", len(doc), MaxDocument)
	}
	out := make([]byte, DeviceSize)
	copy(out, Magic)
	binary.LittleEndian.PutUint32(out[8:12], Version)
	binary.LittleEndian.PutUint32(out[12:16], uint32(len(doc)))
	copy(out[HeaderSize:], doc)
	return out, nil
}
