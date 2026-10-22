package server

import (
	"bufio"
	"io"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"
)

func benchmarkSSELineScan(b *testing.B, input string, oneByte bool) {
	b.ReportAllocs()
	b.SetBytes(int64(len(input)))
	want := strings.TrimSuffix(input, "\r\n")
	for b.Loop() {
		var reader io.Reader = strings.NewReader(input)
		if oneByte {
			reader = iotest.OneByteReader(reader)
		}
		scanner := bufio.NewScanner(reader)
		scanner.Split(splitSSELines())
		if !scanner.Scan() || string(scanner.Bytes()) != want {
			b.Fatal("长行未完整读出")
		}
		if scanner.Scan() || scanner.Err() != nil {
			b.Fatalf("长行结束位置不正确：%v", scanner.Err())
		}
	}
}

func BenchmarkSSELineScan(b *testing.B) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"1KiB", 1024},
		{"32KiB", 32 * 1024},
	} {
		for _, oneByte := range []bool{false, true} {
			name := tc.name + "/整段读取"
			if oneByte {
				name = tc.name + "/逐字节读取"
			}
			b.Run(name, func(b *testing.B) {
				benchmarkSSELineScan(b, strings.Repeat("x", tc.size)+"\r\n", oneByte)
			})
		}
	}
}

func TestSSELineFragmentedScanTime(t *testing.T) {
	result := testing.Benchmark(func(b *testing.B) {
		benchmarkSSELineScan(b, strings.Repeat("x", 32*1024)+"\r\n", true)
	})
	budget := 50 * time.Millisecond
	if raceEnabled {
		// 竞态构建逐字节读取的检测开销较高，单独预留余量。
		budget = 500 * time.Millisecond
	}
	if elapsed := time.Duration(result.NsPerOp()); elapsed > budget {
		t.Fatalf("逐字节读取 32 KiB 行耗时 %s，超过 %s 预算", elapsed, budget)
	}
}

func TestSSELongLinesAcrossReads(t *testing.T) {
	long := strings.Repeat("x", 32*1024)
	input := "头\r\n" + long + "\r\n\r\n短\r" + long + "\n尾\n" + long
	want := []string{"头", long, "", "短", long, "尾", long}
	for _, mode := range []string{"整段读取", "半段读取", "逐字节读取"} {
		t.Run(mode, func(t *testing.T) {
			var reader io.Reader = strings.NewReader(input)
			switch mode {
			case "半段读取":
				reader = iotest.HalfReader(reader)
			case "逐字节读取":
				reader = iotest.OneByteReader(reader)
			}
			scanner := bufio.NewScanner(reader)
			scanner.Buffer(make([]byte, 8), len(long)+16)
			scanner.Split(splitSSELines())
			var got []string
			for scanner.Scan() {
				got = append(got, scanner.Text())
			}
			if err := scanner.Err(); err != nil || !slices.Equal(got, want) {
				t.Fatalf("SSE 长行内容或边界不正确：行数=%d，期望=%d，错误=%v", len(got), len(want), err)
			}
		})
	}
}
