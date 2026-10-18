package server

import (
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"testing"
)

func fillQQEventProbe(b *qqBot) {
	for i := range qqFieldsProbeMax {
		b.probeEventFields(json.RawMessage(`{"synthetic_` + strconv.Itoa(i) + `":null}`))
	}
}

func qqEventProbeCapacityPayload() json.RawMessage {
	return json.RawMessage(`{"content":"` + strings.Repeat("合成消息", 8192) + `","message_reference":{"message_id":"synthetic-reference"},"message_type":103}`)
}

func TestQQEventProbeCapacitySkipsAllocations(t *testing.T) {
	logs := captureLogs(t)
	b := &qqBot{}
	fillQQEventProbe(b)
	before := logs.String()
	if got := strings.Count(before, `"msg":"qq bot: event fields probe"`); got != qqFieldsProbeMax {
		t.Fatalf("达到容量前记录 %d 种字段，期望 %d 种", got, qqFieldsProbeMax)
	}
	raw := qqEventProbeCapacityPayload()
	allocations := testing.AllocsPerRun(100, func() { b.probeEventFields(raw) })
	if allocations != 0 {
		t.Errorf("字段探测达到容量后仍分配 %.0f 次，期望不再为忽略的消息分配", allocations)
	}
	if logs.String() != before {
		t.Error("字段探测达到容量后仍写入日志")
	}
}

func TestQQEventProbeCapacityConcurrent(t *testing.T) {
	logs := captureLogs(t)
	b := &qqBot{}
	var ready, done sync.WaitGroup
	start := make(chan struct{})
	for i := range 2 * qqFieldsProbeMax {
		ready.Add(1)
		done.Add(1)
		go func() {
			defer done.Done()
			raw := json.RawMessage(`{"synthetic_` + strconv.Itoa(i) + `":"synthetic-private-message"}`)
			ready.Done()
			<-start
			b.probeEventFields(raw)
		}()
	}
	ready.Wait()
	close(start)
	done.Wait()
	output := logs.String()
	if got := strings.Count(output, `"msg":"qq bot: event fields probe"`); got != qqFieldsProbeMax {
		t.Fatalf("并发探测记录 %d 种字段，期望 %d 种", got, qqFieldsProbeMax)
	}
	if strings.Contains(output, "synthetic-private-message") {
		t.Error("探测日志包含消息正文")
	}
	b.probeEventFields(qqEventProbeCapacityPayload())
	if logs.String() != output {
		t.Error("并发达到容量后仍记录新字段")
	}
}

func BenchmarkQQEventProbeAfterCapacity(b *testing.B) {
	bot := &qqBot{}
	fillQQEventProbe(bot)
	raw := qqEventProbeCapacityPayload()
	b.ReportAllocs()
	for b.Loop() {
		bot.probeEventFields(raw)
	}
}
