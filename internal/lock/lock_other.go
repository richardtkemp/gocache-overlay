//go:build !linux

package lock

import "errors"

// HeldEnv: see lock.go.
const HeldEnv = "HEAVY_LOCK_HELD"

// Lazy is unsupported off Linux: -lock fails the build instead of running unlocked.
type Lazy struct{ Path string }

// Need always fails off Linux.
func (l *Lazy) Need() error { return errors.New("lock: -lock is supported on Linux only") }

// State is always "".
func (l *Lazy) State() string { return "" }
