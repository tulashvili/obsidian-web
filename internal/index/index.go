// Package index строит граф wiki-ссылок ([[...]]) в памяти: прямые ссылки и
// обратные (backlinks). Граф держится только в RAM и пересобирается при
// изменениях.
package index

import (
	"path"
	"regexp"
	"sort"
	"strings"
	"sync"

	"obsidiansecure/internal/store"
)

// wikiLinkRe ловит [[Target]], [[Target|alias]], [[Target#heading]] и embed
// ![[Target]]. Группа 1 = "!" для встраивания, группа 2 = сырое содержимое.
var wikiLinkRe = regexp.MustCompile(`(!?)\[\[([^\]\n]+)\]\]`)

// Link — одна разрешённая wiki-ссылка.
type Link struct {
	Raw     string // как записано: "Note#heading|alias"
	Target  string // нормализованное имя цели до # и |
	Heading string // часть после #
	Alias   string // часть после |
	Embed   bool   // ![[...]]
	// Resolved — путь файла, на который ссылка указывает; "" если не найден.
	Resolved string
}

// Backlink — ссылка на текущую заметку из другой.
type Backlink struct {
	From string `json:"from"` // путь файла-источника
	// Context — короткий фрагмент текста вокруг ссылки.
	Context string `json:"context"`
}

type Index struct {
	st *store.Store

	mu sync.RWMutex
	// forward: path -> исходящие резолвнутые пути
	forward map[string][]string
	// back: path -> входящие ссылки
	back map[string][]Backlink
	// byBasename: lower(basename без .md) -> []paths (для резолва [[Name]])
	byBasename map[string][]string
	// allPaths: set всех путей в нижнем регистре -> реальный путь
	byLowerPath map[string]string
}

func New(st *store.Store) *Index {
	return &Index{st: st}
}

func isMarkdown(p string) bool {
	return strings.HasSuffix(strings.ToLower(p), ".md")
}

// Rebuild перестраивает весь граф по содержимому хранилища. Дешифрует все
// markdown-файлы в RAM (для 4000 заметок это доли секунды).
func (ix *Index) Rebuild() error {
	files := ix.st.List()

	byBasename := map[string][]string{}
	byLowerPath := map[string]string{}
	for _, f := range files {
		byLowerPath[strings.ToLower(f.Path)] = f.Path
		base := strings.ToLower(strings.TrimSuffix(path.Base(f.Path), path.Ext(f.Path)))
		byBasename[base] = append(byBasename[base], f.Path)
	}

	forward := map[string][]string{}
	back := map[string][]Backlink{}

	for _, f := range files {
		if !isMarkdown(f.Path) {
			continue
		}
		data, err := ix.st.Read(f.Path)
		if err != nil {
			return err
		}
		content := string(data)
		links := parseLinks(content)
		seen := map[string]bool{}
		for _, l := range links {
			resolved := resolve(l.Target, f.Path, byBasename, byLowerPath)
			if resolved == "" || seen[resolved] {
				continue
			}
			seen[resolved] = true
			forward[f.Path] = append(forward[f.Path], resolved)
			back[resolved] = append(back[resolved], Backlink{
				From:    f.Path,
				Context: snippet(content, l.Raw),
			})
		}
	}

	ix.mu.Lock()
	ix.forward = forward
	ix.back = back
	ix.byBasename = byBasename
	ix.byLowerPath = byLowerPath
	ix.mu.Unlock()
	return nil
}

// parseLinks извлекает все wiki-ссылки из текста.
func parseLinks(content string) []Link {
	matches := wikiLinkRe.FindAllStringSubmatch(content, -1)
	out := make([]Link, 0, len(matches))
	for _, m := range matches {
		embed := m[1] == "!"
		raw := m[2]
		l := Link{Raw: raw, Embed: embed}
		rest := raw
		if i := strings.Index(rest, "|"); i >= 0 {
			l.Alias = strings.TrimSpace(rest[i+1:])
			rest = rest[:i]
		}
		if i := strings.Index(rest, "#"); i >= 0 {
			l.Heading = strings.TrimSpace(rest[i+1:])
			rest = rest[:i]
		}
		l.Target = strings.TrimSpace(rest)
		out = append(out, l)
	}
	return out
}

// resolve превращает имя цели в реальный путь файла по правилам Obsidian:
// сначала пробуем как путь, потом как basename. Без расширения подразумевается .md.
func resolve(target, from string, byBasename map[string][]string, byLowerPath map[string]string) string {
	if target == "" {
		return ""
	}
	t := strings.ToLower(strings.TrimSpace(target))

	// Если выглядит как путь (есть слэш) — пробуем напрямую.
	if strings.Contains(t, "/") {
		if p, ok := byLowerPath[t]; ok {
			return p
		}
		if p, ok := byLowerPath[t+".md"]; ok {
			return p
		}
		return ""
	}

	// Иначе ищем по basename.
	candidates := byBasename[t]
	if len(candidates) == 0 {
		// Возможно цель указана с расширением.
		base := strings.TrimSuffix(t, path.Ext(t))
		candidates = byBasename[base]
	}
	switch len(candidates) {
	case 0:
		return ""
	case 1:
		return candidates[0]
	default:
		// Неоднозначность: предпочитаем .md и ближайший по каталогу к from.
		return pickClosest(candidates, from)
	}
}

func pickClosest(candidates []string, from string) string {
	fromDir := path.Dir(from)
	best := candidates[0]
	bestScore := -1
	for _, c := range candidates {
		score := commonPrefixLen(path.Dir(c), fromDir)
		if isMarkdown(c) {
			score += 1000 // markdown в приоритете
		}
		if score > bestScore {
			bestScore = score
			best = c
		}
	}
	return best
}

func commonPrefixLen(a, b string) int {
	as, bs := strings.Split(a, "/"), strings.Split(b, "/")
	n := 0
	for n < len(as) && n < len(bs) && as[n] == bs[n] {
		n++
	}
	return n
}

// snippet возвращает короткий контекст вокруг ссылки для отображения в панели
// обратных ссылок.
func snippet(content, raw string) string {
	full := "[[" + raw + "]]"
	i := strings.Index(content, full)
	if i < 0 {
		// embed-вариант
		i = strings.Index(content, "![["+raw+"]]")
		if i < 0 {
			return ""
		}
	}
	start := i - 60
	if start < 0 {
		start = 0
	}
	end := i + len(full) + 60
	if end > len(content) {
		end = len(content)
	}
	s := content[start:end]
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.TrimSpace(s)
}

// Backlinks возвращает входящие ссылки на путь.
func (ix *Index) Backlinks(p string) []Backlink {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	bl := ix.back[p]
	out := make([]Backlink, len(bl))
	copy(out, bl)
	sort.Slice(out, func(i, j int) bool { return out[i].From < out[j].From })
	return out
}

// Outgoing возвращает исходящие (разрешённые) ссылки из пути.
func (ix *Index) Outgoing(p string) []string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	out := make([]string, len(ix.forward[p]))
	copy(out, ix.forward[p])
	return out
}

// Resolve публичный резолвер для рендерера: имя цели -> путь файла ("" если нет).
func (ix *Index) Resolve(target, from string) string {
	ix.mu.RLock()
	defer ix.mu.RUnlock()
	return resolve(target, from, ix.byBasename, ix.byLowerPath)
}
