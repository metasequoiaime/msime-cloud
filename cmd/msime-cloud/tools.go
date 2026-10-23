package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"

	msimebackend "github.com/metasequoiaime/MSIME-Backend"
	"github.com/metasequoiaime/MSIME-Backend/internal/publictools"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// publicTools 默认连接公共 API；--admin 显式选择独立的管理员工具目录。
func (c cli) publicTools(args []string) error {
	admin := len(args) > 1 && args[1] == "--admin"
	definitions, has, call, newServer := publictools.Definitions, publictools.Has, publictools.Call, publictools.NewServer
	if admin {
		args = append([]string{args[0]}, args[2:]...)
		definitions, has, call, newServer = publictools.AdminDefinitions, publictools.HasAdmin, publictools.AdminCall, publictools.NewAdminServer
	}
	switch args[0] {
	case "tools":
		if len(args) != 1 {
			return usageError{"tools takes no arguments"}
		}
		return json.NewEncoder(c.stdout).Encode(definitions())
	case "mcp":
		if len(args) != 1 {
			return usageError{"mcp takes no arguments"}
		}
	case "tool":
		if len(args) < 2 || len(args) > 3 {
			return usageError{"tool needs NAME and optionally JSON or -"}
		}
	}
	base := c.env("MSIME_CLOUD_URL")
	if base == "" {
		base = defaultServer
	}
	var execute publictools.Execute
	var err error
	if admin {
		execute, err = c.adminToolExecutor()
	} else {
		execute, err = publictools.HTTP(c.client, base, c.env("MSIME_CLOUD_TOKEN"))
	}
	if err != nil {
		return err
	}
	if args[0] == "mcp" {
		return newServer(execute, msimebackend.Version()).Run(context.Background(), &mcp.IOTransport{Reader: io.NopCloser(c.stdin), Writer: toolWriter{c.stdout}, MaxLineLength: publictools.MaxArguments})
	}
	arguments := json.RawMessage(`{}`)
	if len(args) == 3 {
		arguments = json.RawMessage(args[2])
		if args[2] == "-" {
			arguments, err = io.ReadAll(io.LimitReader(c.stdin, publictools.MaxArguments+1))
			if err != nil {
				return errors.New("cannot_read_tool_arguments")
			}
		}
	}
	name := strings.ReplaceAll(args[1], "-", "_")
	if !has(name) {
		return usageError{"unknown tool; run tools to list them"}
	}
	// Call 内部限制参数大小并严格校验，包括从 stdin 读入的内容。
	result, err := call(context.Background(), execute, name, arguments)
	if err != nil {
		return err
	}
	return json.NewEncoder(c.stdout).Encode(result)
}

type toolWriter struct{ io.Writer }

func (toolWriter) Close() error { return nil }

// adminToolExecutor 在每次调用时重新读取管理员会话，长驻 MCP 不保留过期凭据。
func (c cli) adminToolExecutor() (publictools.Execute, error) {
	base := c.adminServer()
	if _, err := publictools.AdminHTTP(c.client, base, ""); err != nil {
		return nil, err
	}
	return func(ctx context.Context, r publictools.Request) (json.RawMessage, error) {
		token := c.env("MSIME_ADMIN_TOKEN")
		if token == "" {
			kept, ok, err := c.store().get(adminKey(base))
			if err != nil {
				return nil, errors.New("admin_credentials_unavailable")
			}
			if !ok || kept.AccessToken == "" || !c.now().Before(kept.ExpiresAt) {
				return nil, errors.New("admin_login_required: run msime-cloud login admin or set MSIME_ADMIN_TOKEN")
			}
			token = kept.AccessToken
		}
		execute, err := publictools.AdminHTTP(c.client, base, token)
		if err != nil {
			return nil, err
		}
		return execute(ctx, r)
	}, nil
}
