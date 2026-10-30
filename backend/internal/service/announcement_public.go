package service

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// publicPopupCacheTTL 匿名弹窗公告列表的缓存时间。管理员增删改公告时会立即失效。
const publicPopupCacheTTL = 30 * time.Second

type publicPopupCache struct {
	mu        sync.Mutex
	items     []Announcement
	expiresAt time.Time
}

func (c *publicPopupCache) get(now time.Time) ([]Announcement, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.expiresAt.IsZero() || !now.Before(c.expiresAt) {
		return nil, false
	}
	return c.items, true
}

func (c *publicPopupCache) set(items []Announcement, expiresAt time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = items
	c.expiresAt = expiresAt
}

func (c *publicPopupCache) invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items = nil
	c.expiresAt = time.Time{}
}

// IsPublicPopup 判断公告是否可以对匿名访客公开弹出：
// 处于生效期、弹窗模式，且没有任何定向规则（即面向全部用户）。
// 带定向规则（按订阅/余额）的公告依赖用户数据，绝不对匿名访客暴露。
func IsPublicPopup(a *Announcement, now time.Time) bool {
	if a == nil {
		return false
	}
	if a.NotifyMode != AnnouncementNotifyModePopup {
		return false
	}
	if len(a.Targeting.AnyOf) > 0 {
		return false
	}
	return a.IsActiveAt(now)
}

// ListPublicPopups 返回可对匿名访客展示的弹窗公告（按 ID 倒序）。
// 不涉及任何用户数据；结果短时缓存，降低匿名请求对数据库的压力。
func (s *AnnouncementService) ListPublicPopups(ctx context.Context) ([]Announcement, error) {
	now := time.Now()
	if cached, ok := s.publicPopupCache.get(now); ok {
		return filterPublicPopups(cached, now), nil
	}

	anns, err := s.announcementRepo.ListActive(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("list active announcements: %w", err)
	}

	out := filterPublicPopups(anns, now)
	s.publicPopupCache.set(out, now.Add(publicPopupCacheTTL))
	return out, nil
}

func filterPublicPopups(anns []Announcement, now time.Time) []Announcement {
	out := make([]Announcement, 0, len(anns))
	for i := range anns {
		if IsPublicPopup(&anns[i], now) {
			out = append(out, anns[i])
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID > out[j].ID })
	return out
}
