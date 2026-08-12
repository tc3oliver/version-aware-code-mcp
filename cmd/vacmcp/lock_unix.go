//go:build unix

package main

import (
	"errors"
	"os"
	"syscall"
)

// lockExclusive blocks until this file descriptor holds the file's exclusive
// lock.
//
// flock treats each open file descriptor separately even within one process, so
// this is a lock between processes and between goroutines alike; the mutex in
// lock.go is kept all the same, because it is the whole of the guarantee on the
// platforms below.
//
// EINTR is retried rather than reported. Go's own signals — the preemption
// SIGURG above all — are what would deliver it, and a lock that gave up because
// the scheduler poked the thread would be a lock that sometimes is not one.
func lockExclusive(f *os.File) error {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}
