//go:build !unix

package main

import "os"

// lockExclusive is the in-process lock alone where there is no flock.
//
// The mutex in lock.go still serialises this process's own operations, so the
// lifecycle is correct for one vacmcp at a time. Two of them on one data
// directory are not serialised here, and the managed lifecycle is not supported
// on such a platform: release-build.sh publishes a windows binary so `vacmcp
// serve --config` runs there, which is the query plane and touches none of this.
func lockExclusive(*os.File) error { return nil }
