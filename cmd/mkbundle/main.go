// Command mkbundle builds the init bundle: the uncompressed newc cpio fctl
// attaches as initrd_path on every boot (PLAN.md §8.2).
//
//	/init          vmsetup
//	/bin/zpinit    zpinit, with machine mode
//	/bin/zpctl
//	/dev/console   c 5 1, opened by the kernel for /init's stdio
//	/dev /proc /run /newroot   empty mount points
//
// The output is reproducible: fixed order, mtime 0, uid and gid 0, inode
// numbers by position, nothing taken from the host but file contents. The
// same inputs give a byte-identical cpio, so fctl can content-address it.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/0ploy/vmsetup/internal/bundle"
)

func main() {
	vmsetup := flag.String("vmsetup", "", "static vmsetup binary, becomes /init")
	zpinit := flag.String("zpinit", "", "static zpinit binary, becomes /bin/zpinit")
	zpctl := flag.String("zpctl", "", "static zpctl binary, becomes /bin/zpctl")
	out := flag.String("o", "init.cpio", "output file")
	// Only for the harness's negative test of a bundle without zpinit.
	noZpinit := flag.Bool("test-omit-zpinit", false, "leave /bin/zpinit out (boot-test negative case only)")
	flag.Parse()

	if *vmsetup == "" || *zpctl == "" || (*zpinit == "" && !*noZpinit) {
		fmt.Fprintln(os.Stderr, "usage: mkbundle -vmsetup FILE -zpinit FILE -zpctl FILE [-o init.cpio]")
		os.Exit(2)
	}
	files := map[string]string{"init": *vmsetup, "bin/zpctl": *zpctl}
	if !*noZpinit {
		files["bin/zpinit"] = *zpinit
	}
	if err := bundle.Write(*out, files); err != nil {
		fmt.Fprintln(os.Stderr, "mkbundle:", err)
		os.Exit(1)
	}
}
