package statistic

import (
	"math/rand"
	"testing"
	"time"
)

func TestTimeStatistic(t *testing.T) {
	ts := NewTimeStatistic("test", 100*time.Microsecond, 100)
	for i := time.Duration(0); i < 100; i++ {
		ts.Set(i * time.Microsecond)
	}
	ts.Report()

	ts = NewTimeStatistic("test", 100*time.Microsecond, 100)
	for i := time.Duration(0); i < 1000000; i++ {
		ts.Set(i * time.Microsecond)
	}
	ts.Report()

	ts = NewTimeStatistic("test", 100*time.Microsecond, 40)
	for i := 0; i < 40; i++ {
		r := rand.Intn(100)
		ts.Set(time.Duration(800+r) * time.Millisecond)
	}
	ts.Report()
}

func Benchmark_TimeStatistic(b *testing.B) {
	ts := NewTimeStatistic("test", 100*time.Microsecond, 100)
	b.ResetTimer()

	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			ts.Set(time.Duration(rand.Int31n(1000000)) * time.Microsecond)
		}
	})
}
