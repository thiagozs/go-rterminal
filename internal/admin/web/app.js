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
  role: "admin",     // admin (everything) | operator (terminals of their groups only)
  mustChange: false, // temporary password: only the password screen is usable
  mustSetup2FA: false, // policy requires a second factor the account does not have yet
  twoFAAvailable: false, // the server can do two-factor authentication
  pending2FA: null,  // {challenge, methods, hint} between the password and the second factor
  serverVersion: "", // build of this server; agents on another build are flagged
  downloads: false,  // server ships agent binaries (enables the update command)
  recording: false,  // server records web terminal sessions (audit > recorded sessions)
  auditTab: "events", // events | recordings
  view: "machines", // machines | users | audit
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
    // Only an expired session logs out; a wrong code or password is a 401 too and must stay on the screen.
    if (resp.status === 401 && data && data.error === "login required") { state.user = null; state.pending2FA = null; render(); }
    if (resp.status === 403 && data && data.error === "password change required") { state.mustChange = true; render(); }
    if (resp.status === 403 && data && data.error === "two-factor setup required") { state.mustSetup2FA = true; render(); }
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

const isAdmin = () => state.role === "admin";
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

// applyMe copies the session view from /api/me or a finished login into state.
function applyMe(me) {
  state.user = me.username;
  state.role = me.role || "admin";
  state.mustChange = !!me.must_change_password;
  state.mustSetup2FA = !!me.must_setup_2fa;
  state.twoFAAvailable = !!me.two_factor_available;
  state.serverVersion = me.server_version || "";
  state.downloads = !!me.agent_downloads;
  state.recording = !!me.recording;
}
const sessionReady = () => !state.mustChange && !state.mustSetup2FA;

