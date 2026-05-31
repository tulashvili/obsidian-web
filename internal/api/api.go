// Package api — HTTP-слой. Сервер стартует «запертым»: до ввода мастер-пароля
// ключа нет, хранилище не открыто, контент недоступен. После /api/unlock
// ключ держится в RAM, открывается store, строится индекс.
package api

import (
	"context"
	"encoding/json"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"obsidiansecure/internal/config"
	"obsidiansecure/internal/crypto"
	"obsidiansecure/internal/gitsync"
	"obsidiansecure/internal/index"
	"obsidiansecure/internal/render"
	"obsidiansecure/internal/store"
)

type App struct {
	cfg config.Config

	mu       sync.RWMutex
	unlocked bool
	cipher   *crypto.Cipher
	store    *store.Store
	index    *index.Index
	render   *render.Renderer
	syncer   *gitsync.Syncer
}

func NewApp(cfg config.Config) *App { return &App{cfg: cfg} }

// Handler возвращает корневой http.Handler с зарегистрированными маршрутами.
func (a *App) Handler(static http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/status", a.handleStatus)
	mux.HandleFunc("/api/unlock", a.handleUnlock)
	mux.HandleFunc("/api/tree", a.guard(a.handleTree))
	mux.HandleFunc("/api/file", a.guard(a.handleFile))
	mux.HandleFunc("/api/raw", a.guard(a.handleRaw))
	mux.HandleFunc("/api/save", a.guard(a.handleSave))
	mux.HandleFunc("/api/sync", a.guard(a.handleSync))
	mux.HandleFunc("/api/search", a.guard(a.handleSearch))
	// Всё остальное — статика SPA (она сама показывает экран разблокировки).
	mux.Handle("/", static)
	return mux
}

// guard пропускает запрос только в разблокированном состоянии.
func (a *App) guard(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a.mu.RLock()
		ok := a.unlocked
		a.mu.RUnlock()
		if !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "locked"})
			return
		}
		h(w, r)
	}
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	resp := map[string]any{
		"unlocked":        a.unlocked,
		"repoConfigured":  a.cfg.RepoURL != "",
	}
	if a.unlocked && a.store != nil {
		resp["fileCount"] = len(a.store.List())
	}
	writeJSON(w, http.StatusOK, resp)
}

type unlockReq struct {
	Password string `json:"password"`
}

func (a *App) handleUnlock(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.unlocked {
		writeJSON(w, http.StatusOK, map[string]string{"status": "already-unlocked"})
		return
	}

	var req unlockReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "пароль обязателен"})
		return
	}
	pw := []byte(req.Password)
	defer crypto.Wipe(pw)

	metaPath := path.Join(a.cfg.DataDir, "vault-meta.json")
	kdf, verifier, exists, err := crypto.LoadVaultMeta(metaPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}

	if !exists {
		// Первый запуск: генерируем параметры и сохраняем отпечаток.
		kdf = crypto.DefaultKDFParams()
		c, err := crypto.NewCipher(pw, kdf)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := crypto.SaveVaultMeta(metaPath, kdf, c.Verifier()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		if err := a.openLocked(c); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "initialized", "firstRun": true})
		return
	}

	// Повторный вход: выводим ключ и проверяем отпечаток.
	c, err := crypto.NewCipher(pw, kdf)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if !c.CheckVerifier(verifier) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "неверный пароль"})
		return
	}
	if err := a.openLocked(c); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "unlocked"})
}

// openLocked открывает хранилище и строит индекс. Вызывается под a.mu.
func (a *App) openLocked(c *crypto.Cipher) error {
	st, err := store.Open(a.cfg.DataDir, c)
	if err != nil {
		return err
	}
	ix := index.New(st)
	if err := ix.Rebuild(); err != nil {
		return err
	}
	a.cipher = c
	a.store = st
	a.index = ix
	a.render = render.New(ix)
	a.syncer = gitsync.New(a.cfg, st)
	a.unlocked = true
	return nil
}

// --- Контентные эндпоинты ---

// treeNode — узел дерева файлов.
type treeNode struct {
	Name     string      `json:"name"`
	Path     string      `json:"path"`
	IsDir    bool        `json:"isDir"`
	Children []*treeNode `json:"children,omitempty"`
}

