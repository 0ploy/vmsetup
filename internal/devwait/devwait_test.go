package devwait

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDevicePresent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vda")
	if err := os.WriteFile(p, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := Device(p, time.Second); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Fatalf("a present device took %s", d)
	}
}

func TestDeviceAppearsLate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vdb")
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(p, nil, 0o600)
	}()
	if err := Device(p, time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestDeviceNeverAppears(t *testing.T) {
	p := filepath.Join(t.TempDir(), "vdz")
	start := time.Now()
	err := Device(p, 50*time.Millisecond)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "did not appear within") {
		t.Fatalf("error does not say the device never appeared: %v", err)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Fatalf("gave up after %s, before the timeout", d)
	}
}

func TestPollReturnsLastError(t *testing.T) {
	n := 0
	want := errors.New("still missing")
	err := Poll(10*time.Millisecond, func() error { n++; return want })
	if !errors.Is(err, want) {
		t.Fatalf("got %v", err)
	}
	if n < 2 {
		t.Fatalf("check ran %d times; want it retried", n)
	}
}
