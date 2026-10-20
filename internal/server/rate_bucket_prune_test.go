package server

import (
	"maps"
	"math/rand"
	"strconv"
	"sync"
	"testing"
	"time"
)

const rateBucketTestBudget = 1 << 30

// previousRateBucketAllow 保留满桶时每次遍历的旧实现，核对额度、回收时机和性能。
func previousRateBucketAllow(s *Server, c Client, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.buckets) > 10000 {
		for id, b := range s.buckets {
			if now.Sub(b.updated) > 10*time.Minute {
				delete(s.buckets, id)
			}
		}
		if _, exists := s.buckets[c.ID]; !exists && len(s.buckets) > 10000 {
			return false
		}
	}
	b, ok := s.buckets[c.ID]
	if !ok {
		b = bucket{float64(c.RequestsPerMinute), now}
	}
	b.tokens = min(float64(c.RequestsPerMinute), b.tokens+now.Sub(b.updated).Seconds()*float64(c.RequestsPerMinute)/60)
	b.updated = now
	allowed := b.tokens >= 1
	if allowed {
		b.tokens--
	}
	s.buckets[c.ID] = b
	return allowed
}
func rateBucketPruneServer(size int, updated time.Time) *Server {
	s := &Server{buckets: make(map[string]bucket, size)}
	for i := range size {
		s.buckets[strconv.Itoa(i)] = bucket{tokens: rateBucketTestBudget, updated: updated}
	}
	return s
}

func checkRateBucketPrune(t *testing.T, current, previous *Server, client Client, now time.Time) {
	t.Helper()
	got, want := current.allow(client, now), previousRateBucketAllow(previous, client, now)
	if got != want || len(current.buckets) != len(previous.buckets) {
		t.Fatalf("限流结果变化：身份=%q，时间=%v，结果=%v/%v，桶数=%d/%d", client.ID, now, got, want, len(current.buckets), len(previous.buckets))
	}
	for id, want := range previous.buckets {
		if got, ok := current.buckets[id]; !ok || got != want {
			t.Fatalf("桶状态变化：身份=%q，结果=%+v，期望=%+v，存在=%v", id, got, want, ok)
		}
	}
}

func TestRateBucketPruneCompatibility(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	for _, size := range []int{0, 9999, 10000, 10001} {
		t.Run(strconv.Itoa(size), func(t *testing.T) {
			current := rateBucketPruneServer(size, base)
			previous := &Server{buckets: maps.Clone(current.buckets)}
			steps := []struct {
				id     string
				offset time.Duration
			}{
				{"0", 0}, {"missing", 0}, {"0", -9 * time.Minute},
				{"missing", time.Minute}, {"missing", time.Minute + time.Nanosecond},
				{"1", -time.Minute}, {"another", 9 * time.Minute}, {"another", 9*time.Minute + time.Nanosecond},
				{"0", 10 * time.Minute}, {"new", 10*time.Minute + time.Nanosecond},
				{"new", 11 * time.Minute}, {"0", 20 * time.Minute},
			}
			for _, step := range steps {
				checkRateBucketPrune(t, current, previous, Client{ID: step.id, RequestsPerMinute: 10}, base.Add(step.offset))
			}
			random := rand.New(rand.NewSource(1064))
			for range 100 {
				id := strconv.Itoa(random.Intn(16))
				if random.Intn(3) == 0 {
					id = "new-" + id
				}
				offset := time.Duration(random.Intn(45)-15)*time.Minute + time.Duration(random.Intn(3)-1)
				checkRateBucketPrune(t, current, previous, Client{ID: id, RequestsPerMinute: []int{1, 10, 300, 1000}[random.Intn(4)]}, base.Add(offset))
			}
		})
	}
}

func TestRateBucketPruneExpiryBoundary(t *testing.T) {
	for _, base := range []time.Time{time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC), time.Now(), {}} {
		current := rateBucketPruneServer(10001, base)
		current.buckets["0"] = bucket{tokens: 1, updated: base.Add(-9 * time.Minute)}
		previous := &Server{buckets: maps.Clone(current.buckets)}
		for _, step := range []struct {
			id     string
			offset time.Duration
		}{
			{"missing", 0}, {"missing", time.Minute}, {"missing", time.Minute + time.Nanosecond},
			{"1", -time.Minute}, {"second", 9 * time.Minute}, {"second", 9*time.Minute + time.Nanosecond},
			{"third", 10 * time.Minute}, {"third", 10*time.Minute + time.Nanosecond},
			{"third", -20 * time.Minute}, {"fourth", 21 * time.Minute},
		} {
			checkRateBucketPrune(t, current, previous, Client{ID: step.id, RequestsPerMinute: 10}, base.Add(step.offset))
		}
	}
}

