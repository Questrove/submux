package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"submux/internal/server"
	"submux/internal/source"
	"submux/internal/store"
)

func main() {
	dbPath := getenv("SUBMUX_DB", "submux.db")

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("open store: %v", err)
	}
	defer st.Close()

	interval, _ := st.GetSettingInt("fetch_interval_sec", 10800)
	listenAddr, _ := st.GetSetting("listen_addr")
	if listenAddr == "" {
		listenAddr = "127.0.0.1:8080"
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 后台定时拉取上游订阅
	f := source.NewFetcher(st)
	app, err := server.NewChecked(st, f)
	if err != nil {
		log.Fatalf("initialize server: %v", err)
	}
	go f.Loop(ctx, time.Duration(interval)*time.Second)

	// HTTP 服务(/sub/{token} 输出已发布的固定引擎订阅产物)
	httpSrv := &http.Server{Addr: listenAddr, Handler: app.Handler()}
	go func() {
		log.Printf("submux listening on %s", listenAddr)
		if err := httpSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("http server error: %v", err)
		}
	}()

	log.Printf("submux started: db=%s fetch_interval=%ds", dbPath, interval)
	<-ctx.Done()

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = httpSrv.Shutdown(shutdownCtx)
	log.Println("shutdown complete")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