// enterPanel loads the data and shows the panel once nothing is pending.
async function enterPanel() {
  if (sessionReady()) { await refresh(); startPolling(); }
  render();
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
  if (!state.user) app.replaceChildren(state.pending2FA ? twoFactorLoginView() : loginView());
  else if (state.mustChange) app.replaceChildren(forcedPasswordView());
  else if (state.mustSetup2FA) app.replaceChildren(forcedSetupView());
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
      if (me.two_factor) {
        state.pending2FA = { challenge: me.challenge, methods: me.methods, hint: me.email_hint };
        render();
        return;
      }
      applyMe(me);
      state.view = "machines";
      await enterPanel();
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
      h("nav", { class: "nav" }, navBtn("machines", "Máquinas"), isAdmin() && navBtn("users", "Usuários"), isAdmin() && navBtn("audit", "Auditoria")),
      h("span", { class: "spacer" }),
      workspace && h("button", { class: "btn small primary", title: "Voltar aos terminais abertos", onclick: () => workspace.show() }, `Terminais (${workspace.count()})`),
      h("span", { class: "muted" }, state.user),
      state.twoFAAvailable && h("button", { class: "btn small", title: "Verificação em duas etapas", onclick: () => securityModal() }, "Segurança"),
      h("button", { class: "btn small", onclick: () => passwordModal() }, "Alterar senha"),
      h("button", { class: "btn small", onclick: logout }, "Sair"),
    ),
    state.view === "users" && isAdmin() ? usersView() : state.view === "audit" && isAdmin() ? auditView() : machinesView(),
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
    try { applyMe(await api("GET", "/api/me")); } catch (_) { state.mustChange = false; }
    await enterPanel();
    toast("Senha alterada.", "ok");
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
  state.mustSetup2FA = false;
  state.pending2FA = null;
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
    h("h3", null, "Grupos", isAdmin() && h("button", { class: "btn small", onclick: () => groupModal() }, "+ Novo")),
    item("all", "Todas", state.machines),
    ...state.groups.map((g) => item(g.id, g.name, state.machines.filter((m) => m.group_id === g.id),
      isAdmin() && h("span", { class: "group-tools" },
        h("button", { title: "Editar grupo", "aria-label": "Editar grupo " + g.name, onclick: (e) => { e.stopPropagation(); groupModal(g); } }, "✎"),
        h("button", { title: "Excluir grupo", "aria-label": "Excluir grupo " + g.name, onclick: (e) => { e.stopPropagation(); deleteGroup(g); } }, "✕")))),
    isAdmin() && item("none", "Sem grupo", state.machines.filter((m) => m.group_id == null)),
  );

  const list = visibleMachines();
  const title = state.filter === "all" ? "Todas as máquinas"
    : state.filter === "none" ? "Sem grupo" : groupName(state.filter);
  const main = h("main", { class: "main" },
    h("div", { class: "main-head" },
      h("h2", null, title),
      h("span", { class: "spacer" }),
      isAdmin() && h("button", { class: "btn primary", onclick: () => machineModal() }, "+ Adicionar máquina"),
    ),
    list.length === 0
      ? h("div", { class: "empty" }, state.machines.length === 0
          ? (isAdmin() ? "Nenhuma máquina ainda. Clique em “Adicionar máquina” para gerar o comando de instalação."
            : "Nenhuma máquina liberada para o seu usuário. Peça a um administrador para incluir você em um grupo.")
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
      isAdmin() && h("button", { class: "btn small", onclick: () => machineModal(m) }, "Editar"),
      isAdmin() && state.downloads && m.os === "linux" && h("button", { class: "btn small" + (isOutdated(m) ? " primary" : ""), title: "Comando para atualizar o agent desta máquina, sem trocar a credencial", onclick: () => updateModal(m) }, "Atualizar agent"),
      isAdmin() && h("button", { class: "btn small", title: "Gera uma nova credencial e desconecta o agent atual", onclick: () => rotate(m) }, "Nova credencial"),
      isAdmin() && h("button", { class: "btn small danger", onclick: () => removeMachine(m) }, "Excluir"),
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
      title, status,
      state.recording && h("span", { class: "rec", title: "Esta sessão está sendo gravada (só a saída do terminal) e pode ser revista por administradores" }, "● gravando"),
      h("span", { class: "spacer" }), reconnect,
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

// --- two-factor authentication ---
const METHOD_LABEL = { totp: "App autenticador", email: "Código por e-mail", recovery: "Código de recuperação" };

// twoFactorLoginView is the step after a correct password.
function twoFactorLoginView() {
  const p = state.pending2FA;
  const modes = [...p.methods, "recovery"];
  let mode = modes[0];
  const error = h("p", { class: "error", role: "alert" });
  const info = h("p", { class: "muted" });
  const code = h("input", { id: "tf-code", autocomplete: "one-time-code", inputmode: "text", required: true, autofocus: true, maxlength: "14" });
  const sendBtn = h("button", { class: "btn", type: "button" }, "Enviar código por e-mail");
  const tabs = h("div", { class: "tabs", role: "tablist" });
  const back = () => { state.pending2FA = null; render(); };
  const hints = {
    totp: "Digite o código de 6 dígitos que aparece no app autenticador.",
    email: `Pedimos um código por e-mail${p.hint ? " para " + p.hint : ""}. Ele vale poucos minutos.`,
    recovery: "Use um dos códigos de recuperação que você guardou. Cada um só funciona uma vez.",
  };
  const sendPIN = async () => {
    error.textContent = "";
    try {
      const out = await api("POST", "/api/login/2fa/send", { challenge: p.challenge });
      info.textContent = `Código enviado para ${out.email_hint}. Confira a caixa de entrada e o spam.`;
      code.focus();
    } catch (e) { error.textContent = e.message; }
  };
  sendBtn.addEventListener("click", sendPIN);
  const show = (m) => {
    mode = m;
    error.textContent = "";
    info.textContent = hints[m];
    sendBtn.hidden = m !== "email";
    code.value = "";
    code.placeholder = m === "recovery" ? "xxxxx-xxxxx" : "000000";
    [...tabs.children].forEach((b, i) => b.classList.toggle("active", modes[i] === m));
  };
  modes.forEach((m) => tabs.append(h("button", { type: "button", role: "tab", onclick: () => show(m) }, METHOD_LABEL[m])));
  const form = h("form", { onsubmit: async (e) => {
    e.preventDefault();
    error.textContent = "";
    try {
      const me = await api("POST", "/api/login/2fa/verify", { challenge: p.challenge, method: mode, code: code.value });
      state.pending2FA = null;
      applyMe(me);
      state.view = "machines";
      await enterPanel();
    } catch (err) {
      if (err.status === 401 && /log in again|expired|start again/.test(err.message)) { state.pending2FA = null; render(); toast("Login expirado. Entre de novo.", "err"); return; }
      error.textContent = err.message;
      code.select();
    }
  } },
    h("div", { class: "brand" }, "GO-RTERMINAL"),
    h("h2", null, "Verificação em duas etapas"),
    tabs, info,
    h("label", { for: "tf-code" }, "Código"), code,
    sendBtn,
    error,
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", type: "button", onclick: back }, "Voltar"),
      h("button", { class: "btn primary", type: "submit" }, "Entrar")));
  show(mode);
  if (mode === "email") sendPIN(); // e-mail is the only method: send right away
  return h("div", { class: "login" }, form);
}

// askPassword asks for the current password; resolves to it, or null if cancelled.
function askPassword(title, explanation) {
  return new Promise((resolve) => {
    const pass = h("input", { id: "ask-pass", type: "password", autocomplete: "current-password", required: true });
    let answered = false;
    const done = (value) => { answered = true; close(); resolve(value); };
    const close = modal(h("form", { onsubmit: (e) => { e.preventDefault(); done(pass.value); } },
      h("h2", null, title), h("p", { class: "muted" }, explanation),
      h("label", { for: "ask-pass" }, "Sua senha"), pass,
      h("div", { class: "modal-actions" },
        h("button", { class: "btn", type: "button", onclick: () => done(null) }, "Cancelar"),
        h("button", { class: "btn primary", type: "submit" }, "Confirmar"))));
    // Escape or a click outside also closes the modal: treat as cancel.
    const watch = setInterval(() => { if (!answered && !document.body.contains(pass)) { clearInterval(watch); answered = true; resolve(null); } }, 200);
  });
}

// securityPanel manages the account's second factors. onChanged runs after a
// change has been saved and the person is done with the recovery codes.
function securityPanel(onChanged) {
  const box = h("div", { class: "security" });
  const fail2 = (e) => toast(e.message || String(e), "err");

  async function reload() {
    let st;
    try { st = await api("GET", "/api/2fa"); } catch (e) { box.replaceChildren(h("p", { class: "error" }, e.message)); return; }
    draw(st);
  }
  async function changed(codes) {
    if (codes && codes.length) { showCodes(codes); return; }
    await onChanged();
    reload();
  }
  function showCodes(codes) {
    const text = codes.join("\n");
    const copyBtn = h("button", { class: "btn small" }, "Copiar");
    copyBtn.addEventListener("click", () => copy(text, copyBtn));
    const saveBtn = h("button", { class: "btn small" }, "Baixar .txt");
    saveBtn.addEventListener("click", () => {
      const a = h("a", { href: URL.createObjectURL(new Blob([`Códigos de recuperação (${state.user})\n\n${text}\n`], { type: "text/plain" })), download: "codigos-de-recuperacao.txt" });
      document.body.append(a); a.click(); a.remove();
    });
    box.replaceChildren(
      h("h3", null, "Códigos de recuperação"),
      h("div", { class: "warn" }, "Guarde estes códigos num lugar seguro (um gerenciador de senhas, por exemplo). Cada um entra uma vez se você perder o celular ou o acesso ao e-mail. Eles não serão mostrados de novo."),
      h("pre", { class: "cmd codes", tabindex: "0" }, text),
      h("div", { class: "row" }, copyBtn, saveBtn),
      h("div", { class: "modal-actions" }, h("button", { class: "btn primary", onclick: async () => { await onChanged(); reload(); } }, "Guardei os códigos")));
  }

  function draw(st) {
    const sections = [];
    // authenticator app
    const totpBody = h("div");
    if (st.totp) {
      totpBody.append(h("p", null, h("span", { class: "tag ok" }, "ativo")),
        h("button", { class: "btn small danger", onclick: async () => {
          const pw = await askPassword("Desativar o app autenticador", "Confirme com a sua senha.");
          if (pw == null) return;
          try { await api("POST", "/api/2fa/totp/disable", { password: pw }); toast("App autenticador desativado", "ok"); await changed(); } catch (e) { fail2(e); }
        } }, "Desativar"));
    } else {
      totpBody.append(h("p", { class: "muted" }, "Google Authenticator, Authy, 1Password, Microsoft Authenticator e similares."),
        h("button", { class: "btn small primary", onclick: () => totpSetup(totpBody) }, "Configurar"));
    }
    sections.push(h("section", null, h("h3", null, "App autenticador"), totpBody));
    // e-mail
    const mailBody = h("div");
    if (st.email_pin) {
      mailBody.append(h("p", null, h("span", { class: "tag ok" }, "ativo"), " ", h("span", { class: "muted" }, st.email)),
        h("button", { class: "btn small danger", onclick: async () => {
          const pw = await askPassword("Desativar o código por e-mail", "Confirme com a sua senha.");
          if (pw == null) return;
          try { await api("POST", "/api/2fa/email/disable", { password: pw }); toast("Código por e-mail desativado", "ok"); await changed(); } catch (e) { fail2(e); }
        } }, "Desativar"));
    } else if (st.email_possible) {
      mailBody.append(h("p", { class: "muted" }, `Enviamos um código de 6 dígitos para ${st.email} a cada login.`),
        h("button", { class: "btn small primary", onclick: () => emailEnable(mailBody, st.email) }, "Ativar"));
    } else {
      mailBody.append(h("p", { class: "muted" }, "Indisponível: o servidor não tem envio de e-mail configurado ou a sua conta não tem e-mail cadastrado (peça a um administrador em Usuários)."));
    }
    sections.push(h("section", null, h("h3", null, "Código por e-mail"), mailBody));
    // recovery
    if (st.totp || st.email_pin) {
      sections.push(h("section", null, h("h3", null, "Códigos de recuperação"),
        h("p", { class: "muted" }, `${st.recovery_left} de 10 códigos restantes.`),
        h("button", { class: "btn small", onclick: async () => {
          const pw = await askPassword("Gerar novos códigos", "Os códigos atuais deixam de valer. Confirme com a sua senha.");
          if (pw == null) return;
          try { const out = await api("POST", "/api/2fa/recovery", { password: pw }); await changed(out.recovery_codes); } catch (e) { fail2(e); }
        } }, "Gerar novos códigos")));
    }
    box.replaceChildren(...(st.required && !st.totp && !st.email_pin
      ? [h("div", { class: "warn" }, "A verificação em duas etapas é obrigatória para a sua conta. Ative pelo menos um método para continuar.")] : []), ...sections);
  }

  async function totpSetup(target) {
    try {
      const s = await api("POST", "/api/2fa/totp/setup");
      const code = h("input", { id: "totp-code", autocomplete: "one-time-code", inputmode: "numeric", maxlength: "7", placeholder: "000000", required: true });
      const error = h("p", { class: "error", role: "alert" });
      target.replaceChildren(
        h("p", { class: "muted" }, "1) Abra o app autenticador e escaneie o QR code (ou digite a chave). 2) Digite o código de 6 dígitos que o app mostrar."),
        h("img", { class: "qr", src: s.qr, alt: "QR code para o app autenticador", width: "240", height: "240" }),
        h("p", { class: "muted" }, "Chave manual: ", h("code", { class: "mono" }, s.secret)),
        h("form", { onsubmit: async (e) => {
          e.preventDefault();
          try { const out = await api("POST", "/api/2fa/totp/enable", { code: code.value }); toast("App autenticador ativado", "ok"); await changed(out.recovery_codes); }
          catch (err) { error.textContent = err.message; code.select(); }
        } }, h("label", { for: "totp-code" }, "Código do app"), code, error,
          h("div", { class: "modal-actions" },
            h("button", { class: "btn", type: "button", onclick: () => reload() }, "Cancelar"),
            h("button", { class: "btn primary", type: "submit" }, "Ativar"))));
      code.focus();
    } catch (e) { fail2(e); }
  }

  async function emailEnable(target, masked) {
    try { await api("POST", "/api/2fa/email/send"); } catch (e) { fail2(e); return; }
    const code = h("input", { id: "email-code", autocomplete: "one-time-code", inputmode: "numeric", maxlength: "7", placeholder: "000000", required: true });
    const error = h("p", { class: "error", role: "alert" });
    target.replaceChildren(
      h("p", { class: "muted" }, `Enviamos um código para ${masked}. Digite-o para confirmar que você recebe os e-mails.`),
      h("form", { onsubmit: async (e) => {
        e.preventDefault();
        try { const out = await api("POST", "/api/2fa/email/enable", { code: code.value }); toast("Código por e-mail ativado", "ok"); await changed(out.recovery_codes); }
        catch (err) { error.textContent = err.message; code.select(); }
      } }, h("label", { for: "email-code" }, "Código recebido"), code, error,
        h("div", { class: "modal-actions" },
          h("button", { class: "btn", type: "button", onclick: () => reload() }, "Cancelar"),
          h("button", { class: "btn primary", type: "submit" }, "Ativar"))));
    code.focus();
  }

  reload();
  return box;
}

function securityModal() {
  const panel = securityPanel(async () => { try { applyMe(await api("GET", "/api/me")); } catch (_) { /* keep going */ } });
  const close = modal(h("h2", null, "Verificação em duas etapas"),
    h("p", { class: "muted" }, "Além da senha, pedimos um segundo fator a cada login."),
    panel,
    h("div", { class: "modal-actions" }, h("button", { class: "btn", onclick: () => close() }, "Fechar")));
}

// forcedSetupView blocks the panel until the policy is met.
function forcedSetupView() {
  const panel = securityPanel(async () => {
    applyMe(await api("GET", "/api/me"));
    await enterPanel();
  });
  return h("div", { class: "login" },
    h("div", { class: "setup" },
      h("div", { class: "brand" }, "GO-RTERMINAL"),
      h("h2", null, "Ative a verificação em duas etapas"),
      panel,
      h("div", { class: "modal-actions" }, h("button", { class: "btn", type: "button", onclick: logout }, "Sair"))));
}

// --- users ---
const ROLE_LABEL = { admin: "Administrador", operator: "Operador" };

function usersView() {
  const body = h("tbody");
  const load = () => api("GET", "/api/users").then((users) => {
    body.replaceChildren(...users.map((u) => h("tr", null,
      h("td", null, h("strong", null, u.username), u.must_change_password && h("span", { class: "tag warn" }, "senha temporária"),
        h("div", { class: "muted" }, u.email || "sem e-mail")),
      h("td", null, h("span", { class: "tag" }, ROLE_LABEL[u.role] || u.role)),
      h("td", null, u.totp_enabled && h("span", { class: "tag ok", title: "App autenticador" }, "app"),
        u.email_pin_enabled && h("span", { class: "tag ok", title: "Código por e-mail" }, "e-mail"),
        !u.totp_enabled && !u.email_pin_enabled && h("span", { class: "muted" }, "—")),
      h("td", { class: "muted" }, u.role === "admin" ? "todas as máquinas"
        : u.group_ids.length ? u.group_ids.map(groupName).filter(Boolean).join(", ") : "nenhum grupo (não vê máquinas)"),
      h("td", null, h("div", { class: "actions" },
        h("button", { class: "btn small", onclick: () => userModal(u, load) }, "Editar"),
        h("button", { class: "btn small", title: "Gera uma senha temporária e encerra as sessões deste usuário", onclick: () => resetUser(u) }, "Nova senha"),
        (u.totp_enabled || u.email_pin_enabled) && h("button", { class: "btn small", title: "Remove a verificação em duas etapas (perdeu o celular), encerra as sessões e deixa configurar de novo", onclick: () => reset2FA(u, load) }, "Zerar 2FA"),
        u.username !== state.user && h("button", { class: "btn small danger", onclick: () => deleteUser(u, load) }, "Excluir"),
      )))));
  }).catch(fail);
  refresh().then(load).catch(fail); // group names come from state.groups
  const main = h("main", { class: "main", style: "grid-column: 1 / -1" },
    h("div", { class: "main-head" }, h("h2", null, "Usuários"), h("span", { class: "spacer" }),
      h("button", { class: "btn primary", onclick: () => userModal(null, load) }, "+ Novo usuário")),
    h("p", { class: "muted" }, "Administradores veem e gerenciam tudo. Operadores só abrem terminal nas máquinas dos grupos liberados para eles."),
    h("table", null,
      h("thead", null, h("tr", null, ["Usuário", "Papel", "2FA", "Acesso", ""].map((x) => h("th", null, x)))),
      body));
  return h("div", { class: "content" }, main);
}

function userModal(user, reload) {
  const name = h("input", { id: "u-name", value: user ? user.username : "", required: !user, disabled: !!user, maxlength: "64", autocomplete: "off" });
  const email = h("input", { id: "u-email", type: "email", value: user ? user.email : "", maxlength: "120", placeholder: "nome@exemplo.com", autocomplete: "off" });
  const role = h("select", { id: "u-role" },
    h("option", { value: "operator", selected: !user || user.role === "operator" }, "Operador (só os grupos marcados)"),
    h("option", { value: "admin", selected: !!user && user.role === "admin" }, "Administrador (acesso total)"));
  const checks = state.groups.map((g) => ({ g, box: h("input", { type: "checkbox", id: "u-g-" + g.id, checked: !!user && user.group_ids.includes(g.id) }) }));
  const groupsBox = h("div", { class: "checks" },
    checks.length ? checks.map(({ g, box }) => h("label", { class: "check", for: "u-g-" + g.id }, box, g.name))
      : h("span", { class: "muted" }, "Crie um grupo antes para liberar máquinas a um operador."));
  const sync = () => { groupsBox.hidden = role.value !== "operator"; groupsLabel.hidden = groupsBox.hidden; };
  const groupsLabel = h("label", null, "Grupos liberados");
  role.addEventListener("change", sync);
  const error = h("p", { class: "error", role: "alert" });
  const close = modal(h("form", { onsubmit: async (e) => {
    e.preventDefault();
    const group_ids = checks.filter((c) => c.box.checked).map((c) => c.g.id);
    try {
      if (user) {
        await api("PUT", `/api/users/${user.id}`, { role: role.value, group_ids, email: email.value.trim() });
        close(); reload(); toast("Usuário atualizado", "ok");
      } else {
        const created = await api("POST", "/api/users", { username: name.value, role: role.value, group_ids, email: email.value.trim() });
        close(); reload();
        temporaryPasswordModal(created.user.username, created.temporary_password);
      }
    } catch (err) { error.textContent = err.message; }
  } },
    h("h2", null, user ? `Editar ${user.username}` : "Novo usuário"),
    !user && h("p", { class: "muted" }, "Geramos uma senha temporária, mostrada uma única vez. No primeiro login a pessoa precisa trocá-la."),
    h("label", { for: "u-name" }, "Usuário"), name,
    h("label", { for: "u-email" }, "E-mail (recebe o código do segundo fator)"), email,
    h("label", { for: "u-role" }, "Papel"), role,
    groupsLabel, groupsBox,
    error,
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", type: "button", onclick: () => close() }, "Cancelar"),
      h("button", { class: "btn primary", type: "submit" }, user ? "Salvar" : "Criar usuário")),
  ));
  sync();
}

