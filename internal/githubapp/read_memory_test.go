package githubapp

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const readCacheBodyBudget = 64 << 20

func cachedBodyCapacity(c *Cache) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	size := 0
	for _, entry := range c.reads {
		size += cap(entry.response.Body)
	}
	return size
}

func TestGetBoundsCachedResponseMemory(t *testing.T) {
	body := bytes.Repeat([]byte("x"), 4<<20)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write(body)
	}))
	t.Cleanup(upstream.Close)
	c := Client{APIURL: upstream.URL, HTTP: upstream.Client(), Cache: &Cache{}}
	for index := range 17 {
		path := fmt.Sprintf("/repos/example/docs/contents/%d", index)
		r, err := c.Get(t.Context(), "synthetic-token", path)
		if err != nil || !r.OK() || !bytes.Equal(r.Body, body) {
			t.Fatal("大响应读取失败", r.Status, err)
		}
		if size := cachedBodyCapacity(c.Cache); size > readCacheBodyBudget {
			t.Fatalf("仅 %d 个响应就保留了 %d 字节正文缓冲，超过 64 MiB 预算", index+1, size)
		}
	}
	if _, err := c.Get(t.Context(), "synthetic-token", "/repos/example/docs/contents/16"); err != nil || calls.Load() != 17 {
		t.Fatal("容量淘汰后最新响应没有留在缓存", calls.Load(), err)
	}
}

func TestReadCacheReleasesBodyBudget(t *testing.T) {
	// 两条缓冲恰好填满预算；只暴露一个字节，确保按底层容量计量。
	body := make([]byte, 1, readCacheBodyBudget/2)
	now := time.Now()
	for _, scenario := range []string{"替换", "路径失效", "全部失效", "过期淘汰"} {
		t.Run(scenario, func(t *testing.T) {
			c := Client{APIURL: "https://github.example.test", Cache: &Cache{}, Now: func() time.Time { return now }}
			key := c.apiURL() + "/repos/example/docs/old"
			entry := cachedRead{response: Response{Status: 200, Body: body}, fetched: now}
			c.store(key, entry, 0)
			switch scenario {
			case "替换":
				for range 3 {
					c.store(key, entry, 0)
				}
			case "路径失效", "全部失效":
				prefix := "/repos/example/docs/"
				if scenario == "全部失效" {
					prefix = ""
				}
				c.Invalidate(prefix)
				c.store(key, entry, 1)
			case "过期淘汰":
				entry.fetched = now.Add(-ReadTTL)
				c.store(key, entry, 0)
			}
			generation := c.Cache.readGeneration
			entry.fetched = now
			c.store(c.apiURL()+"/repos/example/docs/fresh", entry, generation)
			if scenario == "过期淘汰" {
				c.store(c.apiURL()+"/repos/example/docs/small", cachedRead{response: Response{Body: []byte("small")}, fetched: now}, generation)
				if _, found := c.Cache.reads[key]; found {
					t.Error("正文超限时没有优先清除过期响应")
				}
			}
			if len(c.Cache.reads) != 2 || cachedBodyCapacity(c.Cache) > readCacheBodyBudget {
				t.Fatal("释放或替换正文后预算计算错误", len(c.Cache.reads), cachedBodyCapacity(c.Cache))
			}
		})
	}
}
