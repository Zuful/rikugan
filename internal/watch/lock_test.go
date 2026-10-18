//go:build unix

package watch

import (
	"strings"
	"testing"
)

func TestLockRefusesASecondInstance(t *testing.T) {
	dir := t.TempDir()
	release, err := Lock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Lock(dir); err == nil || !strings.Contains(err.Error(), "another rikugan serve instance") {
		t.Fatalf("second lock: %v", err)
	}
	if err := release(); err != nil {
		t.Fatal(err)
	}
	again, err := Lock(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	_ = again()
}
