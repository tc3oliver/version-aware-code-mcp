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

// tryLockShared takes the file's shared lock if it can have it at once, and
// reports whether it got it.
//
// Shared and non-blocking are both the point. Shared, because any number of
// holders may have it together and only an exclusive holder shuts them all
// out — which is exactly the asymmetry the server lock needs: many management
// commands beside each other, none of them beside a running server.
// Non-blocking, because the thing that would be waited for is a server that
// may run for weeks, and a command that hung until it stopped would be worse
// than one that says so.
//
// EINTR is retried for the same reason it is above; EWOULDBLOCK is not an
// error but the answer, and is reported as one.
func tryLockShared(f *os.File) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB)
		switch {
		case err == nil:
			return true, nil
		case errors.Is(err, syscall.EWOULDBLOCK):
			return false, nil
		case errors.Is(err, syscall.EINTR):
		default:
			return false, err
		}
	}
}
