"use strict";

// ---------- helpers ----------
// h() builds DOM nodes; text always goes through text nodes, never innerHTML.
function h(tag, attrs, ...children) {
  const el = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v === false || v == null) continue;
    if (k === "class") el.className = v;
    else if (k.startsWith("on")) el.addEventListener(k.slice(2), v);
    else if (k === "value") el.value = v;
    else el.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children.flat()) {
    if (c == null || c === false) continue;
    el.append(c.nodeType ? c : document.createTextNode(String(c)));
  }
  return el;
}

const MIN_PASSWORD = 8;

const state = {
  user: null,
  mustChange: false, // temporary password: only the password screen is usable
  serverVersion: "", // build of this server; agents on another build are flagged
  downloads: false,  // server ships agent binaries (enables the update command)
  view: "machines", // machines | audit
  groups: [],
  machines: [],
  filter: "all", // all | none | <group id>
  timer: null,
};

class APIError extends Error {
  constructor(status, message) { super(message); this.status = status; }
}

async function api(method, path, body) {
  const opts = { method, headers: { "X-Requested-With": "rterminal" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const resp = await fetch(path, opts);
  if (resp.status === 204) return null;
  let data = null;
  try { data = await resp.json(); } catch (_) { /* no body */ }
  if (!resp.ok) {
    if (resp.status === 401 && path !== "/api/login" && path !== "/api/password") { state.user = null; render(); }
    if (resp.status === 403 && data && data.error === "password change required") { state.mustChange = true; render(); }
    throw new APIError(resp.status, (data && data.error) || resp.statusText);
  }
  return data;
}

function toast(message, kind) {
  const el = h("div", { class: "toast " + (kind || "") }, message);
  document.getElementById("toasts").append(el);
  setTimeout(() => el.remove(), 4500);
}

const fail = (e) => toast(e.message || String(e), "err");

function timeAgo(iso) {
  if (!iso) return "nunca";
  const s = Math.max(0, (Date.now() - new Date(iso).getTime()) / 1000);
  if (s < 60) return "agora há pouco";
  if (s < 3600) return `há ${Math.floor(s / 60)} min`;
  if (s < 86400) return `há ${Math.floor(s / 3600)} h`;
  return `há ${Math.floor(s / 86400)} d`;
}

const groupName = (id) => (state.groups.find((g) => g.id === id) || {}).name || "";

async function copy(text, button) {
  try {
    await navigator.clipboard.writeText(text);
    const old = button.textContent;
    button.textContent = "Copiado!";
    setTimeout(() => (button.textContent = old), 1500);
  } catch (_) {
    toast("Não consegui copiar; selecione o texto manualmente.", "err");
  }
}

// ---------- data ----------
async function refresh() {
  const [groups, machines] = await Promise.all([api("GET", "/api/groups"), api("GET", "/api/machines")]);
  state.groups = groups;
  state.machines = machines;
}

function startPolling() {
  stopPolling();
  state.timer = setInterval(async () => {
    if (!state.user || state.view !== "machines" || document.querySelector(".overlay, .term-overlay:not([hidden])")) return;
    try { await refresh(); render(); } catch (_) { /* surfaced on next action */ }
  }, 5000);
}
function stopPolling() { clearInterval(state.timer); state.timer = null; }

// ---------- views ----------
function render() {
  const app = document.getElementById("app");
  if (!state.user) app.replaceChildren(loginView());
  else if (state.mustChange) app.replaceChildren(forcedPasswordView());
  else { app.replaceChildren(shell()); openPopupIfRequested(); }
}

function loginView() {
  const error = h("p", { class: "error", role: "alert" });
  const user = h("input", { id: "user", autocomplete: "username", required: true, autofocus: true });
  const pass = h("input", { id: "pass", type: "password", autocomplete: "current-password", required: true });
  const form = h("form", { onsubmit: async (e) => {
    e.preventDefault();
    error.textContent = "";
    try {
      const me = await api("POST", "/api/login", { username: user.value, password: pass.value });
      state.user = me.username;
      state.mustChange = !!me.must_change_password;
      state.serverVersion = me.server_version || "";
      state.downloads = !!me.agent_downloads;
      if (!state.mustChange) { await refresh(); startPolling(); }
      render();
    } catch (err) { error.textContent = err.message; }
  } },
    h("div", { class: "brand" }, "GO-RTERMINAL"),
    h("p", { class: "muted" }, "Painel de administração"),
    h("label", { for: "user" }, "Usuário"), user,
    h("label", { for: "pass" }, "Senha"), pass,
    error,
    h("div", { class: "modal-actions" }, h("button", { class: "btn primary", type: "submit" }, "Entrar")),
  );
  return h("div", { class: "login" }, form);
}

function shell() {
  const navBtn = (view, label) => h("button", {
    class: state.view === view ? "active" : "",
    onclick: async () => { state.view = view; if (view === "machines") await refresh().catch(fail); render(); },
  }, label);
  return h("div", { class: "shell" },
    h("header", { class: "topbar" },
      h("span", { class: "brand" }, "GO-RTERMINAL"),
      h("nav", { class: "nav" }, navBtn("machines", "Máquinas"), navBtn("audit", "Auditoria")),
      h("span", { class: "spacer" }),
      workspace && h("button", { class: "btn small primary", title: "Voltar aos terminais abertos", onclick: () => workspace.show() }, `Terminais (${workspace.count()})`),
      h("span", { class: "muted" }, state.user),
      h("button", { class: "btn small", onclick: () => passwordModal() }, "Alterar senha"),
      h("button", { class: "btn small", onclick: logout }, "Sair"),
    ),
    state.view === "machines" ? machinesView() : auditView(),
  );
}

// passwordForm builds the change-password fields; onDone runs after success.
function passwordForm(onDone, extra) {
  const current = h("input", { id: "pw-cur", type: "password", autocomplete: "current-password", required: true });
  const next = h("input", { id: "pw-new", type: "password", autocomplete: "new-password", required: true, minlength: String(MIN_PASSWORD) });
  const again = h("input", { id: "pw-again", type: "password", autocomplete: "new-password", required: true });
  const error = h("p", { class: "error", role: "alert" });
  const form = h("form", { onsubmit: async (e) => {
    e.preventDefault();
    error.textContent = "";
    if (next.value.length < MIN_PASSWORD) { error.textContent = `A nova senha precisa de pelo menos ${MIN_PASSWORD} caracteres.`; return; }
    if (next.value !== again.value) { error.textContent = "A confirmação não confere com a nova senha."; return; }
    try {
      await api("POST", "/api/password", { current_password: current.value, new_password: next.value });
      await onDone();
    } catch (err) { error.textContent = err.message; }
  } },
    ...extra,
    h("label", { for: "pw-cur" }, "Senha atual"), current,
    h("label", { for: "pw-new" }, "Nova senha"), next,
    h("label", { for: "pw-again" }, "Repita a nova senha"), again,
    error,
  );
  return { form, current };
}

function forcedPasswordView() {
  const { form } = passwordForm(async () => {
    state.mustChange = false;
    await refresh();
    startPolling();
    render();
    toast("Senha alterada. Bem-vindo!", "ok");
  }, [
    h("div", { class: "brand" }, "GO-RTERMINAL"),
    h("h2", null, "Defina uma nova senha"),
    h("p", { class: "muted" }, "Você entrou com uma senha temporária. Escolha uma senha própria para continuar. Use a senha temporária como “senha atual”."),
  ]);
  form.append(h("div", { class: "modal-actions" },
    h("button", { class: "btn", type: "button", onclick: logout }, "Sair"),
    h("button", { class: "btn primary", type: "submit" }, "Salvar nova senha")));
  return h("div", { class: "login" }, form);
}

function passwordModal() {
  const { form } = passwordForm(() => { close(); toast("Senha alterada. As outras sessões foram encerradas.", "ok"); }, [
    h("h2", null, "Alterar senha"),
  ]);
  form.append(h("div", { class: "modal-actions" },
    h("button", { class: "btn", type: "button", onclick: () => close() }, "Cancelar"),
    h("button", { class: "btn primary", type: "submit" }, "Salvar")));
  const close = modal(form);
}

async function logout() {
  try { await api("POST", "/api/logout"); } catch (_) { /* already gone */ }
  stopPolling();
  state.user = null;
  state.mustChange = false;
  render();
}

// --- machines ---
function visibleMachines() {
  if (state.filter === "all") return state.machines;
  if (state.filter === "none") return state.machines.filter((m) => m.group_id == null);
  return state.machines.filter((m) => m.group_id === state.filter);
}

function machinesView() {
  const online = (list) => list.filter((m) => m.online).length;
  const item = (key, label, list, tools) => h("div", { class: "group-item" + (state.filter === key ? " active" : ""), onclick: () => { state.filter = key; render(); }, role: "button", tabindex: "0",
      onkeydown: (e) => { if (e.key === "Enter") { state.filter = key; render(); } } },
    h("span", null, label),
    tools,
    h("span", { class: "count", title: "online / total" }, `${online(list)}/${list.length}`),
  );
  const sidebar = h("aside", { class: "sidebar" },
    h("h3", null, "Grupos", h("button", { class: "btn small", onclick: () => groupModal() }, "+ Novo")),
    item("all", "Todas", state.machines),
    ...state.groups.map((g) => item(g.id, g.name, state.machines.filter((m) => m.group_id === g.id),
      h("span", { class: "group-tools" },
        h("button", { title: "Editar grupo", "aria-label": "Editar grupo " + g.name, onclick: (e) => { e.stopPropagation(); groupModal(g); } }, "✎"),
        h("button", { title: "Excluir grupo", "aria-label": "Excluir grupo " + g.name, onclick: (e) => { e.stopPropagation(); deleteGroup(g); } }, "✕")))),
    item("none", "Sem grupo", state.machines.filter((m) => m.group_id == null)),
  );

  const list = visibleMachines();
  const title = state.filter === "all" ? "Todas as máquinas"
    : state.filter === "none" ? "Sem grupo" : groupName(state.filter);
  const main = h("main", { class: "main" },
    h("div", { class: "main-head" },
      h("h2", null, title),
      h("span", { class: "spacer" }),
      h("button", { class: "btn primary", onclick: () => machineModal() }, "+ Adicionar máquina"),
    ),
    list.length === 0
      ? h("div", { class: "empty" }, state.machines.length === 0
          ? "Nenhuma máquina ainda. Clique em “Adicionar máquina” para gerar o comando de instalação."
          : "Nenhuma máquina neste grupo.")
      : machinesTable(list),
  );
  return h("div", { class: "content" }, sidebar, main);
}

// An agent is outdated when it reports a build other than the server's. Builds
// made without a version ("dev") never compare as outdated.
function isOutdated(m) {
  return !!(m.version && state.serverVersion && m.version !== state.serverVersion);
}

function versionBadge(m) {
  if (!m.version) return null;
  return h("div", { class: "muted", title: `agent ${m.version}` },
    "agent ", h("span", { class: "mono" }, m.version),
    isOutdated(m) && h("span", { class: "tag warn", title: `O server está em ${state.serverVersion}. Use "Atualizar agent".` }, "desatualizado"));
}

// updateModal shows the command that downloads the current agent and restarts
// the service. It reads the credential already stored on the machine, so no
// secret appears here.
function updateModal(m) {
  const base = location.origin;
  const script = [
    "# Baixa o agent atual deste server e reinicia o servico (Linux, precisa de sudo e curl)",
    "TOKEN=$(sudo sed -n 's/^RTERMINAL_TOKEN=//p' /etc/rterminal-agent.env)",
    'case "$(uname -m)" in',
    "  x86_64) ARCH=amd64 ;;",
    "  aarch64|arm64) ARCH=arm64 ;;",
    "  armv7l|armv6l) ARCH=arm ;;",
    '  *) echo "arquitetura nao suportada: $(uname -m)" >&2; exit 1 ;;',
    "esac",
    `printf 'Authorization: Bearer %s\\n' "$TOKEN" | curl -fsSL -H @- "${base}/download/agent/linux-$ARCH" -o /tmp/rterminal-agent`,
    "sudo install -m 755 /tmp/rterminal-agent /usr/local/bin/agent.new && sudo mv -f /usr/local/bin/agent.new /usr/local/bin/agent && rm -f /tmp/rterminal-agent",
    "sudo systemctl restart rterminal-agent",
    "unset TOKEN ARCH",
  ].join("\n");
  const copyBtn = h("button", { class: "btn small" }, "Copiar");
  copyBtn.addEventListener("click", () => copy(script, copyBtn));
  const close = modal(
    h("h2", null, `Atualizar o agent de “${m.name}”`),
    h("p", { class: "muted" }, m.version
      ? `Agent atual: ${m.version}. Server: ${state.serverVersion || "—"}.`
      : "Esta máquina ainda não informou a versão do agent (provavelmente é um agent antigo)."),
    h("p", { class: "muted" }, "Rode na máquina. Serve para serviços instalados pelo comando do painel (systemd, credencial em /etc/rterminal-agent.env). A credencial não muda."),
    h("pre", { class: "cmd", tabindex: "0" }, script),
    h("div", { class: "row" }, copyBtn),
    h("div", { class: "modal-actions" }, h("button", { class: "btn primary", onclick: () => close() }, "Concluir")),
  );
}

function machinesTable(list) {
  const rows = list.map((m) => h("tr", null,
    h("td", null, h("span", { class: "dot" + (m.online ? " on" : ""), title: m.online ? "online" : "offline" }),
      h("strong", null, m.name), h("div", { class: "mono muted" }, m.id)),
    h("td", null, m.group_id != null ? h("span", { class: "tag" }, groupName(m.group_id)) : h("span", { class: "muted" }, "—")),
    h("td", null, m.hostname ? `${m.hostname}` : "—", h("div", { class: "muted" }, [m.os, m.arch].filter(Boolean).join("/")),
      versionBadge(m)),
    h("td", { class: "muted" }, m.online ? "online" : timeAgo(m.last_seen)),
    h("td", null, h("div", { class: "actions" },
      h("button", { class: "btn small primary", disabled: !m.online, title: m.online ? "" : "Máquina offline", onclick: () => openTerminal(m) }, "Terminal"),
      h("button", { class: "btn small", onclick: () => machineModal(m) }, "Editar"),
      state.downloads && m.os === "linux" && h("button", { class: "btn small" + (isOutdated(m) ? " primary" : ""), title: "Comando para atualizar o agent desta máquina, sem trocar a credencial", onclick: () => updateModal(m) }, "Atualizar agent"),
      h("button", { class: "btn small", title: "Gera uma nova credencial e desconecta o agent atual", onclick: () => rotate(m) }, "Nova credencial"),
      h("button", { class: "btn small danger", onclick: () => removeMachine(m) }, "Excluir"),
    )),
  ));
  return h("table", null,
    h("thead", null, h("tr", null, ["Máquina", "Grupo", "Host", "Visto", ""].map((t) => h("th", null, t)))),
    h("tbody", null, rows));
}

// --- modals ---
function modal(...content) {
  const overlay = h("div", { class: "overlay", onmousedown: (e) => { if (e.target === overlay) close(); } },
    h("div", { class: "modal", role: "dialog", "aria-modal": "true" }, ...content));
  const close = () => { overlay.remove(); document.removeEventListener("keydown", onKey); };
  const onKey = (e) => { if (e.key === "Escape") close(); };
  document.addEventListener("keydown", onKey);
  document.body.append(overlay);
  const first = overlay.querySelector("input, select, button");
  if (first) first.focus();
  return close;
}

function groupModal(group) {
  const name = h("input", { id: "g-name", value: group ? group.name : "", required: true, maxlength: "64" });
  const desc = h("input", { id: "g-desc", value: group ? group.description : "" });
  const error = h("p", { class: "error", role: "alert" });
  const close = modal(h("form", { onsubmit: async (e) => {
    e.preventDefault();
    try {
      const body = { name: name.value, description: desc.value };
      if (group) await api("PUT", `/api/groups/${group.id}`, body); else await api("POST", "/api/groups", body);
      close(); await refresh(); render();
    } catch (err) { error.textContent = err.message; }
  } },
    h("h2", null, group ? "Editar grupo" : "Novo grupo"),
    h("label", { for: "g-name" }, "Nome"), name,
    h("label", { for: "g-desc" }, "Descrição"), desc,
    error,
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", type: "button", onclick: () => close() }, "Cancelar"),
      h("button", { class: "btn primary", type: "submit" }, "Salvar")),
  ));
}

async function deleteGroup(g) {
  if (!confirm(`Excluir o grupo “${g.name}”? As máquinas ficam sem grupo.`)) return;
  try {
    await api("DELETE", `/api/groups/${g.id}`);
    if (state.filter === g.id) state.filter = "all";
    await refresh(); render();
  } catch (e) { fail(e); }
}

function groupSelect(selected) {
  return h("select", { id: "m-group" },
    h("option", { value: "" }, "Sem grupo"),
    ...state.groups.map((g) => h("option", { value: g.id, selected: g.id === selected }, g.name)));
}

function machineModal(machine) {
  const name = h("input", { id: "m-name", value: machine ? machine.name : "", required: true, maxlength: "64", placeholder: "ex: Raspberry Pi" });
  const preset = machine ? machine.group_id : (typeof state.filter === "number" ? state.filter : null);
  const group = groupSelect(preset);
  const error = h("p", { class: "error", role: "alert" });
  const close = modal(h("form", { onsubmit: async (e) => {
    e.preventDefault();
    const body = { name: name.value, group_id: group.value === "" ? null : Number(group.value) };
    try {
      if (machine) {
        await api("PUT", `/api/machines/${machine.id}`, body);
        close(); await refresh(); render();
      } else {
        const created = await api("POST", "/api/machines", body);
        close(); await refresh(); render();
        credentialModal(created);
      }
    } catch (err) { error.textContent = err.message; }
  } },
    h("h2", null, machine ? "Editar máquina" : "Adicionar máquina"),
    !machine && h("p", { class: "muted" }, "Depois de salvar, mostramos o comando para instalar o agent nela."),
    h("label", { for: "m-name" }, "Nome"), name,
    h("label", { for: "m-group" }, "Grupo"), group,
    error,
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", type: "button", onclick: () => close() }, "Cancelar"),
      h("button", { class: "btn primary", type: "submit" }, machine ? "Salvar" : "Criar e gerar comando")),
  ));
}

