// Package userenv resolves the image's USER to numeric ids and composes the
// environment the workload will run with. See PLAN.md §5.5.
//
// Build with CGO_ENABLED=0. Go's os/user then uses its pure-Go
// implementation, which parses /etc/passwd and /etc/group directly. That is
// exactly what is wanted here: it will not honor NSS (LDAP and friends),
// which is correct for a container image, and it does not drag libc into a
// binary that has to run in a `scratch` rootfs.
package userenv

import (
	"errors"
	"fmt"
	"os/user"
	"sort"
	"strconv"
	"strings"

	"github.com/0ploy/vminit/internal/config"
)

// User is a resolved user spec.
type User struct {
	Name string
	UID  int
	GID  int
	Home string
	// Groups are the supplementary group ids the user belongs to, empty
	// when the user was given numerically and no passwd entry backs it.
	Groups []int
}

// Resolve resolves a `user`, `user:group`, `uid`, `uid:gid` or mixed spec
// against the guest rootfs. An empty spec means root.
//
// Numeric ids are accepted with no backing passwd entry, because that is
// both legal in OCI and the only thing that can work in a `scratch` image
// with no /etc/passwd at all. A *name* with no backing entry is an error:
// silently falling back to root would hand the workload more privilege than
// the image asked for.
func Resolve(spec string) (*User, error) {
	if spec == "" {
		spec = "root"
	}
	userPart, groupPart, hasGroup := strings.Cut(spec, ":")
	if userPart == "" {
		return nil, fmt.Errorf("user spec %q: empty user", spec)
	}

	u, err := lookupUser(userPart)
	if err != nil {
		return nil, err
	}

	if hasGroup {
		gid, err := lookupGroup(groupPart)
		if err != nil {
			return nil, err
		}
		u.GID = gid
		// An explicit group replaces the passwd-derived primary group;
		// supplementary groups from /etc/group no longer apply.
		u.Groups = nil
	}

	return u, nil
}

func lookupUser(spec string) (*User, error) {
	if uid, err := strconv.Atoi(spec); err == nil {
		if uid < 0 {
			return nil, fmt.Errorf("uid %d is negative", uid)
		}
		// A numeric spec that happens to have a passwd entry still gets
		// its home directory and groups from it.
		if pw, err := user.LookupId(spec); err == nil {
			return fromPasswd(pw)
		}
		return &User{Name: spec, UID: uid, GID: uid, Home: "/"}, nil
	}

	pw, err := user.Lookup(spec)
	if err != nil {
		var unknown user.UnknownUserError
		if errors.As(err, &unknown) {
			return nil, fmt.Errorf("user %q not found in the image's /etc/passwd", spec)
		}
		return nil, fmt.Errorf("look up user %q: %w", spec, err)
	}
	return fromPasswd(pw)
}

func fromPasswd(pw *user.User) (*User, error) {
	uid, err := strconv.Atoi(pw.Uid)
	if err != nil {
		return nil, fmt.Errorf("passwd entry for %q has non-numeric uid %q", pw.Username, pw.Uid)
	}
	gid, err := strconv.Atoi(pw.Gid)
	if err != nil {
		return nil, fmt.Errorf("passwd entry for %q has non-numeric gid %q", pw.Username, pw.Gid)
	}
	u := &User{Name: pw.Username, UID: uid, GID: gid, Home: pw.HomeDir}
	if u.Home == "" {
		u.Home = "/"
	}
	// Supplementary groups are best effort: a missing or unreadable
	// /etc/group is normal in a minimal image and must not fail the boot.
	if ids, err := pw.GroupIds(); err == nil {
		for _, s := range ids {
			if g, err := strconv.Atoi(s); err == nil && g != gid {
				u.Groups = append(u.Groups, g)
			}
		}
	}
	return u, nil
}

func lookupGroup(spec string) (int, error) {
	if gid, err := strconv.Atoi(spec); err == nil {
		if gid < 0 {
			return 0, fmt.Errorf("gid %d is negative", gid)
		}
		return gid, nil
	}
	g, err := user.LookupGroup(spec)
	if err != nil {
		var unknown user.UnknownGroupError
		if errors.As(err, &unknown) {
			return 0, fmt.Errorf("group %q not found in the image's /etc/group", spec)
		}
		return 0, fmt.Errorf("look up group %q: %w", spec, err)
	}
	gid, err := strconv.Atoi(g.Gid)
	if err != nil {
		return 0, fmt.Errorf("group entry for %q has non-numeric gid %q", spec, g.Gid)
	}
	return gid, nil
}

// Env is a composed environment. It keeps insertion order so the value
// handed to execve is deterministic for a given config, which matters for
// vmpack's determinism requirement and for diffing boot logs.
type Env struct {
	order []string
	vals  map[string]string
}

// Compose builds the workload environment from the config layers. Precedence
// runs lowest to highest: image.env, then extra_env, then anything MMDS
// supplied. See PLAN.md §5.5.
func Compose(cfg *config.Config, mmds map[string]string) *Env {
	e := &Env{vals: map[string]string{}}
	for _, entry := range cfg.Image.Env {
		k, v, _ := strings.Cut(entry, "=")
		if k != "" {
			e.Set(k, v)
		}
	}
	for _, k := range sortedKeys(cfg.ExtraEnv) {
		e.Set(k, cfg.ExtraEnv[k])
	}
	for _, k := range sortedKeys(mmds) {
		e.Set(k, mmds[k])
	}
	return e
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ApplyDefaults fills in the variables a container runtime would have
// provided: HOME from the resolved user, and TERM, which enough software
// misbehaves without that Docker sets it too.
func (e *Env) ApplyDefaults(u *User) {
	if _, ok := e.vals["HOME"]; !ok && u != nil {
		e.Set("HOME", u.Home)
	}
	if _, ok := e.vals["TERM"]; !ok {
		e.Set("TERM", "linux")
	}
}

func (e *Env) Set(k, v string) {
	if _, ok := e.vals[k]; !ok {
		e.order = append(e.order, k)
	}
	e.vals[k] = v
}

func (e *Env) Get(k string) (string, bool) {
	v, ok := e.vals[k]
	return v, ok
}

// Slice returns the environment in KEY=value form, ready for execve.
func (e *Env) Slice() []string {
	out := make([]string, 0, len(e.order))
	for _, k := range e.order {
		out = append(out, k+"="+e.vals[k])
	}
	return out
}

// Keys returns the variable names in composition order.
func (e *Env) Keys() []string {
	return append([]string(nil), e.order...)
}
