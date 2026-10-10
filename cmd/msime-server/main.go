package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/metasequoiaime/MSIME-Backend/internal/account"
	"github.com/metasequoiaime/MSIME-Backend/internal/server"
)

func main() {
	path := flag.String("config", "config.json", "path to configuration")
	migrate := flag.Bool("migrate-users", false, "迁移用户数据库后退出")
	pluginSeed := flag.String("render-plugin-seed", "", "把目录（通常是 assets/community-starter-plugins）里的精选插件包渲染成种子 SQL 写到 stdout 后退出；不读配置、不连接数据库")
	pluginSeedRole := flag.String("plugin-seed-role", "msime_backend", "种子 SQL 切换到的 DML 运行角色")
	flag.Parse()
	if *pluginSeed != "" {
		sql, err := account.RenderCommunityPluginSeed(*pluginSeed, *pluginSeedRole)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Print(sql)
		return
	}
	config, err := server.LoadConfig(*path)
	if err != nil {
		slog.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	if *migrate {
		if !config.Auth.Enabled {
			slog.Error("用户体系未启用")
			os.Exit(1)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db, err := account.Open(ctx, os.Getenv(config.Auth.DatabaseEnv))
		if err != nil {
			slog.Error("用户数据库连接失败", "error", err)
			os.Exit(1)
		}
		defer db.Close()
		// 和启动时自动迁移走同一条路,否则手动跑一次和自动跑一次会有不同的权限行为。
		if err = db.MigrateAs(ctx, config.Auth.MigrationRole); err != nil {
			slog.Error("用户数据库迁移失败")
			os.Exit(1)
		}
		slog.Info("用户数据库迁移完成")
		return
	}
	handler, err := server.New(config)
	if err != nil {
		slog.Error("configuration rejected", "error", err)
		os.Exit(1)
	}
	defer handler.CloseAccounts()
	srv := &http.Server{Addr: config.Listen, Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: time.Duration(config.TimeoutSeconds+5) * time.Second, WriteTimeout: time.Duration(config.TimeoutSeconds+5) * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	// 管理后台的日志流是长连接，Shutdown 开始时先结束它们，否则会拖满整个优雅关闭时限。
	srv.RegisterOnShutdown(handler.EndLogStreams)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	done := make(chan error, 1)
	go func() { slog.Info("MSIME service listening", "address", config.Listen); done <- srv.ListenAndServe() }()
	select {
	case err := <-done:
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("HTTP server stopped", "error", err)
			os.Exit(1)
		}
	case <-ctx.Done():
		// Drain HTTP requests first, then Close: the final metrics flush must run after the last request has recorded its upstream call. Shutdown leaves hijacked WebSocket streams alone; Close cancels and waits for them and the background jobs, then flushes.
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdown); err != nil {
			_ = srv.Close()
		}
		handler.Close()
	}
}
