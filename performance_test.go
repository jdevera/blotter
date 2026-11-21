//go:build performance

package main

import (
	"os/exec"
	"runtime"
	"slices"
	"testing"
	"time"
)

func TestWrapMedianOverhead(t *testing.T) {
	if runtime.GOOS != "darwin" || runtime.GOARCH != "arm64" {
		t.Skip("15 ms budget is for Apple Silicon")
	}
	env, _ := testEnv(t)
	base := []time.Duration{}
	wrapped := []time.Duration{}
	for range 51 {
		start := time.Now()
		if err := exec.Command("/usr/bin/true").Run(); err != nil {
			t.Fatal(err)
		}
		base = append(base, time.Since(start))
		_, stderr, rc, d := invoke(env, "{}", wrapArgs("claude", "--", "/usr/bin/true")...)
		if rc != 0 {
			t.Fatal(stderr)
		}
		wrapped = append(wrapped, d)
	}
	slices.Sort(base)
	slices.Sort(wrapped)
	delta := wrapped[len(wrapped)/2] - base[len(base)/2]
	t.Logf("%s/%s %s: direct median %s, wrap median %s, added %s", runtime.GOOS, runtime.GOARCH, runtime.Version(), base[len(base)/2], wrapped[len(wrapped)/2], delta)
	if delta > 15*time.Millisecond {
		t.Fatalf("median overhead %s exceeds 15 ms", delta)
	}
}