func TestRateBucketPruneConcurrent(t *testing.T) {
	now := time.Now()
	s := rateBucketPruneServer(10001, now)
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(func() {
			for range 100 {
				if !s.allow(Client{ID: "0", RequestsPerMinute: rateBucketTestBudget}, now) {
					t.Error("同一身份的并发请求被意外拒绝")
				}
			}
		})
	}
	workers.Wait()
	if got := s.buckets["0"].tokens; got != rateBucketTestBudget-1600 {
		t.Fatalf("并发扣减结果=%v，期望=%v", got, rateBucketTestBudget-1600)
	}
}

func TestRateBucketPruneRegrowth(t *testing.T) {
	base := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	current := rateBucketPruneServer(10001, base.Add(-11*time.Minute))
	for _, id := range []string{"0", "1"} {
		current.buckets[id] = bucket{tokens: rateBucketTestBudget, updated: base}
	}
	previous := &Server{buckets: maps.Clone(current.buckets)}
	checkRateBucketPrune(t, current, previous, Client{ID: "new", RequestsPerMinute: rateBucketTestBudget}, base)
	checkRateBucketPrune(t, current, previous, Client{ID: "old", RequestsPerMinute: rateBucketTestBudget}, base.Add(-20*time.Minute))
	// 缩容后通过真实写入重新长满，核对低于容量时写入的旧时间也会触发回收。
	for i := 0; len(current.buckets) < 10001; i++ {
		client := Client{ID: "refill-" + strconv.Itoa(i), RequestsPerMinute: rateBucketTestBudget}
		if got, want := current.allow(client, base), previousRateBucketAllow(previous, client, base); got != want || !got {
			t.Fatalf("重新填充结果变化：身份=%q，结果=%v/%v", client.ID, got, want)
		}
	}
	checkRateBucketPrune(t, current, previous, Client{ID: "after-regrowth", RequestsPerMinute: rateBucketTestBudget}, base)
	if _, exists := current.buckets["old"]; exists {
		t.Fatal("重新长满后没有回收旧时间的桶")
	}
	for _, offset := range []time.Duration{10 * time.Minute, 10*time.Minute + time.Nanosecond} {
		checkRateBucketPrune(t, current, previous, Client{ID: "boundary", RequestsPerMinute: rateBucketTestBudget}, base.Add(offset))
	}
}

func TestRateBucketPruneScanBudget(t *testing.T) {
	now := time.Now()
	for _, id := range []string{"0", "missing"} {
		t.Run(id, func(t *testing.T) {
			client := Client{ID: id, RequestsPerMinute: rateBucketTestBudget}
			previous, current := rateBucketPruneServer(10001, now), rateBucketPruneServer(10001, now)
			var got bool
			baseline := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					_ = previousRateBucketAllow(previous, client, now)
				}
			})
			result := testing.Benchmark(func(b *testing.B) {
				for b.Loop() {
					got = current.allow(client, now)
				}
			})
			if got != (id == "0") {
				t.Fatalf("满桶请求结果=%v，身份=%q", got, id)
			}
			if result.NsPerOp() >= baseline.NsPerOp()/10 {
				t.Fatalf("满桶请求耗时 %d ns，未低于旧扫描 %d ns 的十分之一", result.NsPerOp(), baseline.NsPerOp())
			}
		})
	}
}

func BenchmarkRateBucketPrune(b *testing.B) {
	now := time.Now()
	for _, sample := range []struct {
		name, id      string
		size          int
		cold, expired bool
	}{
		{name: "small_existing", id: "0", size: 32},
		{name: "full_existing", id: "0", size: 10001},
		{name: "full_rejected", id: "missing", size: 10001},
		{name: "cold_full", id: "0", size: 10001, cold: true},
		{name: "expired", id: "0", size: 10001, cold: true, expired: true},
	} {
		for _, impl := range []struct {
			name string
			run  func(*Server, Client, time.Time) bool
		}{
			{"previous", previousRateBucketAllow}, {"current", (*Server).allow},
		} {
			b.Run(sample.name+"/"+impl.name, func(b *testing.B) {
				b.ReportAllocs()
				updated := now
				if sample.expired {
					updated = now.Add(-11 * time.Minute)
				}
				s := rateBucketPruneServer(sample.size, updated)
				client := Client{ID: sample.id, RequestsPerMinute: rateBucketTestBudget}
				for b.Loop() {
					if sample.cold {
						b.StopTimer()
						s = rateBucketPruneServer(sample.size, updated)
						b.StartTimer()
					}
					_ = impl.run(s, client, now)
				}
			})
		}
	}
}