function credentialModal(created) {
  const c = created.commands;
  // With downloads the commands fetch the right agent binary from this server;
  // otherwise they assume `agent` is already installed on the machine.
  const tabs = c.downloads ? [
    ["Linux (systemd)", c.systemd, "Baixa o agent para a arquitetura desta máquina, instala o serviço e inicia. Rode num terminal da máquina (precisa de sudo e curl):"],
    ["Executar agora", c.binary, "Baixa o agent para ./agent e roda em primeiro plano (Linux ou macOS). Bom para testar:"],
  ] : [
    ["Binário", c.binary, "Rode na máquina, com o binário `agent` já instalado:"],
    ["systemd", c.systemd, "Instala o agent como serviço (Linux). Rode como usuário com sudo; o binário precisa estar em /usr/local/bin/agent:"],
    ["Instalar o agent", c.go_install || "", "Compila e instala o binário `agent` (precisa de Go 1.27+ e do repositório acessível). Depois use uma das outras abas:"],
  ];
  const pre = h("pre", { class: "cmd", tabindex: "0" });
  const hint = h("p", { class: "muted" });
  const copyBtn = h("button", { class: "btn small" }, "Copiar");
  let current = 0;
  const tabBar = h("div", { class: "tabs", role: "tablist" });
  const show = (i) => {
    current = i;
    pre.textContent = tabs[i][1];
    hint.textContent = tabs[i][2];
    [...tabBar.children].forEach((b, j) => b.classList.toggle("active", i === j));
  };
  tabs.forEach(([label], i) => tabBar.append(h("button", { role: "tab", type: "button", onclick: () => show(i) }, label)));
  copyBtn.addEventListener("click", () => copy(tabs[current][1], copyBtn));
  show(0);
  const close = modal(
    h("h2", null, `Conectar “${created.machine.name}”`),
    h("div", { class: "warn" }, "A credencial abaixo é exibida só desta vez. Guarde o comando agora; se perder, gere uma nova credencial."),
    tabBar, hint, pre,
    h("div", { class: "row" }, copyBtn, h("span", { class: "muted" }, "ID da máquina: ", h("code", null, created.machine.id))),
    h("div", { class: "modal-actions" }, h("button", { class: "btn primary", onclick: () => close() }, "Concluir")),
  );
}

