"use strict";

const $ = (sel) => document.querySelector(sel);
const api = {
  async get(url) {
    const r = await fetch(url);
    if (r.status === 401) { showLock(); throw new Error("locked"); }
    return r.json();
  },
  async post(url, body) {
    const r = await fetch(url, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body || {}),
    });
    return { ok: r.ok, status: r.status, data: await r.json().catch(() => ({})) };
  },
};

let state = { current: null, editing: false };

// --- Разблокировка ---
function showLock() {
  $("#lock").classList.remove("hidden");
  $("#app").classList.add("hidden");
}
function showApp() {
  $("#lock").classList.add("hidden");
  $("#app").classList.remove("hidden");
}

$("#unlock-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  $("#lock-error").textContent = "";
  const password = $("#password").value;
  const { ok, data } = await api.post("/api/unlock", { password });
  if (!ok) {
    $("#lock-error").textContent = data.error || "Ошибка";
    return;
  }
  $("#password").value = "";
  if (data.firstRun) {
    $("#lock-error").style.color = "var(--text-dim)";
  }
  showApp();
  await init();
});

// --- Инициализация после разблокировки ---
async function init() {
  await loadTree();
  const st = await api.get("/api/status");
  if (st.fileCount === 0 && st.repoConfigured) {
    setSyncStatus('Хранилище пусто — нажмите «Обновить» для клонирования');
  }
}

// --- Дерево файлов ---
async function loadTree() {
  const root = await api.get("/api/tree");
  const el = $("#tree");
  el.innerHTML = "";
  if (root.children) root.children.forEach((c) => el.appendChild(renderNode(c)));
}

function renderNode(node) {
  const wrap = document.createElement("div");
  wrap.className = "node " + (node.isDir ? "dir" : "file");
  const label = document.createElement("div");
  label.className = "label";
  label.textContent = node.name;
  label.dataset.path = node.path;
  wrap.appendChild(label);

  if (node.isDir) {
    const children = document.createElement("div");
    children.className = "children";
    (node.children || []).forEach((c) => children.appendChild(renderNode(c)));
    wrap.appendChild(children);
    label.addEventListener("click", () => wrap.classList.toggle("open"));
  } else {
    label.addEventListener("click", () => openFile(node.path));
  }
  return wrap;
}

function markActive(path) {
  document.querySelectorAll(".tree .label.active").forEach((e) => e.classList.remove("active"));
  const el = document.querySelector(`.tree .label[data-path="${cssEscape(path)}"]`);
  if (el) {
    el.classList.add("active");
    // Раскрываем родительские каталоги.
    let p = el.parentElement;
    while (p && p.classList) {
      if (p.classList.contains("dir")) p.classList.add("open");
      p = p.parentElement;
    }
  }
}

// --- Открытие файла ---
async function openFile(path) {
  state.editing = false;
  const data = await api.get("/api/file?path=" + encodeURIComponent(path));
  state.current = data;
  $("#current-path").textContent = path;
  markActive(path);

  const view = $("#view");
  const editor = $("#editor");
  editor.classList.add("hidden");
  view.classList.remove("hidden");
  $("#save-btn").classList.add("hidden");
  $("#cancel-btn").classList.add("hidden");

  if (data.type === "markdown") {
    view.innerHTML = data.html;
    bindInternalLinks(view);
    $("#edit-btn").classList.remove("hidden");
    renderBacklinks(data.backlinks || []);
  } else if (data.type === "asset") {
    $("#edit-btn").classList.add("hidden");
    view.innerHTML = renderAsset(path, data.rawUrl);
    $("#backlinks").innerHTML = "";
  }
}

function renderAsset(path, url) {
  const ext = path.split(".").pop().toLowerCase();
  if (["png", "jpg", "jpeg", "gif", "svg", "webp"].includes(ext))
    return `<img src="${url}" alt="${path}" />`;
  if (ext === "pdf") return `<iframe src="${url}" style="width:100%;height:80vh;border:none"></iframe>`;
  if (ext === "mp4") return `<video src="${url}" controls style="max-width:100%"></video>`;
  if (ext === "mp3") return `<audio src="${url}" controls></audio>`;
  return `<a href="${url}" download>Скачать ${path}</a>`;
}

