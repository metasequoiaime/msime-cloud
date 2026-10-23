package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"
)

// 群管理员身份。开放平台的「获取群成员信息」接口（GET /v2/groups/{group_openid}/members/{member_openid}）返回 member_role：owner 群主、admin 管理员、member 普通成员。这个接口还在内邀阶段，没进白名单的机器人调用时返回错误码 11253；在那之前只认 qq_bot.admins 的名单。查到的身份在本副本缓存 qqRoleTTL；接口返回 11253 后 qqRoleUnavailableTTL 内不再调用，白名单开通后不用重新部署就会自动生效。@ 消息事件的 author 里也可能带 member_role，有就直接用，不再调接口。

// 群身份只在 qq_bot.groups 列出的群里算数：没有列出群时，任何人自己建个群、把机器人拉进去就是群主，机器人公开后就能「提issue」。没有配置 groups 时只认名单，并且每个群记一次日志（带 group_openid），方便把要信任的群填进配置。

const (
	qqRoleTTL            = 10 * time.Minute
	qqRoleUnavailableTTL = time.Hour
	// qqNoPermission 是开放平台「应用无接口访问权限」的错误码。
	qqNoPermission = 11253
)

// qqRoles 是本副本缓存的群成员身份，键是 group_openid/member_openid。
type qqRoles struct {
	mu               sync.Mutex
	entries          map[string]qqRoleEntry
	unavailableUntil time.Time
	// unlisted 记下已经打过「群身份不算数」日志的群。
	unlisted map[string]bool
	// sequence 在锁内为事件和查询分配顺序，避免较早发出的查询覆盖新身份。
	sequence uint64
}

type qqRoleEntry struct {
	group    string
	admin    bool
	at       time.Time
	sequence uint64
}

// qqAPIError 是开放平台接口的失败：HTTP 状态和错误码。
type qqAPIError struct {
	Status int
	Code   int
}

func (e *qqAPIError) Error() string {
	return fmt.Sprintf("qq bot: api answered %d (code %d)", e.Status, e.Code)
}

// memberRole 查询群成员的身份（owner、admin 或 member）。
func (b *qqBot) memberRole(ctx context.Context, group, member string) (string, error) {
	token, err := b.accessToken(ctx)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.config.APIURL+"/v2/groups/"+group+"/members/"+member, nil)
	if err != nil {
		return "", errors.New("qq bot: invalid member endpoint")
	}
	req.Header.Set("Authorization", "QQBot "+token)
	resp, err := b.client.Do(req)
	if err != nil {
		return "", errors.New("qq bot: member request failed")
	}
	defer resp.Body.Close()
	var result struct {
		Code       int    `json:"code"`
		MemberRole string `json:"member_role"`
	}
	decodeErr := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result)
	if resp.StatusCode != http.StatusOK {
		return "", &qqAPIError{Status: resp.StatusCode, Code: result.Code}
	}
	if decodeErr != nil || result.MemberRole == "" {
		return "", errors.New("qq bot: invalid member response")
	}
	return result.MemberRole, nil
}

// qqIsAdmin 判断发消息的人是不是管理员：在 qq_bot.admins 里，或者在 qq_bot.groups 列出的群里，事件带的 eventRole（为空时查群身份接口）说他是群主或管理员。接口不可用时按普通成员处理；临时出错时沿用过期的缓存，没有缓存才按普通成员处理。
func (s *Server) qqIsAdmin(ctx context.Context, group, member, eventRole string) bool {
	r := &s.qq.roles
	now := s.qq.now()
	key := group + "/" + member
	r.mu.Lock()
	listed := slices.Contains(s.qq.config.Groups, group)
	// 先记日志再看名单：名单里的管理员 @ 机器人时也要把群标识记下来，否则只有普通群友发言后才拿得到。
	if !listed && !r.unlisted[group] {
		if r.unlisted == nil {
			r.unlisted = map[string]bool{}
		}
		r.unlisted[group] = true
		slog.Info("qq bot: group roles ignored, group not in qq_bot.groups", "group_openid", group)
	}
	if slices.Contains(s.qq.config.Admins, member) {
		r.mu.Unlock()
		return true
	}
	if !listed {
		r.mu.Unlock()
		return false
	}
	if eventRole != "" {
		admin := eventRole == "owner" || eventRole == "admin"
		if r.entries == nil {
			r.entries = map[string]qqRoleEntry{}
		}
		r.sequence++
		r.entries[key] = qqRoleEntry{group: group, admin: admin, at: now, sequence: r.sequence}
		r.mu.Unlock()
		return admin
	}
	if entry, ok := r.entries[key]; ok && now.Sub(entry.at) < qqRoleTTL {
		r.mu.Unlock()
		return entry.admin
	}
	if now.Before(r.unavailableUntil) {
		r.mu.Unlock()
		return false
	}
	r.sequence++
	sequence := r.sequence
	r.mu.Unlock()
	role, err := s.qq.memberRole(ctx, group, member)
	r.mu.Lock()
	defer r.mu.Unlock()
	var apiErr *qqAPIError
	switch {
	case errors.As(err, &apiErr) && apiErr.Code == qqNoPermission:
		if !now.Before(r.unavailableUntil) {
			slog.Info("qq bot: member roles unavailable, using qq_bot.admins only", "code", apiErr.Code)
		}
		r.unavailableUntil = now.Add(qqRoleUnavailableTTL)
		return false
	case err != nil:
		// 接口临时出错时沿用过期的缓存：管理员刚被认出来，十分钟后接口抖一下就被当成普通成员，「提issue」突然不能用了。从没查到过时才按普通成员处理。
		slog.Warn("qq bot: member role lookup failed", "reason", err.Error())
		if entry, ok := r.entries[key]; ok {
			return entry.admin
		}
		return false
	}
	// 等待 HTTP 响应时可能已有事件或较晚发出的查询更新了同一成员，旧结果不能恢复或撤销新权限。
	if entry, ok := r.entries[key]; ok && entry.sequence > sequence {
		return entry.admin
	}
	admin := role == "owner" || role == "admin"
	if r.entries == nil {
		r.entries = map[string]qqRoleEntry{}
	}
	r.entries[key] = qqRoleEntry{group: group, admin: admin, at: now, sequence: sequence}
	return admin
}

// qqGroupAdmins 是要请来跟进的管理员：qq_bot.admins，加上群身份接口识别出的本群管理员（按 member_openid 排序，去重）。QQ 群文本消息里的 @ 标签实测原样显示，回复里暂时不 @ 人，这个列表留给确认了 @ 写法之后使用。
func (s *Server) qqGroupAdmins(group string) []string {
	admins := append([]string(nil), s.qq.config.Admins...)
	r := &s.qq.roles
	r.mu.Lock()
	var found []string
	for key, entry := range r.entries {
		if entry.admin && entry.group == group {
			found = append(found, key[len(group)+1:])
		}
	}
	r.mu.Unlock()
	slices.Sort(found)
	for _, member := range found {
		if !slices.Contains(admins, member) {
			admins = append(admins, member)
		}
	}
	return admins
}