async function rotate(m) {
  if (!confirm(`Gerar nova credencial para “${m.name}”? O agent atual será desconectado e a credencial antiga deixa de valer.`)) return;
  try {
    const created = await api("POST", `/api/machines/${m.id}/rotate`);
    await refresh(); render();
    credentialModal(created);
  } catch (e) { fail(e); }
}

async function removeMachine(m) {
  if (!confirm(`Excluir a máquina “${m.name}”? O agent será desconectado.`)) return;
  try { await api("DELETE", `/api/machines/${m.id}`); await refresh(); render(); toast("Máquina excluída", "ok"); }
  catch (e) { fail(e); }
}

// --- terminal ---
// The browser can only use fonts installed on the computer that is viewing the
// panel. Prompt themes (Spaceship, Powerlevel10k) and tools like eza print Nerd
// Font / Font Awesome icons, so the stack tries common Nerd Font names first and
// ends with a bundled symbols font ("RT Symbols NF") so icons never turn into boxes.
const TERM_FONT_KEY = "rt.term.font";
const TERM_SIZE_KEY = "rt.term.size";
const TERM_RENDERER_KEY = "rt.term.renderer"; // "dom" (default) or "webgl"
const NERD_FONTS = ["MesloLGS NF", "MesloLGS Nerd Font", "JetBrainsMono Nerd Font", "JetBrainsMono NF",
  "FiraCode Nerd Font", "Hack Nerd Font", "CaskaydiaCove Nerd Font", "UbuntuMono Nerd Font",
  "DejaVuSansM Nerd Font", "Symbols Nerd Font Mono"];
