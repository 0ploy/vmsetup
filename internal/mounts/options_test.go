package mounts

import (
	"testing"

	"golang.org/x/sys/unix"
)

func TestParseOptionsSplitsFlagsFromData(t *testing.T) {
	tests := []struct {
		opts      string
		wantFlags uintptr
		wantData  string
	}{
		{"", 0, ""},
		{"relatime", unix.MS_RELATIME, ""},
		{"ro,nosuid,nodev", unix.MS_RDONLY | unix.MS_NOSUID | unix.MS_NODEV, ""},
		// Anything the kernel does not take as a flag is filesystem data
		// and passes straight through, which is what lets options this
		// package has never heard of keep working.
		{"noatime,data=ordered,subvol=@home", unix.MS_NOATIME, "data=ordered,subvol=@home"},
		// The negative forms cancel a flag rather than setting one.
		{"ro,rw", 0, ""},
		{"nosuid,suid", 0, ""},
		{" relatime , nodev ", unix.MS_RELATIME | unix.MS_NODEV, ""},
	}
	for _, tt := range tests {
		flags, data := parseOptions(tt.opts)
		if flags != tt.wantFlags || data != tt.wantData {
			t.Errorf("parseOptions(%q) = (%#x, %q), want (%#x, %q)",
				tt.opts, flags, data, tt.wantFlags, tt.wantData)
		}
	}
}
