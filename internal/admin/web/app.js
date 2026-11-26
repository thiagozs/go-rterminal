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
    if (!state.user || state.view !== "machines" || document.querySelector(".overlay, .term-overlay")) return;
    try { await refresh(); render(); } catch (_) { /* surfaced on next action */ }
  }, 5000);
}
function stopPolling() { clearInterval(state.timer); state.timer = null; }

// ---------- views ----------
function render() {
  const app = document.getElementById("app");
  if (!state.user) app.replaceChildren(loginView());
  else if (state.mustChange) app.replaceChildren(forcedPasswordView());
  else app.replaceChildren(shell());
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

function machinesTable(list) {
  const rows = list.map((m) => h("tr", null,
    h("td", null, h("span", { class: "dot" + (m.online ? " on" : ""), title: m.online ? "online" : "offline" }),
      h("strong", null, m.name), h("div", { class: "mono muted" }, m.id)),
    h("td", null, m.group_id != null ? h("span", { class: "tag" }, groupName(m.group_id)) : h("span", { class: "muted" }, "—")),
    h("td", null, m.hostname ? `${m.hostname}` : "—", h("div", { class: "muted" }, [m.os, m.arch].filter(Boolean).join("/"))),
    h("td", { class: "muted" }, m.online ? "online" : timeAgo(m.last_seen)),
    h("td", null, h("div", { class: "actions" },
      h("button", { class: "btn small primary", disabled: !m.online, title: m.online ? "" : "Máquina offline", onclick: () => openTerminal(m) }, "Terminal"),
      h("button", { class: "btn small", onclick: () => machineModal(m) }, "Editar"),
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
  const tabs = [
    ["Binário", created.commands.binary, "Rode na máquina, com o binário `agent` já instalado:"],
    ["systemd", created.commands.systemd, "Instala o agent como serviço (Linux). Rode como usuário com sudo:"],
    ["Instalar o agent", created.commands.go_install, "Compila e instala o binário `agent` (precisa de Go 1.27+). Depois use uma das outras abas:"],
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
function openTerminal(machine) {
  const status = h("span", { class: "muted" }, "conectando…");
  const reconnect = h("button", { class: "btn small", hidden: true }, "Reconectar");
  const host = h("div", { id: "term" });
  const overlay = h("div", { class: "term-overlay" },
    h("div", { class: "term-bar" },
      h("span", { class: "brand" }, machine.name),
      status, h("span", { class: "spacer" }), reconnect,
      h("button", { class: "btn small", onclick: () => end() }, "Fechar")),
    h("div", { class: "term-host" }, host));
  document.body.append(overlay);

  const term = new Terminal({
    cursorBlink: true, fontFamily: "ui-monospace, Menlo, Consolas, monospace", fontSize: 14,
    theme: { background: "#1a1c2c", foreground: "#f4f4f4", cursor: "#ffcd75" },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  term.open(host);
  fit.fit();
  term.focus();

  const enc = new TextEncoder();
  let ws = null;
  const sendResize = () => {
    if (ws && ws.readyState === WebSocket.OPEN) ws.send(JSON.stringify({ type: "resize", rows: term.rows, cols: term.cols }));
  };
  term.onData((d) => { if (ws && ws.readyState === WebSocket.OPEN) ws.send(enc.encode(d)); });
  term.onResize(sendResize);
  const onWindowResize = () => fit.fit();
  window.addEventListener("resize", onWindowResize);

  function connect() {
    reconnect.hidden = true;
    status.textContent = "conectando…";
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}/ws/terminal?machine=${encodeURIComponent(machine.id)}`);
    ws.binaryType = "arraybuffer";
    ws.onmessage = (ev) => {
      if (typeof ev.data === "string") {
        let msg = null;
        try { msg = JSON.parse(ev.data); } catch (_) { return; }
        if (msg.type === "paired") { status.textContent = "conectado"; fit.fit(); sendResize(); }
        return;
      }
      term.write(new Uint8Array(ev.data));
    };
    ws.onclose = (ev) => {
      status.textContent = "desconectado";
      term.write(`\r\n\x1b[33m[sessão encerrada${ev.reason ? ": " + ev.reason : ""}]\x1b[0m\r\n`);
      reconnect.hidden = false;
    };
    ws.onerror = () => { status.textContent = "erro de conexão"; };
  }
  reconnect.addEventListener("click", () => { term.clear(); connect(); });
  const onEsc = (e) => { if (e.key === "Escape" && e.shiftKey) end(); };
  document.addEventListener("keydown", onEsc);

  function end() {
    window.removeEventListener("resize", onWindowResize);
    document.removeEventListener("keydown", onEsc);
    if (ws) { ws.onclose = null; ws.close(); }
    term.dispose();
    overlay.remove();
    refresh().then(render).catch(() => {});
  }
  connect();
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
    if (!state.mustChange) { await refresh(); startPolling(); }
  } catch (_) { state.user = null; }
  render();
})();