function temporaryPasswordModal(username, password) {
  const pre = h("pre", { class: "cmd", tabindex: "0" }, password);
  const copyBtn = h("button", { class: "btn small" }, "Copiar");
  copyBtn.addEventListener("click", () => copy(password, copyBtn));
  const close = modal(
    h("h2", null, `Senha temporária de “${username}”`),
    h("div", { class: "warn" }, "Esta senha é exibida só desta vez. Entregue-a à pessoa por um canal seguro; ela será obrigada a trocá-la no primeiro login."),
    pre,
    h("div", { class: "row" }, copyBtn),
    h("div", { class: "modal-actions" }, h("button", { class: "btn primary", onclick: () => close() }, "Concluir")),
  );
}

async function resetUser(u) {
  if (!confirm(`Gerar nova senha temporária para “${u.username}”? As sessões abertas dele serão encerradas.`)) return;
  try {
    const out = await api("POST", `/api/users/${u.id}/reset`);
    temporaryPasswordModal(u.username, out.temporary_password);
  } catch (e) { fail(e); }
}

async function reset2FA(u, reload) {
  if (!confirm(`Remover a verificação em duas etapas de “${u.username}”? As sessões abertas dele serão encerradas e ele poderá entrar só com a senha.`)) return;
  try { await api("POST", `/api/users/${u.id}/reset-2fa`); reload(); toast("2FA removido", "ok"); }
  catch (e) { fail(e); }
}

