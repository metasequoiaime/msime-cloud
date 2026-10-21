package account

import (
	"math/rand/v2"
	"strings"
	"testing"
)

// 保留未建立前缀索引的逐字段扫描，独立对照识别结果与扫描成本。
func previousCrashSystemPrefixScan(frame string) bool {
	for field := range strings.FieldsSeq(frame) {
		field = strings.TrimLeft(field, "-+[(")
		for _, prefix := range crashSystemPrefixes {
			if strings.HasPrefix(field, prefix) {
				return true
			}
		}
	}
	return false
}

func TestCrashSystemPrefixCompatibility(t *testing.T) {
	random := rand.New(rand.NewPCG(17, 31))
	for i := range 2000 {
		var frame strings.Builder
		for range 1 + random.IntN(8) {
			frame.WriteString(" \t\u2003")
			frame.WriteString(strings.Repeat("-+[(", random.IntN(3)))
			if i%3 == 0 {
				prefix := crashSystemPrefixes[random.IntN(len(crashSystemPrefixes))]
				frame.WriteString(prefix[:random.IntN(len(prefix)+1)])
			} else {
				for range random.IntN(32) {
					frame.WriteByte(byte(random.IntN(256)))
				}
			}
		}
		if got, want := crashSystemFrame(frame.String()), previousCrashSystemPrefixScan(frame.String()); got != want {
			t.Fatalf("栈帧 %q 的系统识别变化：结果=%t，期望=%t", frame.String(), got, want)
		}
	}
}

func TestCrashSystemPrefixScanBudget(t *testing.T) {
	frame := strings.Repeat("msime keyboard.layout() application::dispatch ", 8)
	measure := func(match func(string) bool) testing.BenchmarkResult {
		return testing.Benchmark(func(b *testing.B) {
			for b.Loop() {
				if match(frame) {
					b.Fatal("应用栈帧被识别为系统帧")
				}
			}
		})
	}
	baseline, current := measure(previousCrashSystemPrefixScan), measure(crashSystemFrame)
	t.Logf("原扫描：%s；当前：%s", baseline, current)
	// Go 不给 _test.go 中的旧实现插桩，生产函数的覆盖率计数会使两边的计时条件不同。
	// 覆盖率运行仍执行上述真实函数；两倍耗时门槛由 Linux、Windows 的常规竞态 CI 执行。
	if mode := testing.CoverMode(); mode != "" {
		t.Logf("覆盖率模式 %s 不比较插桩条件不同的耗时，原门槛由未启用覆盖率的测试执行", mode)
		return
	}
	// 多字段应用帧会遍历全部前缀；留出一倍耗时余量，检出重新退化为全表扫描。
	if current.NsPerOp() >= baseline.NsPerOp()/2 {
		t.Fatalf("系统前缀匹配耗时 %d ns，未低于旧扫描 %d ns 的一半", current.NsPerOp(), baseline.NsPerOp())
	}
}

func BenchmarkCrashSystemPrefixIndex(b *testing.B) {
	for _, tc := range []struct{ name, frame string }{
		{"应用帧", "msime keyboard.layout()"},
		{"首前缀命中", "libsystem_kernel.dylib"},
		{"末尾前缀命中", "module <unknown>"},
		{"多字段应用帧", strings.Repeat("msime keyboard.layout() application::dispatch ", 8)},
	} {
		for _, impl := range []struct {
			name  string
			match func(string) bool
		}{{"原扫描", previousCrashSystemPrefixScan}, {"当前", crashSystemFrame}} {
			b.Run(tc.name+"/"+impl.name, func(b *testing.B) {
				want := previousCrashSystemPrefixScan(tc.frame)
				b.ReportAllocs()
				for b.Loop() {
					if got := impl.match(tc.frame); got != want {
						b.Fatal("系统栈帧识别结果改变")
					}
				}
			})
		}
	}
}
