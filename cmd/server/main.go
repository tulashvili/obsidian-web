// Command server — точка входа obsidian-secure.
package main

import (
	"embed"
	"io/fs"
	"log"
	"net/http"
	"os"
	"time"

	"obsidiansecure/internal/api"
	"obsidiansecure/internal/config"
)

//go:embed web
var webFS embed.FS

func main() {
	cfg := config.Load()

	// Проверяем, что рабочий каталог (tmpfs) доступен на запись.
	if err := os.MkdirAll(cfg.WorkDir, 0o700); err != nil {
		log.Fatalf("не удалось создать рабочий каталог %s: %v", cfg.WorkDir, err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatalf("не удалось создать каталог данных %s: %v", cfg.DataDir, err)
	}

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatal(err)
	}
	static := http.FileServer(http.FS(sub))

	app := api.NewApp(cfg)
	srv := &http.Server{
		Addr:              announcedAddr(cfg.Addr),
		Handler:           app.Handler(static),
		ReadHeaderTimeout: 10 * time.Second,
	}

	log.Printf("obsidian-secure слушает на %s (data=%s, work=%s)", cfg.Addr, cfg.DataDir, cfg.WorkDir)
	log.Printf("откройте http://localhost%s и введите мастер-пароль", cfg.Addr)
	if err := srv.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func announcedAddr(a string) string {
	if a == "" {
		return ":8080"
	}
	return a
}
