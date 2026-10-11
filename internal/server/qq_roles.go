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

// 群管理员身份。开放平台的「获取群成员信息」接口（GET /v2/groups/{group_openid}/members/{member_openid}）返回 member_role：owner 群主、admin 管理员、member 普通成员。这个接口还在内邀阶段，没进白名单的机器人调用时返回错误码 11253；在那之前只认 qq_bot.admins 的名单。查到的身份在本副本缓存 qqRoleTTL；接口返回 11253 后 qqRoleUnavailableTTL 内不再调用，白名单开通后不用重新部署就会自动生效。

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
}

type qqRoleEntry struct {
	group string
	admin bool
	at    time.Time
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

// qqIsAdmin 判断发消息的人是不是管理员：在 qq_bot.admins 里，或者群身份接口说他是群主或管理员。接口不可用或出错时按普通成员处理。
func (s *Server) qqIsAdmin(ctx context.Context, group, member string) bool {
	if slices.Contains(s.qq.config.Admins, member) {
		return true
	}
	r := &s.qq.roles
	now := s.qq.now()
	key := group + "/" + member
	r.mu.Lock()
	if entry, ok := r.entries[key]; ok && now.Sub(entry.at) < qqRoleTTL {
		r.mu.Unlock()
		return entry.admin
	}
	unavailable := now.Before(r.unavailableUntil)
	r.mu.Unlock()
	if unavailable {
		return false
	}
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
		slog.Warn("qq bot: member role lookup failed", "reason", err.Error())
		return false
	}
	admin := role == "owner" || role == "admin"
	if r.entries == nil {
		r.entries = map[string]qqRoleEntry{}
	}
	r.entries[key] = qqRoleEntry{group: group, admin: admin, at: now}
	return admin
}

// qqGroupAdmins 是答不上问题时要 @ 的人：qq_bot.admins，加上群身份接口识别出的本群管理员（按 member_openid 排序，去重）。
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
