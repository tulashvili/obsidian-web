// Package render преобразует markdown в HTML с поддержкой wiki-ссылок.
//
// Wiki-ссылки разрешаются ДО goldmark: [[Target|alias]] превращается в обычную
// markdown-ссылку на внутренний маршрут, а embed ![[image.png]] — в <img>.
// Внутри fenced-блоков (```), и inline-кода (`...`) подстановка не делается.
package render

import (
	"bytes"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/renderer/html"

	"obsidiansecure/internal/index"
)

type Renderer struct {
	md goldmark.Markdown
	ix *index.Index
}

func New(ix *index.Index) *Renderer {
	md := goldmark.New(
		goldmark.WithExtensions(extension.GFM, extension.Footnote),
		goldmark.WithRendererOptions(html.WithUnsafe()), // контент доверенный (свой репозиторий)
	)
	return &Renderer{md: md, ix: ix}
}

var (
	wikiRe  = regexp.MustCompile(`(!?)\[\[([^\]\n]+)\]\]`)
	fenceRe = regexp.MustCompile("(?s)```.*?```|`[^`\n]*`")
)

// Render возвращает HTML для markdown-файла по пути from.
func (r *Renderer) Render(content, from string) (string, error) {
	pre := r.expandWikiLinks(content, from)
	var buf bytes.Buffer
	if err := r.md.Convert([]byte(pre), &buf); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// expandWikiLinks заменяет wiki-ссылки вне код-блоков.
func (r *Renderer) expandWikiLinks(content, from string) string {
	// Защищаем код-блоки: вырезаем, заменяем плейсхолдерами, потом возвращаем.
	var codes []string
	masked := fenceRe.ReplaceAllStringFunc(content, func(m string) string {
		codes = append(codes, m)
		return "\x00CODE" + itoa(len(codes)-1) + "\x00"
	})

	masked = wikiRe.ReplaceAllStringFunc(masked, func(m string) string {
		sub := wikiRe.FindStringSubmatch(m)
		embed := sub[1] == "!"
		raw := sub[2]

		target, heading, alias := splitLink(raw)
		resolved := r.ix.Resolve(target, from)

		if embed {
			// Встраивание ассета (картинки и т.п.).
			if resolved == "" {
				return m // оставляем как есть, если не нашли
			}
			return "![" + alias + "](/api/raw?path=" + urlEscape(resolved) + ")"
		}

		label := alias
		if label == "" {
			label = target
			if heading != "" {
				label += " › " + heading
			}
		}
		if resolved == "" {
			// Несуществующая ссылка: href со схемой missing: — фронт стилизует
			// через селектор a[href^="missing:"].
			return "[" + label + "](missing:" + urlEscape(target) + ")"
		}
		// Существующие wiki-ссылки ведут на /view?path=... — фронт перехватывает
		// клик по таким ссылкам (a[href^="/view"]) для SPA-навигации.
		href := "/view?path=" + urlEscape(resolved)
		if heading != "" {
			href += "#" + urlEscape(slugify(heading))
		}
		return "[" + label + "](" + href + ")"
	})

	// Возвращаем код-блоки.
	for i, c := range codes {
		masked = strings.Replace(masked, "\x00CODE"+itoa(i)+"\x00", c, 1)
	}
	return masked
}

func splitLink(raw string) (target, heading, alias string) {
	rest := raw
	if i := strings.Index(rest, "|"); i >= 0 {
		alias = strings.TrimSpace(rest[i+1:])
		rest = rest[:i]
	}
	if i := strings.Index(rest, "#"); i >= 0 {
		heading = strings.TrimSpace(rest[i+1:])
		rest = rest[:i]
	}
	target = strings.TrimSpace(rest)
	return
}

func slugify(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, " ", "-")
	return s
}

func urlEscape(s string) string {
	// Минимальное экранирование для query-параметра path.
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c == ' ':
			b.WriteString("%20")
		case c == '#':
			b.WriteString("%23")
		case c == '?':
			b.WriteString("%3F")
		case c == '&':
			b.WriteString("%26")
		default:
			b.WriteByte(c)
		}
	}
	return b.String()
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