const MONO_FALLBACK = ["ui-monospace", "Menlo", "Consolas", '"DejaVu Sans Mono"', "monospace"];

function store(key, value) {
  try { if (value === null) localStorage.removeItem(key); else localStorage.setItem(key, value); } catch (_) { /* private mode */ }
}
function stored(key) {
  try { return localStorage.getItem(key) || ""; } catch (_) { return ""; }
}
const quoteFont = (name) => {
  const clean = name.trim().replace(/^["']|["']$/g, "").replace(/["\\]/g, "");
  return clean ? `"${clean}"` : "";
};
function termFontFamily() {
  const custom = stored(TERM_FONT_KEY).split(",").map(quoteFont).filter(Boolean);
  return [...custom, ...NERD_FONTS.map(quoteFont), '"RT Symbols NF"', ...MONO_FALLBACK].join(", ");
}
function termFontSize() {
  const n = parseInt(stored(TERM_SIZE_KEY), 10);
  return n >= 8 && n <= 32 ? n : 14;
}

// writeClipboard copies text, falling back to execCommand when the async
// Clipboard API is unavailable (insecure context) or denied.
async function writeClipboard(text) {
  try {
    await navigator.clipboard.writeText(text);
    return true;
  } catch (_) {
    const area = h("textarea", { style: "position:fixed;opacity:0;top:0;left:0" });
    area.value = text;
    document.body.append(area);
    area.select();
    let ok = false;
    try { ok = document.execCommand("copy"); } catch (_) { /* not allowed */ }
    area.remove();
    return ok;
  }
}

// terminalKeyHandler decides which key presses xterm.js must NOT send to the
// shell, so the browser can copy and paste:
//   Ctrl/Cmd+C with a selection, or Ctrl+Shift+C  -> copy the selection
//   Ctrl+C without a selection                    -> goes to the shell (SIGINT)
//   Ctrl/Cmd+V, Ctrl+Shift+V                      -> browser paste (xterm would send ^V)
function terminalKeyHandler(term) {
  return (ev) => {
    if (ev.type !== "keydown" || !(ev.ctrlKey || ev.metaKey) || ev.altKey) return true;
    const key = ev.key.toLowerCase();
    if (key === "c" && (term.hasSelection() || ev.shiftKey)) {
      const text = term.getSelection();
      if (text) {
        writeClipboard(text).then((ok) => ok ? toast("Copiado", "ok") : toast("Não consegui copiar; use o menu do botão direito.", "err"));
        term.clearSelection();
      }
      ev.preventDefault(); // also keeps Chrome from opening DevTools on Ctrl+Shift+C
      return false;
    }
    if (key === "v") return false; // no preventDefault: the browser then pastes into xterm's textarea
    return true;
  };
}

// loadTerminalFonts waits (briefly) for the bundled icon font so xterm measures
// its character cells with the final fonts.
function loadTerminalFonts() {
  const load = document.fonts && document.fonts.load
    ? document.fonts.load('14px "RT Symbols NF"', "\ue0b0\uf07b").catch(() => {})
    : Promise.resolve();
  return Promise.race([load, new Promise((resolve) => setTimeout(resolve, 2000))]);
}

function fontModal(onChange) {
  const family = h("input", { id: "f-family", value: stored(TERM_FONT_KEY), placeholder: "ex: MesloLGS NF, JetBrainsMono Nerd Font" });
  const size = h("input", { id: "f-size", type: "number", min: "8", max: "32", value: String(termFontSize()) });
  const renderer = h("select", { id: "f-renderer" },
    h("option", { value: "dom", selected: stored(TERM_RENDERER_KEY) !== "webgl" }, "Padrão (DOM)"),
    h("option", { value: "webgl", selected: stored(TERM_RENDERER_KEY) === "webgl" }, "WebGL (desenha em canvas; tente se algo aparecer fora do lugar)"));
  const close = modal(h("form", { onsubmit: (e) => {
    e.preventDefault();
    store(TERM_FONT_KEY, family.value.trim() || null);
    store(TERM_SIZE_KEY, String(Number(size.value) || 14));
    store(TERM_RENDERER_KEY, renderer.value === "webgl" ? "webgl" : null);
    onChange();
    close();
  } },
    h("h2", null, "Fonte do terminal"),
    h("p", { class: "muted" }, "O navegador só enxerga as fontes instaladas neste computador. Informe o nome da sua fonte Nerd Font (a mesma do seu terminal local) para ter ícones e glifos do Powerline idênticos. Sem uma fonte instalada, os ícones usam a fonte de símbolos embutida."),
    h("label", { for: "f-family" }, "Fonte (nomes separados por vírgula)"), family,
    h("label", { for: "f-size" }, "Tamanho"), size,
    h("label", { for: "f-renderer" }, "Renderizador"), renderer,
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", type: "button", onclick: () => { family.value = ""; size.value = "14"; renderer.value = "dom"; } }, "Restaurar padrão"),
      h("button", { class: "btn", type: "button", onclick: () => close() }, "Cancelar"),
      h("button", { class: "btn primary", type: "submit" }, "Aplicar")),
  ));
}

// escapeBytes renders terminal traffic readably: control characters and
// private-use (icon) glyphs are shown as escapes so cursor movements are visible.
function escapeBytes(text) {
  let out = "";
  for (const ch of text) {
    const c = ch.codePointAt(0);
    if (c === 0x1b) out += "\\e";
    else if (c === 0x08) out += "\\b";
    else if (c === 0x0d) out += "\\r";
    else if (c === 0x0a) out += "\\n";
    else if (c === 0x09) out += "\\t";
    else if (c < 0x20 || c === 0x7f) out += "\\x" + c.toString(16).padStart(2, "0");
    else if ((c >= 0xe000 && c <= 0xf8ff) || c >= 0xf0000 || (c >= 0x2500 && c <= 0x27bf) || c === 0x2026) out += `\\u{${c.toString(16)}}`;
    else out += ch;
  }
  return out;
}

// --- terminal workspace ---
// One overlay holds several terminal sessions ("panes"). Tabs pick the visible
// one; the split layouts show every session at once. Each pane owns its own
// WebSocket, so each tab is an independent shell on its machine.
const TERM_LAYOUT_KEY = "rt.term.layout"; // tabs | cols | rows | grid
const LAYOUTS = [["tabs", "Abas"], ["cols", "Lado a lado"], ["rows", "Empilhados"], ["grid", "Grade"]];
let workspace = null;

// A window opened with the popup button carries #popup=<machine id>: it shows
// just that machine's terminal, full window.
const popupId = location.hash.startsWith("#popup=") ? decodeURIComponent(location.hash.slice(7)) : "";
let popupOpened = false;

function openPopupIfRequested() {
  if (!popupId || popupOpened) return;
  popupOpened = true;
  const m = state.machines.find((x) => x.id === popupId);
  if (!m) { toast("Máquina não encontrada.", "err"); return; }
  document.title = m.name;
  workspace = createWorkspace(true);
  workspace.add(m);
  workspace.show();
}

function openTerminal(machine) {
  if (!workspace) workspace = createWorkspace(false);
  workspace.add(machine);
  workspace.show();
}

// createPane builds one terminal session. hooks: onState(state), onClose().
function createPane(machine, label, hooks) {
  const status = h("span", { class: "muted" }, "conectando…");
  const reconnect = h("button", { class: "btn small", hidden: true }, "Reconectar");
  const host = h("div", { class: "term" });
  let term = null, fit = null, ws = null, ended = false, webgl = null, observer = null;
  // Diagnostics: records what the terminal sends and receives (see traceModal).
  const trace = { on: false, started: 0, lines: [] };
  const decoder = new TextDecoder("utf-8", { fatal: false });
  const record = (dir, text) => {
    if (!trace.on) return;
    if (trace.lines.length >= 3000) trace.lines.shift();
    trace.lines.push(`${String(Math.round(performance.now() - trace.started)).padStart(6)}ms ${dir} ${escapeBytes(text)}`);
  };
  const title = h("strong", null, label);
  const el = h("section", { class: "term-pane" },
    h("div", { class: "pane-bar" },
      title, status, h("span", { class: "spacer" }), reconnect,
      h("button", { class: "btn small", title: "Grava o que o terminal envia e recebe, para investigar problemas de exibição", onclick: () => traceModal() }, "Diagnóstico"),
      h("button", { class: "btn small", title: "Fechar este terminal", onclick: () => hooks.onClose() }, "✕")),
    h("div", { class: "term-host" }, host));

  const enc = new TextEncoder();
  const setState = (s, text) => { status.textContent = text; hooks.onState(s); };
  const sendResize = () => {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", rows: term.rows, cols: term.cols }));
  };
  let fitQueued = false;
  const queueFit = () => {
    if (fitQueued || !fit) return;
    fitQueued = true;
    requestAnimationFrame(() => { fitQueued = false; if (!ended && host.offsetWidth > 0) fit.fit(); });
  };

  // setRenderer switches between the DOM renderer and the WebGL (canvas) one.
  function setRenderer() {
    const wantWebgl = stored(TERM_RENDERER_KEY) === "webgl" && typeof WebglAddon !== "undefined";
    if (wantWebgl && !webgl) {
      try {
        webgl = new WebglAddon.WebglAddon();
        webgl.onContextLoss(() => { webgl.dispose(); webgl = null; });
        term.loadAddon(webgl);
      } catch (_) {
        webgl = null;
        toast("WebGL indisponível neste navegador; usando o renderizador padrão.", "err");
      }
    } else if (!wantWebgl && webgl) {
      webgl.dispose();
      webgl = null;
    }
  }

  function applyFont() {
    if (!term) return;
    term.options.fontFamily = termFontFamily();
    term.options.fontSize = termFontSize();
    setRenderer();
    queueFit();
    sendResize();
  }

  function connect() {
    reconnect.hidden = true;
    setState("connecting", "conectando…");
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}/ws/terminal?machine=${encodeURIComponent(machine.id)}`);
    ws.binaryType = "arraybuffer";
    ws.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        let msg = null;
        try { msg = JSON.parse(ev.data); } catch (_) { return; }
        if (msg.type === "paired") { setState("on", "conectado"); queueFit(); sendResize(); }
        return;
      }
      const bytes = new Uint8Array(ev.data);
      if (trace.on) record("<", decoder.decode(bytes, { stream: true }));
      term.write(bytes);
    };
    ws.onclose = (ev) => {
      setState("off", "desconectado");
      term.write(`\r\n\x1b[33m[sessão encerrada${ev.reason ? ": " + ev.reason : ""}]\x1b[0m\r\n`);
      reconnect.hidden = false;
    };
    ws.onerror = () => setState("off", "erro de conexão");
  }

  // start must run once el is attached to the document.
  function start() {
    if (ended) return;
    term = new Terminal({
      cursorBlink: true, fontFamily: termFontFamily(), fontSize: termFontSize(),
      theme: { background: "#1a1c2c", foreground: "#f4f4f4", cursor: "#ffcd75" },
    });
    fit = new FitAddon.FitAddon();
    term.loadAddon(fit);
    term.open(host);
    term.attachCustomKeyEventHandler(terminalKeyHandler(term));
    setRenderer();
    fit.fit();
    term.onData((d) => {
      record(">", d);
      if (ws && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d));
    });
    term.onResize(sendResize);
    // Refit whenever the pane's box changes: window resize, layout switch,
    // tab shown again after being hidden.
    observer = new ResizeObserver(queueFit);
    observer.observe(host);
    reconnect.addEventListener("click", () => { term.clear(); connect(); term.focus(); });
    connect();
  }

  function traceModal() {
    const box = h("textarea", { readonly: true, rows: "14", style: "width:100%;font:12px/1.4 var(--mono);white-space:pre;" });
    // What xterm itself holds in its buffer: compare with what is painted.
    const bufferSnapshot = () => {
      if (!term) return "";
      const b = term.buffer.active;
      const lines = [];
      const cursorLine = b.baseY + b.cursorY;
      for (let y = Math.max(0, cursorLine - 13); y <= cursorLine; y++) {
        const line = b.getLine(y);
        const text = line ? line.translateToString(true) : "";
        if (text.trim()) lines.push(`${String(y).padStart(4)}| ${escapeBytes(text)}`);
      }
      return `buffer do xterm (cursor em x=${b.cursorX} y=${b.cursorY + b.baseY}, renderizador ${webgl ? "webgl" : "dom"}):\n` + lines.join("\n");
    };
    const info = () => [
      `terminal: ${term ? term.cols : "?"}x${term ? term.rows : "?"} | dpr ${window.devicePixelRatio} | ${navigator.userAgent}`,
      `fonte: ${term ? term.options.fontFamily : ""} ${term ? term.options.fontSize : ""}px`,
      `Legenda: ">" = teclas enviadas, "<" = saida recebida; \\e = ESC, \\b = backspace.`,
    ].join("\n");
    const toggle = h("button", { class: "btn primary", type: "button" });
    const refreshBox = () => {
      toggle.textContent = trace.on ? "Parar gravação" : "Iniciar gravação";
      box.value = info() + "\n\n" + bufferSnapshot() + "\n\nfluxo gravado:\n" + trace.lines.join("\n");
    };
    toggle.addEventListener("click", () => {
      trace.on = !trace.on;
      if (trace.on) { trace.started = performance.now(); trace.lines = []; }
      refreshBox();
    });
    const copyBtn = h("button", { class: "btn", type: "button" }, "Copiar");
    copyBtn.addEventListener("click", () => copy(box.value, copyBtn));
    refreshBox();
    const close = modal(
      h("h2", null, "Diagnóstico do terminal"),
      h("p", { class: "muted" }, "1) Clique em Iniciar gravação e feche esta janela. 2) Digite, por exemplo, ls no terminal (sem Enter). 3) Reabra o Diagnóstico, clique em Parar e em Copiar, e envie o texto. Ele contém o que você digitou nesse intervalo: não grave senhas."),
      box,
      h("div", { class: "modal-actions" }, copyBtn, toggle, h("button", { class: "btn", type: "button", onclick: () => close() }, "Fechar")),
    );
  }

  function dispose() {
    ended = true;
    if (observer) observer.disconnect();
    if (ws) { ws.onclose = null; ws.close(); }
    if (term) term.dispose();
    el.remove();
  }

  return { el, machine, start, applyFont, dispose, focus: () => term && term.focus() };
}

// createWorkspace builds the overlay with the tab bar, layout switch and panes.
function createWorkspace(popup) {
  const tabs = [];       // {id, label, pane, state}
  const counters = {};   // machine id -> sessions opened so far (for "#2" labels)
  let active = null, nextId = 1, layout = stored(TERM_LAYOUT_KEY);
  if (!LAYOUTS.some(([v]) => v === layout)) layout = "tabs";

  const tabBar = h("div", { class: "term-tabs", role: "tablist" });
  const grid = h("div", { class: "term-grid" });
  const picker = h("select", { class: "pick", title: "Abrir outro terminal", "aria-label": "Abrir outro terminal" });
  const layoutSel = h("select", { class: "pick", title: "Disposição dos terminais", "aria-label": "Disposição dos terminais",
    onchange: () => { layout = layoutSel.value; store(TERM_LAYOUT_KEY, layout); applyLayout(); } },
    ...LAYOUTS.map(([v, t]) => h("option", { value: v, selected: v === layout }, t)));

  const fillPicker = () => {
    picker.replaceChildren(h("option", { value: "" }, "+ Novo terminal…"),
      ...state.machines.filter((m) => m.online).map((m) => h("option", { value: m.id }, m.name)));
  };
  picker.addEventListener("focus", () => { refresh().then(fillPicker).catch(() => {}); });
  picker.addEventListener("change", () => {
    const m = state.machines.find((x) => x.id === picker.value);
    fillPicker();
    if (m) api_add(m);
  });
  fillPicker();

  const popoutBtn = h("button", { class: "btn small", title: "Abre uma nova sessão desta máquina em outra janela",
    onclick: () => {
      if (!active) return;
      const url = `${location.pathname}#popup=${encodeURIComponent(active.pane.machine.id)}`;
      if (!window.open(url, "_blank", "popup=yes,width=1100,height=700")) toast("O navegador bloqueou a janela; libere pop-ups para este site.", "err");
    } }, "Popup");

  const overlay = h("div", { class: "term-overlay" },
    h("div", { class: "term-bar" },
      h("span", { class: "brand" }, popup ? "Terminal" : "Terminais"),
      tabBar, popup ? null : picker, h("span", { class: "spacer" }),
      layoutSel,
      !popup && popoutBtn,
      h("button", { class: "btn small", title: "Fonte e tamanho do terminal", onclick: () => fontModal(() => tabs.forEach((t) => t.pane.applyFont())) }, "Fonte"),
      !popup && h("button", { class: "btn small", title: "Volta para a lista; os terminais continuam conectados", onclick: () => ws_.hide() }, "Minimizar"),
      h("button", { class: "btn small", title: popup ? "Fechar a janela" : "Encerra todos os terminais", onclick: () => ws_.closeAll() }, popup ? "Fechar" : "Encerrar todos")),
    grid);
  overlay.hidden = true;
  document.body.append(overlay);

  function renderTabs() {
    tabBar.replaceChildren(...tabs.map((t) => h("div", { class: "term-tab" + (t === active ? " active" : ""), role: "tab", "aria-selected": String(t === active) },
      h("button", { class: "tab-name", title: t.label, onclick: () => activate(t) },
        h("span", { class: "dot" + (t.state === "on" ? " on" : "") }), t.label),
      h("button", { class: "tab-x", title: "Fechar este terminal", "aria-label": "Fechar " + t.label, onclick: () => close(t) }, "✕"))));
  }

  function applyLayout() {
    layoutSel.value = layout;
    const n = tabs.length || 1;
    let cols = 1, rows = 1;
    if (layout === "cols") cols = n;
    else if (layout === "rows") rows = n;
    else if (layout === "grid") { cols = Math.ceil(Math.sqrt(n)); rows = Math.ceil(n / cols); }
    grid.style.gridTemplateColumns = `repeat(${cols}, minmax(0, 1fr))`;
    grid.style.gridTemplateRows = `repeat(${rows}, minmax(0, 1fr))`;
    for (const t of tabs) {
      t.pane.el.hidden = layout === "tabs" && t !== active;
      t.pane.el.classList.toggle("active", t === active && layout !== "tabs");
    }
  }

  function activate(t, focus = true) {
    if (active === t) { if (focus) t.pane.focus(); return; }
    active = t;
    applyLayout();
    renderTabs();
    if (focus) t.pane.focus();
  }

  function add(machine) {
    counters[machine.id] = (counters[machine.id] || 0) + 1;
    const label = counters[machine.id] > 1 ? `${machine.name} #${counters[machine.id]}` : machine.name;
    const t = { id: nextId++, label, state: "connecting", pane: null };
    t.pane = createPane(machine, label, {
      onState: (s) => { t.state = s; renderTabs(); },
      onClose: () => close(t),
    });
    t.pane.el.addEventListener("focusin", () => activate(t, false));
    tabs.push(t);
    grid.append(t.pane.el);
    active = t;
    applyLayout();
    renderTabs();
    // xterm measures its cell size, so it can only start once attached and with the fonts loaded.
    loadTerminalFonts().then(() => { t.pane.start(); if (active === t) t.pane.focus(); });
  }
  const api_add = (m) => { add(m); };

  function close(t) {
    const i = tabs.indexOf(t);
    if (i < 0) return;
    tabs.splice(i, 1);
    t.pane.dispose();
    if (!tabs.length) { ws_.destroy(); return; }
    if (active === t) active = tabs[Math.min(i, tabs.length - 1)];
    applyLayout();
    renderTabs();
    active.pane.focus();
  }

  const ws_ = {
    add,
    show() {
      overlay.hidden = false;
      fillPicker();
      if (active) active.pane.focus();
    },
    hide() { overlay.hidden = true; render(); },
    closeAll() {
      if (tabs.length > 1 && !confirm(`Encerrar ${tabs.length} terminais?`)) return;
      ws_.destroy();
    },
    destroy() {
      tabs.splice(0).forEach((t) => t.pane.dispose());
      overlay.remove();
      workspace = null;
      if (popup) { window.close(); return; }
      refresh().then(render).catch(() => render());
    },
    count: () => tabs.length,
    visible: () => !overlay.hidden,
  };
  return ws_;
}

