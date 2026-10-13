package scan

import (
	"syscall"
	"testing"
)

func nice(t *testing.T) int {
	p, err := syscall.Getpriority(syscall.PRIO_PROCESS, syscall.Gettid())
	if err != nil {
		t.Fatal(err)
	}
	return p
}
