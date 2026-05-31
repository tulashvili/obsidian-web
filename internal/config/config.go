// Package config собирает конфигурацию из переменных окружения.
package config

import (
	"os"
	"strings"
)

type Config struct {
	// DataDir — постоянный том, где лежат ТОЛЬКО зашифрованные данные.
	DataDir string
	// WorkDir — рабочая директория в tmpfs (RAM). Plaintext живёт только тут.
	WorkDir string
	// RepoURL — источник истины (git). Может быть пустым при первом запуске
	// до инициализации.
	RepoURL string
	// RepoBranch — ветка по умолчанию.
	RepoBranch string
	// GitToken / GitSSHKeyPath — учётные данные для приватного репозитория.
	GitToken     string
	GitUsername  string
	GitSSHKeyPath string
	// Addr — адрес HTTP-сервера.
	Addr string
	// AssetCacheBytes — лимит RAM-кэша расшифрованных assets.
	AssetCacheBytes int64
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func Load() Config {
	return Config{
		DataDir:       env("OSA_DATA_DIR", "/data"),
		WorkDir:       env("OSA_WORK_DIR", "/work"),
		RepoURL:       env("OSA_REPO_URL", ""),
		RepoBranch:    env("OSA_REPO_BRANCH", "main"),
		GitToken:      env("OSA_GIT_TOKEN", ""),
		GitUsername:   env("OSA_GIT_USERNAME", "git"),
		GitSSHKeyPath: env("OSA_GIT_SSH_KEY", ""),
		Addr:          env("OSA_ADDR", ":8080"),
		AssetCacheBytes: parseSize(env("OSA_ASSET_CACHE", "268435456")), // 256 МиБ
	}
}

func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	var n int64
	for _, c := range s {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int64(c-'0')
	}
	if n == 0 {
		return 268435456
	}
	return n
}
