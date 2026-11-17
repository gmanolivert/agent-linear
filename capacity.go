package main

import (
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"syscall"
)

// capacityWait is a pre-dispatch refusal, never a failed or claimed issue.
type capacityWait struct {
	available uint64
	minimum   uint64
	cause     error
}

func (e *capacityWait) Error() string {
	if e.cause != nil {
		return fmt.Sprintf("disk capacity unavailable; need at least %d free bytes before starting work", e.minimum)
	}
	return fmt.Sprintf("disk capacity low: %d free bytes, need at least %d; work deferred", e.available, e.minimum)
}

func availableDiskBytes(path string) (uint64, error) {
	var fs syscall.Statfs_t
	if err := syscall.Statfs(path, &fs); err != nil {
		return 0, err
	}
	// Bavail excludes blocks reserved for root: jobs run as the unprivileged user.
	if fs.Bsize <= 0 || fs.Bavail > math.MaxUint64/uint64(fs.Bsize) {
		return 0, errors.New("invalid filesystem capacity")
	}
	return fs.Bavail * uint64(fs.Bsize), nil
}

func (r *runner) checkCapacity(c *config) error {
	if c.MinFreeBytes == 0 {
		return nil // Preserve installations that have not configured a reserve.
	}
	measure := r.availableBytes
	if measure == nil {
		measure = availableDiskBytes
	}
	free, err := measure(filepath.Join(r.root, ".runs"))
	if err != nil || free < c.MinFreeBytes {
		return &capacityWait{available: free, minimum: c.MinFreeBytes, cause: err}
	}
	return nil
}
