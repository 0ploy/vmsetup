// Package vmlog is vmsetup's console log, and the park line the host
// matches.
//
// The awkward part it exists to solve: vmsetup may have to log before it can
// log. In the initramfs /dev is empty until vmsetup mounts devtmpfs, and
// PID 1's inherited stdio is no help either, because production boots
// without console= (PLAN.md §7.2).
//
// So the logger starts out buffering, tees to stderr in case something is
// listening there, and is switched onto the real serial port by Attach as
// soon as /dev is known to be up. Anything logged before that point is
// flushed at that moment rather than lost (PLAN.md §5.1).
package vmlog

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"syscall"
	"time"
)

// SerialPath is the port Firecracker forwards to a per-VM file on the host
// via `PUT /serial`. On x86_64 the device exists whether or not console= was
// passed, so this works with a quiet kernel.
const SerialPath = "/dev/ttyS0"

// maxBuffered caps the pre-Attach buffer. A boot that fails before devtmpfs
// is mounted produces a handful of lines, so the cap only ever trips on a
// pathological loop; dropping the tail of that is better than growing
// without bound in PID 1.
const maxBuffered = 64 << 10

// Logger writes human-readable boot progress. Every line carries the elapsed
// time since vmsetup started, which is a monotonic delta and therefore usable
// even though the guest's wall clock is not trustworthy this early. Absolute
// boot timing is measured from the host, by fctl.
type Logger struct {
	mu       sync.Mutex
	start    time.Time
	out      io.Writer
	buf      bytes.Buffer
	overflow bool
	// tee is stderr while buffering: harmless if it goes nowhere, and it
	// is the only channel that works when the kernel did give init a
	// console.
	tee io.Writer
}

// New returns a Logger that buffers until Attach or Discard is called.
func New() *Logger {
	return &Logger{start: time.Now(), tee: os.Stderr}
}

// Attach opens the log target and flushes everything buffered so far. It
// returns the opened file, or nil when there is none, so the caller can point
// PID 1's stdio at the same place.
//
// A serial port that cannot be opened is not fatal: on a kernel built
// without 8250, or on aarch64 later, the machine should still boot. It
// degrades to stderr, and then to discarding, but says so first.
func (l *Logger) Attach(target string) *os.File {
	if target == "none" {
		l.Discard()
		return nil
	}
	f, err := os.OpenFile(SerialPath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		l.Warnf("cannot open %s (%v); logging to stderr instead", SerialPath, err)
		l.switchTo(os.Stderr)
		return nil
	}
	l.switchTo(f)
	return f
}

// Attached reports whether the buffer has already been flushed to a real
// destination. The fatal path uses it to decide whether it still has to open
// the console before it can report anything.
func (l *Logger) Attached() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.out != nil
}

// Discard drops the buffer and everything after it, for logging.target =
// "none".
func (l *Logger) Discard() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.buf.Reset()
	l.out = io.Discard
	l.tee = nil
}

func (l *Logger) switchTo(w io.Writer) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.out = w
	l.tee = nil
	if l.overflow {
		fmt.Fprintf(w, "[vmsetup] ... earlier output truncated at %d bytes\n", maxBuffered)
	}
	if l.buf.Len() > 0 {
		_, _ = w.Write(l.buf.Bytes())
		l.buf.Reset()
	}
}

// Writer returns the current log destination, for handing to a child's
// stdout/stderr. Before Attach it is the buffer, which is not useful to a
// child, so callers wanting to wire up a process should Attach first.
func (l *Logger) Writer() io.Writer {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out == nil {
		return io.Discard
	}
	return l.out
}

func (l *Logger) Infof(format string, args ...any) { l.emit("", format, args...) }
func (l *Logger) Warnf(format string, args ...any) { l.emit("warning: ", format, args...) }
func (l *Logger) Errorf(format string, args ...any) {
	l.emit("error: ", format, args...)
}

func (l *Logger) emit(level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	// Keep one event to one line: a multi-line message would break any
	// host-side parser reading the serial log, and Firecracker's rate
	// limiter makes long output expensive anyway.
	msg = strings.ReplaceAll(strings.TrimRight(msg, "\n"), "\n", "; ")
	line := fmt.Sprintf("[vmsetup %8.3fms] %s%s\n", float64(time.Since(l.start).Microseconds())/1000, level, msg)

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.out != nil {
		_, _ = io.WriteString(l.out, line)
		return
	}
	if l.buf.Len()+len(line) > maxBuffered {
		l.overflow = true
	} else {
		l.buf.WriteString(line)
	}
	if l.tee != nil {
		_, _ = io.WriteString(l.tee, line)
	}
}

// Elapsed reports how long vmsetup has been running, for the handoff log
// line. This is vmsetup's own contribution to cold start: the interval from
// the kernel running /init to the execve of zpinit.
func (l *Logger) Elapsed() time.Duration { return time.Since(l.start) }

// Sentinel writes one protocol line to the serial port for the host to match:
// the park line (PLAN.md §6.2). It is not a log line: it is written whatever
// logging.target says, it carries no timestamp prefix, and it starts with a
// newline so a partial line from the workload cannot swallow it.
//
// The port is opened afresh rather than reusing the log's file, because under
// logging.target = "none" there is no log file. Best effort: if the port
// cannot be opened there is nowhere else the host would look.
func Sentinel(line string) {
	f, err := os.OpenFile(SerialPath, os.O_WRONLY|os.O_APPEND|syscall.O_NOCTTY, 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "\n%s\n", line)
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "\n%s\n", line)
}
