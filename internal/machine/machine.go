// Package machine writes what vmsetup hands zpinit besides argv, envp and the
// working directory: /run/zpinit/machine.json and /run/zpinit/env. See
// PLAN.md §5.5 and §5.6.
//
// zpinit owns both formats (zpinit#10); vmsetup is the writer that runs
// before it. zpinit reads machine.json only in machine mode, so nothing here
// can change how zpinit behaves in a container.
package machine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/0ploy/vmsetup/internal/config"
)

const (
	// Dir is zpinit's runtime directory, on the /run tmpfs.
	Dir = "/run/zpinit"
	// SettingsPath is machine.json.
	SettingsPath = Dir + "/machine.json"
	// EnvPath is the env file zpinit merges before every entrypoint.d script.
	EnvPath = Dir + "/env"
	// BinDir holds the bundle's zpinit and zpctl after the switch root, on a
	// read-only tmpfs of its own (PLAN.md §5.2).
	BinDir = Dir + "/bin"
	// RescueConfigDir is the empty zpinit config directory a parked machine
	// boots zpinit with, so a broken image config cannot also break rescue.
	RescueConfigDir = Dir + "/rescue.d"
)

// Version is the machine.json format version this vmsetup writes.
const Version = 1

// Settings is machine.json, PLAN.md §5.6.
type Settings struct {
	V int `json:"v"`
	// Console is where zpinit writes the reset line, even with logging off.
	Console string `json:"console"`
	Stop    Stop   `json:"stop"`
	// User is the wrap-mode child's identity. Empty in supervise mode, where
	// users are per service.
	User string `json:"user,omitempty"`
	// Exec, when set, makes zpinit open its vsock listener.
	Exec *Exec `json:"exec,omitempty"`
	// Rescue is set only when bringup failed and vmsetup parked.
	Rescue *Rescue `json:"rescue,omitempty"`
}

type Stop struct {
	Signal  string          `json:"signal"`
	Timeout config.Duration `json:"timeout"`
}

type Exec struct {
	Port       uint32 `json:"port"`
	User       string `json:"user"`
	WorkingDir string `json:"working_dir"`
	// Env is the composed env, the base every exec request layers over.
	Env []string `json:"env"`
}

type Rescue struct {
	Step  string `json:"step"`
	Error string `json:"error"`
}

// FromConfig builds the settings for a normal handoff. env is the composed
// environment, already defaulted.
func FromConfig(cfg *config.Config, console string, env []string) *Settings {
	s := &Settings{
		V:       Version,
		Console: console,
		Stop:    Stop{Signal: cfg.Handoff.StopSignal, Timeout: cfg.Handoff.StopTimeout},
	}
	if cfg.Handoff.Wrap() {
		s.User = cfg.Handoff.User
	}
	if e := cfg.Exec; e != nil {
		s.Exec = &Exec{Port: e.Port, User: e.User, WorkingDir: e.WorkingDir, Env: env}
	}
	return s
}

// Write writes the settings to SettingsPath, mode 0600: exec.env routinely
// carries credentials.
func (s *Settings) Write() error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(SettingsPath, append(b, '\n'))
}

// envKeyPattern matches the keys a line-based KEY=value parser (zpinit's in
// particular) accepts.
var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// EnvFile renders env for EnvPath and reports the variables it had to leave
// out. The format cannot represent a newline in a value or an unusual key,
// so such a variable is skipped rather than corrupting every line after it;
// it still reaches zpinit through envp.
func EnvFile(env []string) (content string, skipped []string) {
	var sb strings.Builder
	sb.WriteString("# written by vmsetup; zpinit's entrypoint.d scripts may append\n")
	for _, entry := range env {
		k, v, _ := strings.Cut(entry, "=")
		if !envKeyPattern.MatchString(k) || strings.ContainsAny(v, "\n\r") {
			skipped = append(skipped, k)
			continue
		}
		fmt.Fprintf(&sb, "%s=%s\n", k, v)
	}
	return sb.String(), skipped
}

// WriteEnv writes env to EnvPath, mode 0600, and returns the skipped keys.
//
// The file equals envp, so zpinit's merge of it above the container-env tier
// changes no value; it exists so entrypoint.d scripts can see the env and
// append to it (PLAN.md §5.5).
func WriteEnv(env []string) ([]string, error) {
	content, skipped := EnvFile(env)
	return skipped, writePrivate(EnvPath, []byte(content))
}

func writePrivate(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(path), err)
	}
	// Remove first so an existing file's mode cannot survive the write.
	_ = os.Remove(path)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
