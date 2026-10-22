package server

import (
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestInputDatetimeRepeatedTimezoneAllocationBudget(t *testing.T) {
	s := &Server{}
	allocations := func(body string) float64 {
		return testing.AllocsPerRun(100, func() {
			r := httptest.NewRequest("POST", "/v1/input/datetime", strings.NewReader(body))
			r.SetPathValue("operation", "datetime")
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.inputQuery(w, r)
			// 未配置引擎时，合法日期时间仍需完成时区与日期参数转换后才返回 503。
			if w.Code != 503 {
				t.Fatalf("日期时间请求返回 %d：%s", w.Code, w.Body.String())
			}
		})
	}
	utc := allocations(`{"text":"date","time":"2026-09-08T18:00:00Z","timezone":"UTC"}`)
	budget := utc
	// 竞态构建会随机丢弃 JSON 编码对象池；时区缓存本身另有零分配断言。
	if raceEnabled {
		budget += 2
	}
	for _, zone := range []string{"Asia/Shanghai", "America/New_York", "Europe/London"} {
		body := `{"text":"date","time":"2026-09-08T18:00:00Z","timezone":"` + zone + `"}`
		if got := allocations(body); got > budget {
			t.Fatalf("重复使用时区 %q 分配 %.0f 次，UTC 分配 %.0f 次，不应重复加载时区数据", zone, got, utc)
		}
	}
}

func TestInputTimezoneCacheLocationsMatchStandardLibrary(t *testing.T) {
	var cache inputTimezoneCache
	for _, name := range []string{"", "UTC", "Local", "Asia/Shanghai", "Asia/Tokyo", "America/New_York", "Europe/London"} {
		got, err := cache.load(name)
		if err != nil {
			t.Fatal(err)
		}
		want, err := time.LoadLocation(name)
		if err != nil {
			t.Fatal(err)
		}
		for _, stamp := range []string{
			"2026-01-01T00:00:00Z", "2026-03-08T06:59:59Z", "2026-03-08T07:00:00Z",
			"2026-07-01T00:00:00Z", "2026-09-08T18:00:00Z", "2026-11-01T05:59:59Z", "2026-11-01T06:00:00Z",
		} {
			instant, err := time.Parse(time.RFC3339, stamp)
			if err != nil {
				t.Fatal(err)
			}
			if actual, expected := instant.In(got).Format(time.RFC3339), instant.In(want).Format(time.RFC3339); actual != expected {
				t.Fatalf("时区 %q 在 %s 转换变化：got=%s want=%s", name, stamp, actual, expected)
			}
		}
		if again, err := cache.load(name); err != nil || again != got {
			t.Fatalf("时区 %q 未复用已加载对象：%v", name, err)
		}
		allocations := testing.AllocsPerRun(100, func() {
			if _, err := cache.load(name); err != nil {
				t.Fatal(err)
			}
		})
		if allocations != 0 {
			t.Fatalf("重复使用时区 %q 分配 %.0f 次，期望零分配", name, allocations)
		}
	}
	if len(cache.locations) != 4 {
		t.Fatalf("标准库共享时区不应占用缓存：缓存了 %d 条", len(cache.locations))
	}
}

func TestInputTimezoneCacheDoesNotStoreFailures(t *testing.T) {
	var cache inputTimezoneCache
	for _, name := range []string{"invalid/zone", "../invalid", "\x00", strings.Repeat("x", 129)} {
		for range 2 {
			if location, err := cache.load(name); err == nil || location != nil {
				t.Fatalf("非法时区 %q 未被拒绝：%v", name, err)
			}
		}
	}
	if cache.locations != nil || cache.next != 0 {
		t.Fatal("失败的时区加载修改了缓存")
	}
}

func TestInputTimezoneCacheSharesConcurrentLoads(t *testing.T) {
	for _, names := range [][]string{
		{"America/New_York"},
		{"America/New_York", "Asia/Shanghai", "Europe/London"},
	} {
		var cache inputTimezoneCache
		start := make(chan struct{})
		locations := make([]*time.Location, 64)
		errors := make([]error, len(locations))
		var workers sync.WaitGroup
		for i := range locations {
			workers.Go(func() {
				<-start
				locations[i], errors[i] = cache.load(names[i%len(names)])
			})
		}
		close(start)
		workers.Wait()
		loaded := map[string]*time.Location{}
		for i, location := range locations {
			name := names[i%len(names)]
			if errors[i] != nil || location == nil || location.String() != name {
				t.Fatalf("并发请求 %d 的时区加载失败：%v", i, errors[i])
			}
			if previous := loaded[name]; previous != nil && location != previous {
				t.Fatalf("并发请求 %d 没有复用时区 %q 的对象", i, name)
			}
			loaded[name] = location
		}
	}
}

func TestInputTimezoneCacheEvictsOldest(t *testing.T) {
	var cache inputTimezoneCache
	// 使用 Go 自带时区数据库中的名称，测试不依赖宿主机额外安装时区文件。
	zones := []string{
		"Africa/Abidjan", "Africa/Accra", "Africa/Addis_Ababa", "Africa/Algiers",
		"Africa/Asmara", "Africa/Asmera", "Africa/Bamako", "Africa/Bangui",
		"Africa/Banjul", "Africa/Bissau", "Africa/Blantyre", "Africa/Brazzaville",
		"Africa/Bujumbura", "Africa/Cairo", "Africa/Casablanca", "Africa/Ceuta",
		"Africa/Conakry", "Africa/Dakar", "Africa/Dar_es_Salaam", "Africa/Djibouti",
		"Africa/Douala", "Africa/El_Aaiun", "Africa/Freetown", "Africa/Gaborone",
		"Africa/Harare", "Africa/Johannesburg", "Africa/Juba", "Africa/Kampala",
		"Africa/Khartoum", "Africa/Kigali", "Africa/Kinshasa", "Africa/Lagos",
		"Africa/Libreville", "Africa/Lome", "Africa/Luanda", "Africa/Lubumbashi",
		"Africa/Lusaka", "Africa/Malabo", "Africa/Maputo", "Africa/Maseru",
		"Africa/Mbabane", "Africa/Mogadishu", "Africa/Monrovia", "Africa/Nairobi",
		"Africa/Ndjamena", "Africa/Niamey", "Africa/Nouakchott", "Africa/Ouagadougou",
		"Africa/Porto-Novo", "Africa/Sao_Tome", "Africa/Timbuktu", "Africa/Tripoli",
		"Africa/Tunis", "Africa/Windhoek", "America/Adak", "America/Anchorage",
		"America/Anguilla", "America/Antigua", "America/Araguaina", "America/Argentina/Buenos_Aires",
		"America/Argentina/Catamarca", "America/Argentina/ComodRivadavia", "America/Argentina/Cordoba", "America/Argentina/Jujuy",
		"America/Argentina/La_Rioja",
	}
	if len(zones) <= inputTimezoneCacheLimit {
		t.Fatal("淘汰测试需要超过缓存上限的时区名称")
	}
	var first, last *time.Location
	for i, name := range zones {
		location, err := cache.load(name)
		if err != nil {
			t.Fatalf("加载测试时区 %q：%v", name, err)
		}
		if i == 0 {
			first = location
		}
		last = location
		if len(cache.locations) > inputTimezoneCacheLimit {
			t.Fatalf("时区缓存超过上限：%d", len(cache.locations))
		}
	}
	if len(cache.locations) != inputTimezoneCacheLimit {
		t.Fatalf("时区缓存意外清空：%d", len(cache.locations))
	}
	if reloaded, err := cache.load(zones[0]); err != nil || reloaded == first {
		t.Fatalf("最早加载的时区未被淘汰：%v", err)
	}
	if retained, err := cache.load(zones[len(zones)-1]); err != nil || retained != last {
		t.Fatalf("最近加载的时区没有保留：%v", err)
	}
}

func TestInputDatetimeRejectsInvalidTimezone(t *testing.T) {
	s := &Server{}
	for _, name := range []string{"invalid/zone", strings.Repeat("x", 129)} {
		r := httptest.NewRequest("POST", "/v1/input/datetime", strings.NewReader(`{"text":"date","timezone":"`+name+`"}`))
		r.SetPathValue("operation", "datetime")
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.inputQuery(w, r)
		if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":"invalid_timezone"`) {
			t.Fatalf("非法时区未保留原有错误格式：%d %s", w.Code, w.Body.String())
		}
	}
}

func BenchmarkInputTimezoneLocations(b *testing.B) {
	for _, name := range []string{"UTC", "Asia/Shanghai", "America/New_York", "Europe/London"} {
		var cache inputTimezoneCache
		if _, err := cache.load(name); err != nil {
			b.Fatal(err)
		}
		for _, tc := range []struct {
			name string
			load func(string) (*time.Location, error)
		}{
			{"previous", time.LoadLocation},
			{"current", cache.load},
		} {
			b.Run(name+"/"+tc.name, func(b *testing.B) {
				b.ReportAllocs()
				for b.Loop() {
					if location, err := tc.load(name); err != nil || location.String() != name {
						b.Fatal("时区加载结果变化", err)
					}
				}
			})
		}
	}
}
