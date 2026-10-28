package runner

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// TestConcurrentAccessIsRaceFree drives every entry point from several
// goroutines at once. The supervisor reads and writes the same records from the
// HTTP handlers, the log readers and the exit watchers, so this is the test that
// keeps the locks honest; run it with -race.
func TestConcurrentAccessIsRaceFree(t *testing.T) {
	h := newHarness(t, testTunnel("a"), testTunnel("b"))
	tunnelA, _ := h.store.Tunnel("a")
	tunnelB, _ := h.store.Tunnel("b")

	h.dns.set([]string{"nas.example.com"}, okResult("nas.example.com"))

	var wg sync.WaitGroup
	start := make(chan struct{})

	worker := func(fn func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 60; i++ {
				fn(i)
			}
		}()
	}

	worker(func(i int) { _ = h.m.Start("a") })
	worker(func(i int) { h.m.Stop("a") })
	worker(func(i int) { _ = h.m.View(tunnelA) })
	worker(func(i int) { _ = h.m.View(tunnelB) })
	worker(func(i int) { _ = h.m.Logs("a", 50) })
	worker(func(i int) { _ = h.m.Running("a") })
	worker(func(i int) { h.m.RunDNSRoutes("a", i%2 == 0) })
	worker(func(i int) { h.m.SetDNSResult("a", okResult("nas.example.com")) })
	worker(func(i int) { h.m.ApplyLocalConfig(tunnelA) })
	worker(func(i int) { _ = h.m.Start("b") })
	worker(func(i int) { h.m.Stop("b") })
	worker(func(i int) { h.m.Forget("b") })
	worker(func(i int) { h.m.WaitIdle(time.Millisecond) })
	worker(func(i int) { h.m.pushLog("a", "line "+strconv.Itoa(i)) })

	// A child that keeps exiting and being restarted, so the timers, the exit
	// watcher and the log readers overlap with everything above.
	worker(func(i int) {
		child := h.sp.last()
		if child != nil {
			child.exit(exitInfo{Code: i % 3})
		}
	})

	close(start)
	wg.Wait()
}

// TestConcurrentOutputAndStop checks the narrow window the record's isCurrent
// rule exists for: output from a process that is being replaced.
func TestConcurrentOutputAndStop(t *testing.T) {
	h := newHarness(t, testTunnel("a"))
	if err := h.m.Start("a"); err != nil {
		t.Fatalf("Start: %v", err)
	}
	child := h.sp.last()

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < 200; i++ {
			child.writeOutput("line " + strconv.Itoa(i) + "\n")
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < 20; i++ {
			h.m.Stop("a")
			_ = h.m.Start("a")
			_ = h.m.Logs("a", 10)
		}
	}()
	wg.Wait()

	// Whatever the interleaving, the record must still be coherent.
	var status string
	var pid int
	h.withRec("a", func(r *rec) {
		status = r.status
		pid = r.pid
	})
	switch status {
	case StatusStarting, StatusRunning, StatusStopping, StatusStopped, StatusError:
	default:
		t.Fatalf("unexpected status %q", status)
	}
	if pid < 0 {
		t.Fatalf("pid = %d", pid)
	}
}
