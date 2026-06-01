// CodeMirror 6 live-preview редактор для obsidian-secure.
//
// Single-pane WYSIWYG-подобный режим (как Live Preview в Obsidian):
//  - заголовки/жирный/курсив/зачёркивание/инлайн-код рендерятся «по месту»,
//    а служебные символы (#, **, ~~, `) скрываются, ПОКА курсор не на них;
//  - [[wiki-ссылки]] показываются как кликабельные пилюли, ![[...]] — как
//    встроенные изображения; при заходе курсором внутрь снова виден исходник;
//  - автодополнение [[ по списку заметок.
//
// Бандлится esbuild'ом в один файл cmd/server/web/cm.js и вшивается в бинарник.

import { EditorState } from "@codemirror/state";
import {
  EditorView,
  keymap,
  Decoration,
  ViewPlugin,
  WidgetType,
  drawSelection,
  highlightActiveLine,
} from "@codemirror/view";
import {
  defaultKeymap,
  history,
  historyKeymap,
  indentWithTab,
} from "@codemirror/commands";
import { markdown, markdownLanguage } from "@codemirror/lang-markdown";
import {
  syntaxTree,
  syntaxHighlighting,
  defaultHighlightStyle,
  HighlightStyle,
} from "@codemirror/language";
import { tags as t } from "@lezer/highlight";
import { autocompletion } from "@codemirror/autocomplete";

// --- Виджет встроенного [[wiki-ссылки]] / ![[embed]] ---
class WikiWidget extends WidgetType {
  constructor(opts) {
    super();
    this.opts = opts; // {label, target, resolved, embed, onLinkClick}
  }
  eq(o) {
    return (
      o.opts.label === this.opts.label &&
      o.opts.resolved === this.opts.resolved &&
      o.opts.embed === this.opts.embed
    );
  }
  ignoreEvent() {
    return false;
  }
  toDOM() {
    const o = this.opts;
    if (o.embed && o.resolved && isImage(o.resolved)) {
      const img = document.createElement("img");
      img.className = "cm-embed-img";
      img.src = "/api/raw?path=" + encodeURIComponent(o.resolved);
      img.alt = o.label;
      return img;
    }
    const span = document.createElement("span");
    span.className = "cm-wikilink" + (o.resolved ? "" : " cm-wikilink-missing");
    span.textContent = o.label;
    if (o.resolved) {
      span.title = o.resolved;
      span.addEventListener("mousedown", (e) => {
        e.preventDefault();
        e.stopPropagation();
        if (o.onLinkClick) o.onLinkClick(o.resolved);
      });
    }
    return span;
  }
}

function isImage(p) {
  return /\.(png|jpe?g|gif|svg|webp|bmp)$/i.test(p);
}

// Разбор содержимого [[Target#heading|alias]] -> {target, heading, alias}.
function parseWiki(raw) {
  let rest = raw;
  let alias = "";
  let heading = "";
  const pipe = rest.indexOf("|");
  if (pipe >= 0) {
    alias = rest.slice(pipe + 1).trim();
    rest = rest.slice(0, pipe);
  }
  const hash = rest.indexOf("#");
  if (hash >= 0) {
    heading = rest.slice(hash + 1).trim();
    rest = rest.slice(0, hash);
  }
  return { target: rest.trim(), heading, alias };
}