// --- audit ---
function auditView() {
  const body = h("tbody");
  const main = h("main", { class: "main", style: "grid-column: 1 / -1" },
    h("div", { class: "main-head" }, h("h2", null, "Auditoria"), h("span", { class: "muted" }, "últimos 200 eventos")),
    h("table", { class: "audit" },
      h("thead", null, h("tr", null, ["Quando", "Quem", "Ação", "Alvo", "Detalhe"].map((t) => h("th", null, t)))),
      body));
  api("GET", "/api/audit?limit=200").then((entries) => {
    body.replaceChildren(...entries.map((e) => h("tr", null,
      h("td", { class: "mono" }, new Date(e.at).toLocaleString()), h("td", null, e.actor),
      h("td", null, h("span", { class: "tag" }, e.action)), h("td", { class: "mono" }, e.target), h("td", { class: "muted" }, e.detail))));
  }).catch(fail);
  return h("div", { class: "content" }, main);
}

// ---------- boot ----------
(async function boot() {
  try {
    const me = await api("GET", "/api/me");
    state.user = me.username;
    state.mustChange = !!me.must_change_password;
    state.serverVersion = me.server_version || "";
    state.downloads = !!me.agent_downloads;
    if (!state.mustChange) { await refresh(); startPolling(); }
  } catch (_) { state.user = null; }
  render();
})();