// Перехват кликов по wiki-ссылкам для SPA-навигации.
function bindInternalLinks(container) {
  container.querySelectorAll('a[href^="/view"]').forEach((a) => {
    a.addEventListener("click", (e) => {
      e.preventDefault();
      const u = new URL(a.getAttribute("href"), location.origin);
      const p = u.searchParams.get("path");
      if (p) openFile(p);
    });
  });
}

function renderBacklinks(backlinks) {
  const el = $("#backlinks");
  if (!backlinks.length) {
    el.innerHTML = '<h3>Обратные ссылки</h3><div style="color:var(--text-dim);font-size:13px">Нет обратных ссылок</div>';
    return;
  }
  let html = `<h3>Обратные ссылки (${backlinks.length})</h3>`;
  for (const bl of backlinks) {
    html += `<div class="bl" data-path="${esc(bl.from)}"><div class="from">${esc(bl.from)}</div><div class="ctx">${esc(bl.context)}</div></div>`;
  }
  el.innerHTML = html;
  el.querySelectorAll(".bl").forEach((d) => d.addEventListener("click", () => openFile(d.dataset.path)));
}

// --- Редактирование ---
$("#edit-btn").addEventListener("click", () => {
  if (!state.current || state.current.type !== "markdown") return;
  state.editing = true;
  $("#editor").value = state.current.raw;
  $("#editor").classList.remove("hidden");
  $("#view").classList.add("hidden");
  $("#edit-btn").classList.add("hidden");
  $("#save-btn").classList.remove("hidden");
  $("#cancel-btn").classList.remove("hidden");
});

$("#cancel-btn").addEventListener("click", () => openFile(state.current.path));

$("#save-btn").addEventListener("click", async () => {
  const content = $("#editor").value;
  const path = state.current.path;
  const { ok, data } = await api.post("/api/save", { path, content });
  if (!ok) { alert(data.error || "Ошибка сохранения"); return; }
  await loadTree();
  await openFile(path);
});

// --- Синхронизация ---
$("#sync-btn").addEventListener("click", async () => {
  setSyncStatus("Синхронизация…");
  $("#sync-btn").disabled = true;
  const { ok, data } = await api.post("/api/sync", {});
  $("#sync-btn").disabled = false;
  if (!ok) { setSyncStatus("Ошибка: " + (data.error || "")); return; }
  setSyncStatus(`✓ ${data.filesCount} файлов` + (data.pushed ? ", запушено" : ""));
  await loadTree();
  if (state.current) await openFile(state.current.path).catch(() => {});
});

function setSyncStatus(s) { $("#sync-status").textContent = s; }

// --- Поиск ---
let searchTimer;
$("#search").addEventListener("input", (e) => {
  clearTimeout(searchTimer);
  const q = e.target.value.trim();
  const box = $("#search-results");
  if (!q) { box.classList.add("hidden"); box.innerHTML = ""; return; }
  searchTimer = setTimeout(async () => {
    const hits = await api.get("/api/search?q=" + encodeURIComponent(q));
    box.classList.remove("hidden");
    box.innerHTML = hits.map((h) =>
      `<div class="hit" data-path="${esc(h.path)}"><div class="p">${esc(h.path)}</div><div class="s">${esc(h.snippet || "")}</div></div>`
    ).join("") || '<div class="hit">Ничего не найдено</div>';
    box.querySelectorAll(".hit[data-path]").forEach((d) =>
      d.addEventListener("click", () => { openFile(d.dataset.path); box.classList.add("hidden"); }));
  }, 200);
});

// --- Утилиты ---
function esc(s) {
  return String(s || "").replace(/[&<>"]/g, (c) => ({ "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;" }[c]));
}
function cssEscape(s) { return String(s).replace(/["\\]/g, "\\$&"); }

// --- Старт: проверяем, заблокировано ли ---
(async () => {
  const st = await fetch("/api/status").then((r) => r.json()).catch(() => ({ unlocked: false }));
  if (st.unlocked) { showApp(); await init(); }
  else showLock();
})();