// --- Плагин live-preview: строит декорации из синтакс-дерева + регэкспа ---
function livePreviewPlugin(config) {
  return ViewPlugin.fromClass(
    class {
      constructor(view) {
        this.decorations = this.build(view);
      }
      update(u) {
        if (u.docChanged || u.selectionSet || u.viewportChanged || u.focusChanged) {
          this.decorations = this.build(u.view);
        }
      }
      build(view) {
        // Собираем диапазоны декораций; порядок не важен — Decoration.set с
        // флагом sort=true сам разложит их по from/startSide (это надёжнее
        // ручной сортировки и не падает на смешении строчных и инлайновых).
        const ranges = [];
        const state = view.state;
        const sel = state.selection.main;
        // Курсор «на узле», если выделение пересекает его диапазон —
        // тогда показываем исходник, чтобы было удобно редактировать.
        const cursorTouches = (from, to) => sel.from <= to && sel.to >= from;

        const tree = syntaxTree(state);
        for (const { from, to } of view.visibleRanges) {
          tree.iterate({
            from,
            to,
            enter: (node) => {
              const name = node.name;
              const nodeFrom = node.from;
              const nodeTo = node.to;

              // Заголовки: увеличиваем строку, прячем "# ".
              const hm = name.match(/^ATXHeading(\d)$/);
              if (hm) {
                const lineStart = state.doc.lineAt(nodeFrom).from;
                ranges.push(Decoration.line({ class: "cm-h" + hm[1] }).range(lineStart));
                return;
              }

              if (name === "HeaderMark") {
                const parent = node.node.parent;
                if (parent && cursorTouches(parent.from, parent.to)) return;
                // Прячем "#"-ы и один пробел после них.
                let end = nodeTo;
                if (state.doc.sliceString(end, end + 1) === " ") end += 1;
                ranges.push(Decoration.replace({}).range(nodeFrom, end));
                return;
              }

              if (name === "StrongEmphasis") {
                ranges.push(Decoration.mark({ class: "cm-strong" }).range(nodeFrom, nodeTo));
                return;
              }
              if (name === "Emphasis") {
                ranges.push(Decoration.mark({ class: "cm-em" }).range(nodeFrom, nodeTo));
                return;
              }
              if (name === "Strikethrough") {
                ranges.push(Decoration.mark({ class: "cm-strike" }).range(nodeFrom, nodeTo));
                return;
              }
              if (name === "InlineCode") {
                ranges.push(Decoration.mark({ class: "cm-inline-code" }).range(nodeFrom, nodeTo));
                return;
              }

              // Скрываемые маркеры форматирования (если курсор не на родителе).
              if (
                name === "EmphasisMark" ||
                name === "StrikethroughMark" ||
                name === "CodeMark"
              ) {
                const parent = node.node.parent;
                if (parent && cursorTouches(parent.from, parent.to)) return;
                if (nodeTo > nodeFrom) ranges.push(Decoration.replace({}).range(nodeFrom, nodeTo));
                return;
              }
            },
          });

          // [[wiki-ссылки]] и ![[embed]] — регэкспом по видимому тексту.
          const text = state.doc.sliceString(from, to);
          const re = /(!?)\[\[([^\]\n]+)\]\]/g;
          let m;
          while ((m = re.exec(text)) !== null) {
            const start = from + m.index;
            const end = start + m[0].length;
            if (cursorTouches(start, end)) continue; // редактируем — показываем исходник
            const embed = m[1] === "!";
            const { target, heading, alias } = parseWiki(m[2]);
            const resolved = config.resolve ? config.resolve(target) : "";
            let label = alias || target;
            if (!alias && heading) label += " › " + heading;
            ranges.push(
              Decoration.replace({
                widget: new WikiWidget({
                  label,
                  target,
                  resolved,
                  embed,
                  onLinkClick: config.onLinkClick,
                }),
              }).range(start, end)
            );
          }
        }

        return Decoration.set(ranges, true);
      }
    },
    {
      decorations: (v) => v.decorations,
    }
  );
}

