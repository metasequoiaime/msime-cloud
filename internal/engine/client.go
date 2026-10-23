// Package engine runs the pinned native Engine with bounded, isolated requests.
package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

type Config struct {
	Binary    string `json:"binary"`
	Resources string `json:"resources"`
}

func (c Config) Validate() error {
	if c.Binary != "" && !filepath.IsAbs(c.Binary) {
		return errors.New("engine binary must be an absolute path")
	}
	if c.Resources != "" && !filepath.IsAbs(c.Resources) {
		return errors.New("engine resources must be an absolute path")
	}
	return nil
}

var ErrUnavailable = errors.New("engine_unavailable")
var ErrFailure = errors.New("engine_failure")
var ErrInvalid = errors.New("invalid_engine_request")

// 丢弃超限输出，同时让子进程正常结束或到达超时期限。
// 使用命名字段，避免提升 Buffer 的方法后绕过 Write 中的限额。
type boundedOutput struct {
	buffer   bytes.Buffer
	exceeded bool
}

func (b *boundedOutput) Len() int { return b.buffer.Len() }

func (b *boundedOutput) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := (1 << 20) - b.Len()
	if len(p) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.buffer.Write(p)
	return n, nil
}

var nativeSlots = make(chan struct{}, 4)

func (c Config) Query(ctx context.Context, request any) (json.RawMessage, error) {
	return c.QuerySnapshot(ctx, request, nil)
}

// QuerySnapshot streams an authenticated database snapshot to a private, temporary input file.
// HTTP callers cannot choose this filename or the native resource paths.
func (c Config) QuerySnapshot(ctx context.Context, request any, snapshot func(context.Context, io.Writer) error) (json.RawMessage, error) {
	if c.Binary == "" {
		return nil, ErrUnavailable
	}
	raw, err := json.Marshal(request)
	if err != nil || len(raw) > 65536 {
		return nil, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	select {
	case nativeSlots <- struct{}{}:
		defer func() { <-nativeSlots }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	scratch, err := os.MkdirTemp("", "msime-query-")
	if err != nil {
		return nil, ErrFailure
	}
	defer os.RemoveAll(scratch)
	if snapshot != nil {
		file, err := os.OpenFile(filepath.Join(scratch, "snapshot.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, ErrFailure
		}
		err = snapshot(ctx, file)
		closeErr := file.Close()
		if err != nil {
			return nil, err
		}
		if closeErr != nil {
			return nil, ErrFailure
		}
	}
	command := exec.CommandContext(ctx, c.Binary, c.Resources, scratch)
	command.Stdin = bytes.NewReader(raw)
	command.Stderr = io.Discard
	command.WaitDelay = time.Second
	var output boundedOutput
	command.Stdout = &output
	if err = command.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrFailure
	}
	if output.exceeded || !json.Valid(output.Bytes()) {
		return nil, ErrFailure
	}
	var envelope struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(output.Bytes(), &envelope) != nil {
		return nil, ErrFailure
	}
	switch envelope.Error {
	case "":
		return json.RawMessage(output.Bytes()), nil
	case "resources_unavailable":
		return nil, ErrUnavailable
	case "invalid_request", "invalid_dictionary_entry":
		return nil, ErrInvalid
	default:
		return nil, ErrFailure
	}
}