async function deleteUser(u, reload) {
  if (!confirm(`Excluir o usuário “${u.username}”?`)) return;
  try { await api("DELETE", `/api/users/${u.id}`); reload(); toast("Usuário excluído", "ok"); }
  catch (e) { fail(e); }
}

// --- audit ---
function auditView() {
  const body = h("div");
  const tab = (id, label) => h("button", { type: "button", role: "tab", class: state.auditTab === id ? "active" : "",
    onclick: () => { state.auditTab = id; render(); } }, label);
  const showRecordings = state.recording && state.auditTab === "recordings";
  body.append(showRecordings ? recordingsPanel() : eventsPanel());
  return h("div", { class: "content" }, h("main", { class: "main", style: "grid-column: 1 / -1" },
    h("div", { class: "main-head" }, h("h2", null, "Auditoria"),
      state.recording && h("div", { class: "tabs", role: "tablist", style: "margin:0" }, tab("events", "Eventos"), tab("recordings", "Sessões gravadas"))),
    body));
}

function eventsPanel() {
  const body = h("tbody");
  api("GET", "/api/audit?limit=200").then((entries) => {
    body.replaceChildren(...entries.map((e) => h("tr", null,
      h("td", { class: "mono" }, new Date(e.at).toLocaleString()), h("td", null, e.actor),
      h("td", null, h("span", { class: "tag" }, e.action)), h("td", { class: "mono" }, e.target), h("td", { class: "muted" }, e.detail))));
  }).catch(fail);
  return h("div", null, h("p", { class: "muted" }, "últimos 200 eventos"),
    h("table", { class: "audit" },
      h("thead", null, h("tr", null, ["Quando", "Quem", "Ação", "Alvo", "Detalhe"].map((x) => h("th", null, x)))),
      body));
}

