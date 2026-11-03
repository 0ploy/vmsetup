// Command mkblob turns a JSON config document into the raw config device
// vmsetup reads (PLAN.md §4.1). It is the test fixture's stand-in for fctl's
// encoder, and uses the same package vmsetup decodes with, so the two cannot
// disagree about the format.
//
// The document is validated with vmsetup's own parser first, so a fixture
// cannot boot a config vmsetup would reject -- unless a scenario asks for
// exactly that with -magic or -version.
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"os"

	"github.com/0ploy/vmsetup/internal/config"
)

func main() {
	magic := flag.String("magic", "", "write this 8-byte magic instead of the real one (negative tests)")
	version := flag.Uint("version", config.Version, "header version to write (negative tests)")
	flag.Parse()
	if flag.NArg() != 2 {
		fmt.Fprintln(os.Stderr, "usage: mkblob [-magic M] [-version N] DOC.json OUT.blob")
		os.Exit(2)
	}
	doc, err := os.ReadFile(flag.Arg(0))
	if err != nil {
		fail(err)
	}
	if _, err := config.Parse(doc); err != nil {
		fail(fmt.Errorf("%s does not validate: %w", flag.Arg(0), err))
	}
	blob, err := config.Encode(doc)
	if err != nil {
		fail(err)
	}
	if *magic != "" {
		if len(*magic) > 8 {
			fail(fmt.Errorf("-magic is at most 8 bytes"))
		}
		copy(blob[:8], make([]byte, 8))
		copy(blob[:8], *magic)
	}
	binary.LittleEndian.PutUint32(blob[8:12], uint32(*version))
	if err := os.WriteFile(flag.Arg(1), blob, 0o644); err != nil {
		fail(err)
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "mkblob:", err)
	os.Exit(1)
}
