package server

import (
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// inputTimezoneCacheLimit 限制各副本保留的时区元数据数量。
const inputTimezoneCacheLimit = 64

var errInputTimezoneName = errors.New("invalid timezone name")

// inputTimezoneCache 只缓存成功加载的只读时区；满额后依次淘汰最早载入的条目。
type inputTimezoneCache struct {
	mu        sync.Mutex
	loads     singleflight.Group
	locations map[string]*time.Location
	order     [inputTimezoneCacheLimit]string
	next      int
}

func (c *inputTimezoneCache) load(name string) (*time.Location, error) {
	if len(name) > 128 {
		return nil, errInputTimezoneName
	}
	// 标准库为这些名称直接返回共享对象，无需占用缓存。
	if name == "" || name == "UTC" || name == "Local" {
		return time.LoadLocation(name)
	}
	if location := c.cached(name); location != nil {
		return location, nil
	}
	// 按名称合并冷加载，读文件时不占缓存锁，已缓存的其他时区无需等待。
	loaded, err, _ := c.loads.Do(name, func() (any, error) {
		if location := c.cached(name); location != nil {
			return location, nil
		}
		location, err := time.LoadLocation(name)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.locations == nil {
			c.locations = make(map[string]*time.Location)
		}
		delete(c.locations, c.order[c.next])
		c.order[c.next] = name
		c.locations[name] = location
		c.next = (c.next + 1) % len(c.order)
		return location, nil
	})
	if err != nil {
		return nil, err
	}
	return loaded.(*time.Location), nil
}

func (c *inputTimezoneCache) cached(name string) *time.Location {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.locations[name]
}
