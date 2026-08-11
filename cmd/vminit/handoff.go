package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/0ploy/vminit/internal/config"
	"github.com/0ploy/vminit/internal/userenv"
)

// zpinitEnvFile is the file zpinit's entrypoint.d phase reads KEY=value lines
// from, and the one its scripts append to in order to hand variables forward.
const zpinitEnvFile = "/run/zpinit/env"

// vmFlag is the flag that puts zpinit into VM mode. Without it zpinit behaves
// as a container init and exits when its services are done, which as PID 1 of
// a VM panics the kernel with "Attempted to kill init!". See ZPINIT_CHANGES.md
// §1 and §2.
const vmFlag = "--vm"

// envKeyPattern matches the keys zpinit's env-file parser accepts. Anything
// else it logs and skips, so there is no point writing it.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// handoffZpinit finishes bringup by replacing vminit with zpinit.
//
// execve, never fork: zpinit's reaper is written on the assumption that it is
// PID 1 ("As PID 1 zpinit must reap every child, including orphans reparented
// to us after a double-fork"), and it does not set PR_SET_CHILD_SUBREAPER. It
// also owns the machine lifecycle from here -- the reboot on shutdown, the
// exit-status sentinel -- none of which works if vminit is still sitting above
// it.
func (b *booter) handoffZpinit(cfg *config.Config, env *userenv.Env) error {
	if err := b.writeZpinitEnv(env); err != nil {
		return err
	}
	b.checkZpinitArgs(cfg)

	path := cfg.Boot.Path
	argv := append([]string{path}, cfg.Boot.Args...)

	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("handoff target %s: %w", path, err)
	}

	b.log.Infof("bringup complete in %.3fms; execve %s %s",
		float64(b.log.Elapsed().Microseconds())/1000, path, strings.Join(cfg.Boot.Args, " "))

	// The composed environment travels in envp as well as in the env file.
	// envp is zpinit's "container env" tier, which is precisely what image
	// env is, and unlike the file it can carry any byte a variable's value
	// might contain.
	if err := unix.Exec(path, argv, env.Slice()); err != nil {
		return fmt.Errorf("execve %s: %w", path, err)
	}
	return nil // unreachable: Exec only returns on failure
}

// writeZpinitEnv writes the composed environment where zpinit's entrypoint.d
// phase will find it.
//
// This is redundant with envp for zpinit itself, and deliberately so: the file
// is what makes the composed environment visible to entrypoint.d scripts, and
// what they extend by appending to it. /run is a tmpfs, so the file costs
// nothing and disappears on shutdown.
//
// The format cannot represent a value containing a newline -- zpinit's parser
// is line-based -- so such a variable is skipped with a warning rather than
// written, which would silently corrupt every line after it. Nothing is lost:
// it still reaches zpinit through envp.
func (b *booter) writeZpinitEnv(env *userenv.Env) error {
	if err := os.MkdirAll(filepath.Dir(zpinitEnvFile), 0o755); err != nil && !os.IsExist(err) {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(zpinitEnvFile), err)
	}

	var sb strings.Builder
	sb.WriteString("# written by vminit; do not edit\n")
	skipped := 0
	for _, entry := range env.Slice() {
		k, v, _ := strings.Cut(entry, "=")
		if !envKeyPattern.MatchString(k) {
			b.log.Warnf("env variable %q has a name zpinit's env file cannot represent; passing it via envp only", k)
			skipped++
			continue
		}
		if strings.ContainsAny(v, "\n\r") {
			b.log.Warnf("env variable %s has a value containing a newline; passing it via envp only", k)
			skipped++
			continue
		}
		sb.WriteString(k)
		sb.WriteByte('=')
		sb.WriteString(v)
		sb.WriteByte('\n')
	}

	// 0600: image environment routinely carries credentials, and everything
	// that legitimately reads this file (zpinit itself, entrypoint.d
	// scripts) runs as root.
	if err := os.WriteFile(zpinitEnvFile, []byte(sb.String()), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", zpinitEnvFile, err)
	}
	b.log.Infof("wrote %d variables to %s", len(env.Keys())-skipped, zpinitEnvFile)
	return nil
}

// checkZpinitArgs warns when the handoff would start zpinit in container mode.
//
// vminit cannot fix this by adding the flag itself: older zpinit releases
// reject unknown flags outright, so injecting it would turn a working boot into
// a failing one. vmpack writes the flag; this catches a hand-written config
// that forgot it, where the symptom would otherwise be a kernel panic at
// shutdown rather than anything pointing at the cause.
func (b *booter) checkZpinitArgs(cfg *config.Config) {
	if slices.Contains(cfg.Boot.Args, vmFlag) {
		return
	}
	b.log.Warnf("boot.args does not contain %s: zpinit will run in container mode and exiting will panic the kernel with \"Attempted to kill init!\"", vmFlag)
}