// --- recorded sessions ---
function formatDuration(seconds) {
  const s = Math.max(0, Math.round(seconds));
  const m = Math.floor(s / 60), sec = String(s % 60).padStart(2, "0");
  return s >= 3600 ? `${Math.floor(m / 60)}:${String(m % 60).padStart(2, "0")}:${sec}` : `${m}:${sec}`;
}
function formatSize(bytes) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1048576) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / 1048576).toFixed(1)} MB`;
}

function recordingsPanel() {
  const body = h("tbody");
  const load = () => api("GET", "/api/recordings?limit=200").then((list) => {
    if (!list.length) {
      body.replaceChildren(h("tr", null, h("td", { colspan: "6", class: "muted" }, "Nenhuma sessão gravada ainda. Cada terminal aberto no painel passa a ser gravado.")));
      return;
    }
    body.replaceChildren(...list.map((r) => h("tr", null,
      h("td", { class: "mono" }, new Date(r.started_at).toLocaleString()),
      h("td", null, r.username),
      h("td", null, r.machine_name),
      h("td", null, r.duration_seconds == null ? h("span", { class: "tag" }, "gravando") : formatDuration(r.duration_seconds),
        r.truncated && h("span", { class: "tag warn", title: "A gravação passou do limite de tamanho e foi cortada" }, "cortada")),
      h("td", { class: "muted" }, formatSize(r.bytes)),
      h("td", null, h("div", { class: "actions" },
        h("button", { class: "btn small primary", onclick: () => playRecording(r) }, "Assistir"),
        h("a", { class: "btn small", href: `/api/recordings/${r.id}/cast`, download: `${r.machine_name}-${r.id.slice(0, 8)}.cast`, title: "Baixa no formato asciicast (asciinema)" }, "Baixar"),
        h("button", { class: "btn small danger", onclick: () => deleteRecording(r, load) }, "Excluir"))))));
  }).catch(fail);
  load();
  return h("div", null,
    h("p", { class: "muted" }, "Só a saída do terminal é gravada, nunca o que a pessoa digita (senhas digitadas sem eco não aparecem). Quem assiste ou apaga fica registrado em Eventos."),
    h("table", { class: "audit" },
      h("thead", null, h("tr", null, ["Início", "Quem", "Máquina", "Duração", "Tamanho", ""].map((x) => h("th", null, x)))),
      body));
}

async function deleteRecording(r, reload) {
  if (!confirm(`Apagar a gravação de ${r.username} em ${r.machine_name}? Não dá para desfazer.`)) return;
  try { await api("DELETE", `/api/recordings/${r.id}`); reload(); toast("Gravação apagada", "ok"); }
  catch (e) { fail(e); }
}

// playRecording replays an asciicast in a read-only xterm.js. Pauses longer
// than IDLE_LIMIT seconds are shortened so long idle stretches do not stall the replay.
const IDLE_LIMIT = 2;
async function playRecording(rec) {
  let text;
  try {
    const resp = await fetch(`/api/recordings/${rec.id}/cast`, { credentials: "same-origin" });
    if (!resp.ok) throw new Error(((await resp.json().catch(() => null)) || {}).error || resp.statusText);
    text = await resp.text();
  } catch (e) { fail(e); return; }
  const lines = text.split("\n").filter(Boolean);
  let header;
  try { header = JSON.parse(lines[0]); } catch (_) { toast("Arquivo de gravação inválido.", "err"); return; }
  const events = [];
  let previous = 0, clock = 0;
  for (const line of lines.slice(1)) {
    try {
      const [t, kind, data] = JSON.parse(line);
      if (kind !== "o" && kind !== "r") continue;
      clock += Math.min(Math.max(t - previous, 0), IDLE_LIMIT);
      previous = t;
      events.push({ at: clock, kind, data });
    } catch (_) { /* skip a damaged line */ }
  }
  const total = events.length ? events[events.length - 1].at : 0;

  const host = h("div", { class: "term" });
  const playBtn = h("button", { class: "btn small primary", style: "min-width:84px", title: "Pausar / continuar (espaço)" }, "Pausar");
  const ctl = (label, title, fn) => h("button", { class: "btn small", title, "aria-label": /[A-Za-zÀ-ú]{3,}/.test(label) ? false : title, onclick: (e) => { fn(); if (e.detail > 0) e.currentTarget.blur(); } }, label); // mouse clicks give the focus back so Space keeps toggling playback; icon-only buttons need an accessible name
  const slider = h("input", { type: "range", min: "0", max: String(Math.round(total * 1000)), value: "0", class: "player-slider", "aria-label": "Posição" });
  const label = h("span", { class: "mono muted" }, `0:00 / ${formatDuration(total)}`);
  const speed = h("select", { class: "pick", "aria-label": "Velocidade" },
    ...[0.5, 1, 2, 4, 8].map((v) => h("option", { value: String(v), selected: v === 1 }, v + "x")));
  const overlay = h("div", { class: "player-overlay" },
    h("div", { class: "term-bar" },
      h("span", { class: "brand" }, "Gravação"),
      h("span", { class: "muted" }, `${rec.username} em ${rec.machine_name}, ${new Date(rec.started_at).toLocaleString()}`),
      h("span", { class: "spacer" }),
      h("button", { class: "btn small", onclick: () => end() }, "Fechar")),
    h("div", { class: "player-host" }, host),
    h("div", { class: "player-controls" },
      ctl("⏹", "Parar e voltar ao início (Home)", () => { pause(); seek(0); playBtn.textContent = "Reproduzir"; }),
      ctl("⏪ 10s", "Voltar 10 segundos (Shift+←)", () => jump(-10)),
      ctl("◀", "Evento anterior: volta um passo da gravação, tecla por tecla (,)", () => step(-1)),
      playBtn,
      ctl("▶", "Próximo evento: avança um passo da gravação, tecla por tecla (.)", () => step(1)),
      ctl("10s ⏩", "Avançar 10 segundos (Shift+→)", () => jump(10)),
      slider, label, speed,
      ctl("Copiar seleção", "Copia o texto selecionado (ou a tela atual, se nada estiver selecionado). Ctrl+C também copia a seleção", () => copyScreen()),
      ctl("Copiar tudo", "Copia toda a saída da sessão como texto", () => copyAll())));
  document.body.append(overlay);

  await loadTerminalFonts();
  const term = new Terminal({
    cols: header.width || 80, rows: header.height || 24, disableStdin: true, cursorBlink: false, scrollback: 5000,
    fontFamily: termFontFamily(), fontSize: termFontSize(),
    theme: { background: "#1a1c2c", foreground: "#f4f4f4", cursor: "#ffcd75" },
  });
  term.open(host);
  term.attachCustomKeyEventHandler(terminalKeyHandler(term)); // Ctrl+C copies the selection

  let pos = 0, now = 0, timer = null, playing = false;
  const apply = (ev) => {
    if (ev.kind === "o") { term.write(ev.data); return; }
    const [c, r] = String(ev.data).split("x").map(Number);
    if (c > 0 && r > 0) term.resize(c, r);
  };
  const show = () => {
    slider.value = String(Math.round(now * 1000));
    label.textContent = `${formatDuration(now)} / ${formatDuration(total)}`;
  };
  function stopTimer() { clearTimeout(timer); timer = null; }
  function tick() {
    stopTimer();
    if (!playing) return;
    if (pos >= events.length) { playing = false; playBtn.textContent = "Reiniciar"; return; }
    const wait = Math.max(0, (events[pos].at - now) / Number(speed.value) * 1000);
    timer = setTimeout(() => {
      now = events[pos].at;
      // events within one frame go out together
      while (pos < events.length && events[pos].at - now < 0.016) apply(events[pos++]);
      show();
      tick();
    }, wait);
  }
  function play() { playing = true; playBtn.textContent = "Pausar"; if (pos >= events.length) seek(0); tick(); }
  function pause() { playing = false; stopTimer(); playBtn.textContent = "Continuar"; }
  // seek rebuilds the screen up to the given effective time.
  function seek(seconds) {
    stopTimer();
    term.reset();
    term.resize(header.width || 80, header.height || 24);
    let out = "", i = 0;
    for (; i < events.length && events[i].at <= seconds; i++) {
      if (events[i].kind === "o") out += events[i].data;
      else { if (out) { term.write(out); out = ""; } apply(events[i]); }
    }
    if (out) term.write(out);
    pos = i;
    now = seconds;
    show();
    if (playing) tick();
  }
  // jump moves by seconds of (shortened) replay time; step moves to the previous/next event.
  function jump(delta) { seek(Math.min(total, Math.max(0, now + delta))); }
  function step(direction) {
    if (playing) pause();
    if (direction > 0) {
      if (pos < events.length) seek(events[pos].at);
      return;
    }
    const current = pos > 0 ? events[pos - 1].at : 0;
    let i = pos - 1;
    while (i >= 0 && events[i].at >= current) i--;
    seek(i >= 0 ? events[i].at : 0);
  }
  // screenText reads a terminal buffer as plain text, joining wrapped lines.
  const bufferText = (t, last) => {
    const b = t.buffer.active;
    const out = [];
    for (let y = 0; y < Math.min(b.length, last); y++) {
      const line = b.getLine(y);
      if (!line) continue;
      const text = line.translateToString(true);
      if (line.isWrapped && out.length) out[out.length - 1] += text; else out.push(text);
    }
    while (out.length && !out[out.length - 1].trim()) out.pop();
    return out.join("\n");
  };
  async function copyScreen() {
    const text = term.hasSelection() ? term.getSelection() : bufferText(term, Infinity);
    const ok = text && await writeClipboard(text);
    toast(ok ? (term.hasSelection() ? "Seleção copiada" : "Tela copiada") : "Não consegui copiar; selecione o texto e use Ctrl+C.", ok ? "ok" : "err");
  }
  // copyAll replays the whole session into a throwaway terminal and reads its buffer.
  async function copyAll() {
    const scratch = new Terminal({ cols: header.width || 80, rows: header.height || 24, scrollback: 100000, allowProposedApi: true });
    const write = (data) => new Promise((resolve) => scratch.write(data, resolve));
    let out = "";
    for (const ev of events) {
      if (ev.kind === "o") out += ev.data;
      else { if (out) { await write(out); out = ""; } const [c, r] = String(ev.data).split("x").map(Number); if (c > 0 && r > 0) scratch.resize(c, r); }
    }
    if (out) await write(out);
    const text = bufferText(scratch, Infinity);
    scratch.dispose();
    const ok = text && await writeClipboard(text);
    toast(ok ? "Saída completa copiada" : "Não consegui copiar.", ok ? "ok" : "err");
  }
  playBtn.addEventListener("click", (e) => { if (playing) pause(); else play(); if (e.detail > 0) playBtn.blur(); });
  slider.addEventListener("input", () => seek(Number(slider.value) / 1000));
  speed.addEventListener("change", () => { if (playing) tick(); });
  const onKey = (e) => {
    if (document.querySelector(".overlay")) return;
    if (e.key === "Escape") { end(); return; }
    const t = e.target.tagName;
    if (t === "INPUT" || t === "SELECT" || t === "TEXTAREA") return; // slider and speed keep their own keys
    if (e.key === " " && t !== "BUTTON") { e.preventDefault(); playBtn.click(); }
    else if (e.key === "ArrowLeft") { e.preventDefault(); jump(e.shiftKey ? -10 : -5); }
    else if (e.key === "ArrowRight") { e.preventDefault(); jump(e.shiftKey ? 10 : 5); }
    else if (e.key === ",") step(-1);
    else if (e.key === ".") step(1);
    else if (e.key === "Home") { e.preventDefault(); pause(); seek(0); playBtn.textContent = "Reproduzir"; }
  };
  document.addEventListener("keydown", onKey);
  function end() {
    stopTimer();
    document.removeEventListener("keydown", onKey);
    term.dispose();
    overlay.remove();
  }
  play();
}

// ---------- boot ----------
(async function boot() {
  try {
    applyMe(await api("GET", "/api/me"));
    if (sessionReady()) { await refresh(); startPolling(); }
  } catch (_) { state.user = null; }
  render();
})();
