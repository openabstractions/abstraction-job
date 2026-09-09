package job

import (
	"fmt"
	"os"
	"sort"
	"testing"
	"time"
)

// The instrument behind research/watch/measure.sh. Skipped unless asked for:
// it takes seconds and its output is a number, not a verdict.
func TestMeasureWatch(t *testing.T) {
	if os.Getenv("WATCH_MEASURE") == "" {
		t.Skip("set WATCH_MEASURE=1 to measure")
	}
	const samples = 40
	const budget = 100 * time.Millisecond
	var s Store = openStore(t)

	var latency []time.Duration
	sub := Watch(s, "test")
	nextNotice(t, sub)
	for i := 0; i < samples; i++ {
		waiting := make(chan struct{})
		got := make(chan time.Time, 1)
		go func() {
			close(waiting)
			nextNotice(t, sub)
			got <- time.Now()
		}()
		<-waiting
		wrote := time.Now()
		submitOne(t, s)
		latency = append(latency, (<-got).Sub(wrote))
	}
	sub.Close()

	var overshoot []time.Duration
	quiet := WatchQuiet(s, "test", budget)
	nextNotice(t, quiet)
	for i := 0; i < samples; i++ {
		submitOne(t, s)
		nextNotice(t, quiet)
		n := nextNotice(t, quiet)
		if !n.Quiet {
			t.Fatalf("sample %d: %+v, want quiet", i, n)
		}
		overshoot = append(overshoot, n.Silence-budget)
	}
	quiet.Close()

	var read []time.Duration
	for i := 0; i < samples; i++ {
		start := time.Now()
		if _, err := s.List(); err != nil {
			t.Fatal(err)
		}
		read = append(read, time.Since(start))
	}

	report := func(name string, d []time.Duration) {
		sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
		p := func(q float64) float64 { return float64(d[int(q*float64(len(d)-1))]) / float64(time.Millisecond) }
		fmt.Printf("%s_p50_ms\t%.1f\n%s_p95_ms\t%.1f\n", name, p(0.5), name, p(0.95))
	}
	fmt.Printf("records\t%d\n", 2*samples)
	report("notice_latency", latency)
	report("quiet_overshoot", overshoot)
	report("watch_read", read)
}
