package userenv

import (
	"reflect"
	"testing"

	"github.com/0ploy/vminit/internal/config"
)

func TestComposePrecedence(t *testing.T) {
	cfg := &config.Config{
		Image:    config.Image{Env: []string{"PATH=/usr/bin", "A=image", "B=image"}},
		ExtraEnv: map[string]string{"B": "extra", "C": "extra"},
	}
	env := Compose(cfg, map[string]string{"C": "mmds", "D": "mmds"})

	want := []string{"PATH=/usr/bin", "A=image", "B=extra", "C=mmds", "D=mmds"}
	if got := env.Slice(); !reflect.DeepEqual(got, want) {
		t.Errorf("Slice() = %v, want %v", got, want)
	}
}

func TestComposeKeepsFirstAppearanceOrder(t *testing.T) {
	// Order has to be stable for a given config: vmpack's determinism
	// requirement and readable boot-log diffs both depend on it.
	cfg := &config.Config{
		Image:    config.Image{Env: []string{"Z=1", "A=1"}},
		ExtraEnv: map[string]string{"Y": "1", "B": "1"},
	}
	want := []string{"Z=1", "A=1", "B=1", "Y=1"}
	if got := Compose(cfg, nil).Slice(); !reflect.DeepEqual(got, want) {
		t.Errorf("Slice() = %v, want %v (image order, then extra_env sorted)", got, want)
	}
}

func TestApplyDefaults(t *testing.T) {
	env := Compose(&config.Config{}, nil)
	env.ApplyDefaults(&User{Home: "/home/app"})

	if got, _ := env.Get("HOME"); got != "/home/app" {
		t.Errorf("HOME = %q, want /home/app", got)
	}
	if got, _ := env.Get("TERM"); got != "linux" {
		t.Errorf("TERM = %q, want linux", got)
	}

	// An image that sets them keeps its own values.
	set := Compose(&config.Config{Image: config.Image{Env: []string{"HOME=/root", "TERM=xterm"}}}, nil)
	set.ApplyDefaults(&User{Home: "/home/app"})
	if got, _ := set.Get("HOME"); got != "/root" {
		t.Errorf("HOME = %q, want the image's /root", got)
	}
	if got, _ := set.Get("TERM"); got != "xterm" {
		t.Errorf("TERM = %q, want the image's xterm", got)
	}
}

func TestResolveNumeric(t *testing.T) {
	// A numeric spec has to work with no passwd entry behind it: that is
	// the only thing that can work in a scratch image with no /etc/passwd.
	u, err := Resolve("65534:65534")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.UID != 65534 || u.GID != 65534 {
		t.Errorf("uid/gid = %d/%d, want 65534/65534", u.UID, u.GID)
	}
}

func TestResolveRootFromPasswd(t *testing.T) {
	// The test host has an /etc/passwd, so root resolves through it.
	u, err := Resolve("root")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.UID != 0 || u.GID != 0 {
		t.Errorf("uid/gid = %d/%d, want 0/0", u.UID, u.GID)
	}
	if u.Home == "" {
		t.Error("Home is empty; it must always have a value for HOME to default to")
	}
}

func TestResolveEmptyMeansRoot(t *testing.T) {
	u, err := Resolve("")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.UID != 0 {
		t.Errorf("uid = %d, want 0", u.UID)
	}
}

func TestResolveUnknownNameIsAnError(t *testing.T) {
	// Falling back to root would hand the workload more privilege than the
	// image asked for, so an unknown name is fatal.
	if _, err := Resolve("nosuchuser-fbb1a2"); err == nil {
		t.Fatal("Resolve accepted a user with no passwd entry")
	}
	if _, err := Resolve("root:nosuchgroup-fbb1a2"); err == nil {
		t.Fatal("Resolve accepted a group with no group entry")
	}
	if _, err := Resolve(":wheel"); err == nil {
		t.Fatal("Resolve accepted an empty user")
	}
}

func TestResolveExplicitGroupOverridesPasswd(t *testing.T) {
	u, err := Resolve("root:12345")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if u.GID != 12345 {
		t.Errorf("gid = %d, want 12345", u.GID)
	}
	if len(u.Groups) != 0 {
		t.Errorf("Groups = %v, want none once the group was given explicitly", u.Groups)
	}
}