// --- Автодополнение [[ ---
function wikiAutocomplete(getFiles) {
  return (ctx) => {
    const before = ctx.matchBefore(/\[\[[^\]\n]*$/);
    if (!before) return null;
    if (before.from === before.to) return null;
    const q = before.text.slice(2).toLowerCase();
    const files = getFiles ? getFiles() : [];
    let opts = files.filter(
      (f) => f.name.toLowerCase().includes(q) || f.path.toLowerCase().includes(q)
    );
    opts.sort((a, b) => {
      const aw = a.name.toLowerCase().startsWith(q) ? 0 : 1;
      const bw = b.name.toLowerCase().startsWith(q) ? 0 : 1;
      if (aw !== bw) return aw - bw;
      return a.name.localeCompare(b.name, "ru");
    });
    const options = opts.slice(0, 50).map((f) => ({
      label: f.name,
      detail: f.path,
      type: "text",
      apply: (view, completion, fromPos, toPos) => {
        const insert = f.name + "]]";
        // Не дублируем уже стоящие закрывающие скобки.
        const after = view.state.doc.sliceString(toPos, toPos + 2);
        const tail = after === "]]" ? 2 : 0;
        view.dispatch({
          changes: { from: fromPos, to: toPos + tail, insert },
          selection: { anchor: fromPos + insert.length },
        });
      },
    }));
    return { from: before.from + 2, options, filter: false };
  };
}

// --- Тема оформления (тёмная, под палитру приложения) ---
const osaTheme = EditorView.theme(
  {
    "&": {
      color: "var(--text)",
      backgroundColor: "var(--bg)",
      height: "100%",
      fontSize: "15px",
    },
    ".cm-scroller": {
      fontFamily: "-apple-system, system-ui, 'Segoe UI', Roboto, sans-serif",
      lineHeight: "1.7",
      padding: "16px 32px",
      overflow: "auto",
    },
    ".cm-content": { caretColor: "var(--accent)", maxWidth: "860px" },
    "&.cm-focused": { outline: "none" },
    ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--accent)" },
    "&.cm-focused .cm-selectionBackground, .cm-selectionBackground, ::selection": {
      backgroundColor: "#3a3a52",
    },
    ".cm-activeLine": { backgroundColor: "rgba(255,255,255,0.025)" },
    ".cm-tooltip.cm-tooltip-autocomplete": {
      border: "1px solid var(--border)",
      borderRadius: "8px",
      background: "var(--bg-alt)",
      boxShadow: "0 8px 24px rgba(0,0,0,.45)",
    },
    ".cm-tooltip-autocomplete ul li[aria-selected]": {
      background: "var(--accent)",
      color: "#fff",
    },
    ".cm-completionDetail": { color: "var(--text-dim)", fontStyle: "normal", marginLeft: "8px" },
  },
  { dark: true }
);

// Подсветка синтаксиса (ссылки, цитаты, списки и т.п., что не покрыто live-preview).
const osaHighlight = HighlightStyle.define([
  { tag: t.link, color: "var(--accent)", textDecoration: "underline" },
  { tag: t.url, color: "var(--text-dim)" },
  { tag: t.quote, color: "var(--text-dim)", fontStyle: "italic" },
  { tag: t.list, color: "var(--accent)" },
  { tag: [t.meta, t.processingInstruction], color: "var(--text-dim)" },
  { tag: t.monospace, color: "#c8b3ff" },
]);

// mount создаёт редактор внутри parent. Возвращает API для app.js.
function mount(parent, cfg) {
  cfg = cfg || {};
  const view = new EditorView({
    parent,
    state: EditorState.create({
      doc: cfg.doc || "",
      extensions: [
        history(),
        drawSelection(),
        highlightActiveLine(),
        EditorState.allowMultipleSelections.of(true),
        markdown({ base: markdownLanguage, addKeymap: true }),
        syntaxHighlighting(osaHighlight),
        syntaxHighlighting(defaultHighlightStyle, { fallback: true }),
        livePreviewPlugin({
          resolve: cfg.resolve,
          onLinkClick: cfg.onLinkClick,
        }),
        autocompletion({ override: [wikiAutocomplete(cfg.getFiles)], icons: false }),
        EditorView.lineWrapping,
        keymap.of([indentWithTab, ...defaultKeymap, ...historyKeymap]),
        osaTheme,
      ],
    }),
  });
  return {
    view,
    getValue: () => view.state.doc.toString(),
    focus: () => view.focus(),
    destroy: () => view.destroy(),
  };
}

export { mount };
