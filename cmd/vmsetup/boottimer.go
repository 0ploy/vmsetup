package main

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// Firecracker's boot-timer device, a pseudo MMIO device at the start of the
// x86_64 MMIO32 window. A one-byte write of bootTimerMagic at offset 0 makes
// the VMM log "Guest-boot-time = N us" measured on the host, from the
// InstanceStart request. Its value and address are upstream's
// resources/rootfs/overlay/usr/local/bin/init.c.
const (
	bootTimerAddr  = 0xc0000000
	bootTimerMagic = 123
)

// signalBootTimer writes the boot-timer byte through /dev/mem. That needs
// CONFIG_DEVMEM; CONFIG_STRICT_DEVMEM still allows it, because the address is
// MMIO no driver claims, not RAM.
func signalBootTimer() error {
	f, err := os.OpenFile("/dev/mem", os.O_RDWR|os.O_SYNC|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	page := os.Getpagesize()
	mem, err := unix.Mmap(int(f.Fd()), bootTimerAddr, page, unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		return fmt.Errorf("mmap /dev/mem at %#x: %w", bootTimerAddr, err)
	}
	mem[0] = bootTimerMagic
	return unix.Munmap(mem)
}
