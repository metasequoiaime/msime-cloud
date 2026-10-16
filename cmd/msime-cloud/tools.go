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

// publicTools 只连接显式选定的公共 API，不读取 CLI 保存的个人或管理员会话。
func (c cli) publicTools(args []string) error {
	switch args[0] {
	case "tools":
		if len(args) != 1 {
			return usageError{"tools takes no arguments"}
		}
		return json.NewEncoder(c.stdout).Encode(publictools.Definitions())
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
	execute, err := publictools.HTTP(c.client, base, c.env("MSIME_CLOUD_TOKEN"))
	if err != nil {
		return err
	}
	if args[0] == "mcp" {
		return publictools.NewServer(execute, msimebackend.Version()).Run(context.Background(), &mcp.IOTransport{Reader: io.NopCloser(c.stdin), Writer: toolWriter{c.stdout}, MaxLineLength: publictools.MaxArguments})
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
	if !publictools.Has(name) {
		return usageError{"unknown tool; run tools to list them"}
	}
	// Call 内部限制参数大小并严格校验，包括从 stdin 读入的内容。
	result, err := publictools.Call(context.Background(), execute, name, arguments)
	if err != nil {
		return err
	}
	return json.NewEncoder(c.stdout).Encode(result)
}

type toolWriter struct{ io.Writer }

func (toolWriter) Close() error { return nil }
