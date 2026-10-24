package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/metasequoiaime/MSIME-Backend/docs"
	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/server"
)

// operation is one method on one path: from the published OpenAPI document for /v1, or from the admin site's route tables for /api, where Method "*" is a route taking more than one method.
type operation struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Auth    string `json:"auth"`
	Summary string `json:"summary"`
	// The OpenAPI operation object.
	Detail json.RawMessage `json:"detail,omitempty"`
	// For an admin route, what docs/admin.md says about it.
	Guide []string `json:"guide,omitempty"`
}

const (
	authNone         = "none"
	authUser         = "user"
	authOptionalUser = "optional-user"
	authAny          = "device-or-user"
	authAdmin        = "admin"
	adminPrefix      = "/api/"
)

// catalog is every operation the CLI knows: /v1 from the OpenAPI document, /api from the tables the admin site dispatches with.
func catalog() ([]operation, error) {
	var spec struct {
		Security json.RawMessage                       `json:"security"`
		Paths    map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(server.OpenAPI(), &spec); err != nil {
		return nil, fmt.Errorf("the embedded API description is unreadable: %w", err)
	}
	var operations []operation
	for path, methods := range spec.Paths {
		for method, raw := range methods {
			switch method {
			case "get", "post", "put", "patch", "delete":
			default:
				continue
			}
			var detail struct {
				Summary  string          `json:"summary"`
				Security json.RawMessage `json:"security"`
			}
			if err := json.Unmarshal(raw, &detail); err != nil {
				return nil, fmt.Errorf("the API description of %s %s is unreadable: %w", method, path, err)
			}
			security := detail.Security
			if security == nil {
				security = spec.Security
			}
			operations = append(operations, operation{Method: strings.ToUpper(method), Path: path, Auth: authOf(security), Summary: detail.Summary, Detail: raw})
		}
	}
	for _, route := range server.AdminRouteList() {
		operations = append(operations, operation{Method: route.Method, Path: route.Path, Auth: authAdmin, Summary: adminSummary(route)})
	}
	sort.Slice(operations, func(i, j int) bool {
		if operations[i].Path != operations[j].Path {
			return operations[i].Path < operations[j].Path
		}
		return operations[i].Method < operations[j].Method
	})
	return operations, nil
}

// authOf 根据 OpenAPI 的安全要求区分匿名、可选用户会话和必需的凭据。
func authOf(security json.RawMessage) string {
	var requirements []map[string]json.RawMessage
	_ = json.Unmarshal(security, &requirements)
	if len(requirements) == 0 {
		return authNone
	}
	optional, user := false, false
	for _, requirement := range requirements {
		optional = optional || len(requirement) == 0
		if _, ok := requirement["userSession"]; ok {
			user = true
		}
		if _, ok := requirement["deviceToken"]; ok {
			return authAny
		}
	}
	if optional {
		if user {
			return authOptionalUser
		}
		return authNone
	}
	return authUser
}

// find 按方法和路径查找接口，先去掉不会参与服务端路由的片段与查询；字面量段优先于参数段。
func find(operations []operation, method, path string) (operation, bool) {
	path, _, _ = strings.Cut(path, "#")
	path, _, _ = strings.Cut(path, "?")
	best, bestScore := operation{}, -1
	for _, candidate := range operations {
		if candidate.Method != method && candidate.Method != "*" {
			continue
		}
		if score, ok := match(candidate.Path, path); ok && score > bestScore {
			best, bestScore = candidate, score
		}
	}
	return best, bestScore >= 0
}

// match 逐段判断路径是否符合模板，按匹配的字面量段数评分。
func match(template, path string) (int, bool) {
	want, have := strings.Trim(template, "/"), strings.Trim(path, "/")
	score := 0
	haveSegment := true
	for {
		segment, rest, more := strings.Cut(want, "/")
		if strings.HasPrefix(segment, "{") && strings.HasSuffix(segment, "...}") {
			return score, haveSegment
		}
		if !haveSegment {
			return 0, false
		}
		var actual string
		actual, have, haveSegment = strings.Cut(have, "/")
		// 与 ServeMux 一样先分段再解码，避免把 %2F 当成分隔符或重复解码 %25。
		literal, err := url.PathUnescape(segment)
		if err != nil {
			return 0, false
		}
		actual, err = url.PathUnescape(actual)
		if err != nil {
			return 0, false
		}
		if literal == actual {
			score++
		} else if !strings.HasPrefix(segment, "{") || actual == "" {
			return 0, false
		}
		if !more {
			return score, !haveSegment
		}
		want = rest
	}
}

func listRoutes(out io.Writer, filter string) error {
	operations, err := catalog()
	if err != nil {
		return err
	}
	filter = strings.ToLower(filter)
	for _, op := range operations {
		line := fmt.Sprintf("%-6s %-52s %-14s %s", op.Method, op.Path, op.Auth, op.Summary)
		if filter == "" || strings.Contains(strings.ToLower(line), filter) {
			if _, err := fmt.Fprintln(out, strings.TrimRight(line, " ")); err != nil {
				return err
			}
		}
	}
	return nil
}

func describe(out io.Writer, method, path string) error {
	operations, err := catalog()
	if err != nil {
		return err
	}
	op, ok := find(operations, strings.ToUpper(method), path)
	if !ok {
		return usageError{fmt.Sprintf("no operation %s %s; `msime-cloud routes` lists them", strings.ToUpper(method), path)}
	}
	if op.Auth == authAdmin {
		op.Guide = adminGuide(op.Path)
	}
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	encoder.SetEscapeHTML(false)
	return encoder.Encode(op)
}

// adminSummary describes an admin route from its table entry; the request bodies are in the guide describe prints.
func adminSummary(route account.AdminRoute) string {
	parts := []string{"admin API"}
	if route.Method == "*" {
		parts = append(parts, "methods per docs/admin.md")
	}
	if len(route.Filters) > 0 {
		filters := append([]string{"page", "q"}, route.Filters...)
		parts = append(parts, "filters: "+strings.Join(filters, ", "))
	}
	return strings.Join(parts, "; ")
}

// adminGuide 按首个参数之前的路径收集文档中提到该接口的行，供 describe 展示路由表未包含的请求体和权限。
func adminGuide(path string) []string {
	base, _, _ := strings.Cut(path, "{")
	base = strings.TrimSuffix(base, "/")
	var lines []string
	for line := range strings.SplitSeq(docs.Admin, "\n") {
		if mentions(line, base) {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) == 0 {
		lines = []string{"docs/admin.md does not name " + base + "; see the admin-web client in admin-web/src/api"}
	}
	return lines
}

// mentions reports whether line names path itself rather than a longer path that starts with it: /api/users is in "GET /api/users?page=" and "/api/users/{id}" but not in "/api/users-archive".
func mentions(line, path string) bool {
	for rest := line; ; {
		i := strings.Index(rest, path)
		if i < 0 {
			return false
		}
		rest = rest[i+len(path):]
		if rest == "" || !strings.ContainsAny(rest[:1], "abcdefghijklmnopqrstuvwxyz-_") {
			return true
		}
	}
}