func (a *App) handleTree(w http.ResponseWriter, r *http.Request) {
	files := a.store.List()
	root := &treeNode{Name: "", Path: "", IsDir: true}
	dirs := map[string]*treeNode{"": root}

	ensureDir := func(p string) *treeNode {
		if n, ok := dirs[p]; ok {
			return n
		}
		parent := ensureDirChain(dirs, path.Dir(p))
		n := &treeNode{Name: path.Base(p), Path: p, IsDir: true}
		parent.Children = append(parent.Children, n)
		dirs[p] = n
		return n
	}

	for _, f := range files {
		dir := path.Dir(f.Path)
		if dir == "." {
			dir = ""
		}
		parent := root
		if dir != "" {
			parent = ensureDir(dir)
		}
		parent.Children = append(parent.Children, &treeNode{
			Name:  path.Base(f.Path),
			Path:  f.Path,
			IsDir: false,
		})
	}
	sortTree(root)
	writeJSON(w, http.StatusOK, root)
}

func ensureDirChain(dirs map[string]*treeNode, p string) *treeNode {
	if p == "." || p == "/" {
		p = ""
	}
	if n, ok := dirs[p]; ok {
		return n
	}
	parent := ensureDirChain(dirs, path.Dir(p))
	n := &treeNode{Name: path.Base(p), Path: p, IsDir: true}
	parent.Children = append(parent.Children, n)
	dirs[p] = n
	return n
}

func sortTree(n *treeNode) {
	// Каталоги выше файлов, затем по алфавиту.
	c := n.Children
	for i := 0; i < len(c); i++ {
		for j := i + 1; j < len(c); j++ {
			less := false
			if c[i].IsDir != c[j].IsDir {
				less = c[j].IsDir
			} else {
				less = c[j].Name < c[i].Name
			}
			if less {
				c[i], c[j] = c[j], c[i]
			}
		}
	}
	for _, ch := range c {
		if ch.IsDir {
			sortTree(ch)
		}
	}
}

func (a *App) handleFile(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !a.store.Has(p) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "файл не найден"})
		return
	}
	data, err := a.store.Read(p)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	resp := map[string]any{"path": p}
	if strings.HasSuffix(strings.ToLower(p), ".md") {
		html, err := a.render.Render(string(data), p)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		resp["type"] = "markdown"
		resp["html"] = html
		resp["raw"] = string(data)
		resp["backlinks"] = a.index.Backlinks(p)
		resp["outgoing"] = a.index.Outgoing(p)
	} else {
		resp["type"] = "asset"
		resp["rawUrl"] = "/api/raw?path=" + p
	}
	writeJSON(w, http.StatusOK, resp)
}

func (a *App) handleRaw(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Query().Get("path")
	if !a.store.Has(p) {
		http.NotFound(w, r)
		return
	}
	data, err := a.store.Read(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType(p))
	w.Header().Set("Cache-Control", "no-store") // расшифрованное не кэшируем
	w.Write(data)
}

type saveReq struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (a *App) handleSave(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	var req saveReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Path == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path и content обязательны"})
		return
	}
	if err := a.store.Write(req.Path, []byte(req.Content)); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	// Пересобираем граф ссылок (для 4000 файлов это быстро; можно оптимизировать
	// до инкрементального обновления позже).
	if err := a.index.Rebuild(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "saved"})
}

func (a *App) handleSync(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method", http.StatusMethodNotAllowed)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()
	res, err := a.syncer.Sync(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if err := a.index.Rebuild(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (a *App) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if q == "" {
		writeJSON(w, http.StatusOK, []any{})
		return
	}
	type hit struct {
		Path    string `json:"path"`
		Snippet string `json:"snippet"`
	}
	var hits []hit
	for _, f := range a.store.List() {
		nameMatch := strings.Contains(strings.ToLower(f.Path), q)
		var snip string
		if strings.HasSuffix(strings.ToLower(f.Path), ".md") {
			data, err := a.store.Read(f.Path)
			if err == nil {
				lc := strings.ToLower(string(data))
				if idx := strings.Index(lc, q); idx >= 0 {
					start := idx - 40
					if start < 0 {
						start = 0
					}
					end := idx + 40
					if end > len(data) {
						end = len(data)
					}
					snip = strings.ReplaceAll(string(data[start:end]), "\n", " ")
					nameMatch = true
				}
			}
		}
		if nameMatch {
			hits = append(hits, hit{Path: f.Path, Snippet: snip})
			if len(hits) >= 100 {
				break
			}
		}
	}
	writeJSON(w, http.StatusOK, hits)
}

func contentType(p string) string {
	switch strings.ToLower(path.Ext(p)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".svg":
		return "image/svg+xml"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".mp4":
		return "video/mp4"
	case ".mp3":
		return "audio/mpeg"
	case ".md", ".txt":
		return "text/plain; charset=utf-8"
	default:
		return "application/octet-stream"
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
