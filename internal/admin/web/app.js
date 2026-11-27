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
  fileTransfer: false, // server allows browsing, sending and downloading files
  auditTab: "events", // events | recordings
  view: "machines", // machines | users | live | audit
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
    const err = new APIError(resp.status, (data && data.error) || resp.statusText);
    err.data = data;
    throw err;
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
  state.fileTransfer = !!me.file_transfer;
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
      h("nav", { class: "nav" }, navBtn("machines", "Máquinas"), isAdmin() && navBtn("users", "Usuários"), isAdmin() && navBtn("live", "Ao vivo"), isAdmin() && navBtn("audit", "Auditoria")),
      h("span", { class: "spacer" }),
      workspace && h("button", { class: "btn small primary", title: "Voltar aos terminais abertos", onclick: () => workspace.show() }, `Terminais (${workspace.count()})`),
      h("span", { class: "muted" }, state.user),
      h("button", { class: "btn small", title: "Preferências deste navegador", onclick: () => preferencesModal() }, "Preferências"),
      state.twoFAAvailable && h("button", { class: "btn small", title: "Verificação em duas etapas", onclick: () => securityModal() }, "Segurança"),
      h("button", { class: "btn small", onclick: () => passwordModal() }, "Alterar senha"),
      h("button", { class: "btn small", onclick: logout }, "Sair"),
    ),
    state.view === "users" && isAdmin() ? usersView() : state.view === "live" && isAdmin() ? liveView() : state.view === "audit" && isAdmin() ? auditView() : machinesView(),
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
  return h("div", { class: "muted version", title: `agent ${m.version}` },
    "agent ", h("span", { class: "mono nowrap" }, shortVersion(m.version)),
    isOutdated(m) && h("span", { class: "tag warn", title: `O server está em ${state.serverVersion}.` }, "desatualizado"));
}

// updateNow asks the agent to update itself. Terminals open on the machine end
// when it restarts, so the server asks for confirmation first.
async function updateNow(m, force) {
  try {
    await api("POST", `/api/machines/${encodeURIComponent(m.id)}/update`, force ? { force: true } : {});
    toast(`Atualização pedida a ${m.name}. Ela reinicia em instantes; acompanhe em Histórico.`, "ok");
    setTimeout(() => refresh().then(render).catch(() => {}), 15000);
  } catch (e) {
    if (e.status === 409 && e.data && e.data.sessions) {
      if (confirm(`${m.name} tem ${e.data.sessions} terminal(is) aberto(s). Eles serão encerrados quando o agent reiniciar. Atualizar mesmo assim?`)) return updateNow(m, true);
      return;
    }
    fail(e);
  }
}

// updateModal shows the command that downloads the current agent and restarts
// the service. It reads the credential already stored on the machine, so no
// secret appears here.
function updateModal(m) {
  const base = location.origin;
  const unit = "/etc/systemd/system/rterminal-agent.service";
  const script = [
    "# Baixa o agent atual deste server e reinicia o servico (Linux, precisa de sudo e curl)",
    "TOKEN=$(sudo sed -n 's/^RTERMINAL_TOKEN=//p' /etc/rterminal-agent.env)",
    "# O binario e o que o servico executa (pode nao estar em /usr/local/bin)",
    `BIN=$(sed -n 's/^ExecStart=\\([^ ]*\\).*/\\1/p' ${unit}); BIN=\${BIN:-/usr/local/bin/agent}`,
    'case "$(uname -m)" in',
    "  x86_64) ARCH=amd64 ;;",
    "  aarch64|arm64) ARCH=arm64 ;;",
    "  armv7l|armv6l) ARCH=arm ;;",
    '  *) echo "arquitetura nao suportada: $(uname -m)" >&2; exit 1 ;;',
    "esac",
    `printf 'Authorization: Bearer %s\\n' "$TOKEN" | curl -fsSL -H @- "${base}/download/agent/linux-$ARCH" -o /tmp/rterminal-agent`,
    'sudo install -m 755 /tmp/rterminal-agent "$BIN.new" && sudo mv -f "$BIN.new" "$BIN" && rm -f /tmp/rterminal-agent',
    "sudo systemctl restart rterminal-agent",
    "unset TOKEN ARCH BIN",
  ].join("\n");
  // For services that run as a normal user (User= in the unit): run the
  // service as root and open terminals and files as that user instead.
  const autoScript = [
    "# Passa o servico a rodar como root e abrir terminais e arquivos como o usuario que ele usa hoje",
    `UNIT=${unit}`,
    "SVC_USER=$(systemctl show -p User --value rterminal-agent)",
    `BIN=$(sed -n 's/^ExecStart=\\([^ ]*\\).*/\\1/p' $UNIT)`,
    'if [ -z "$SVC_USER" ] || [ "$SVC_USER" = root ]; then',
    '  echo "O servico ja roda como root. Para abrir o terminal como outro usuario, acrescente --shell-user NOME ao ExecStart de $UNIT."',
    'elif ! "$BIN" --help 2>&1 | grep -q -- --shell-user; then',
    '  echo "Este agent ainda nao tem --shell-user: rode antes o comando de atualizacao acima."',
    "else",
    `  sudo sed -i -e '/^User=/d' -e "s#^\\(ExecStart=.*\\)#\\1 --shell-user $SVC_USER#" $UNIT &&`,
    "  sudo systemctl daemon-reload && sudo systemctl restart rterminal-agent &&",
    '  echo "Pronto: o agent roda como root e abre terminais e arquivos como $SVC_USER."',
    "fi",
    "unset UNIT SVC_USER BIN",
  ].join("\n");
  const copyBtn = h("button", { class: "btn small" }, "Copiar");
  copyBtn.addEventListener("click", () => copy(script, copyBtn));
  const autoBtn = h("button", { class: "btn small" }, "Copiar");
  autoBtn.addEventListener("click", () => copy(autoScript, autoBtn));
  const close = modal(
    h("h2", null, `Atualizar o agent de “${m.name}”`),
    h("p", { class: "muted" }, m.version
      ? `Agent atual: ${m.version}. Server: ${state.serverVersion || "—"}.`
      : "Esta máquina ainda não informou a versão do agent (provavelmente é um agent antigo)."),
    h("p", { class: "muted" }, "Rode na máquina. Serve para serviços instalados pelo comando do painel (systemd, credencial em /etc/rterminal-agent.env). A credencial não muda."),
    h("pre", { class: "cmd", tabindex: "0" }, script),
    h("div", { class: "row" }, copyBtn),
    h("h3", { style: "margin-top:18px" }, "Serviço rodando como usuário comum (User= na unit)"),
    h("p", { class: "muted" }, "Com User= o agent não tem permissão para trocar o próprio binário e o botão “Atualizar agora” não aparece. Depois do comando acima, rode este: ele tira o User= e passa o serviço a rodar como root com --shell-user, então cada terminal e cada transferência de arquivos continua abrindo como o seu usuário (mesmo shell, mesmo HOME, mesmas permissões) e o agent pode se atualizar. Se o serviço já roda como root, não faz nada."),
    h("pre", { class: "cmd", tabindex: "0" }, autoScript),
    h("div", { class: "row" }, autoBtn),
    h("div", { class: "modal-actions" }, h("button", { class: "btn primary", onclick: () => close() }, "Concluir")),
  );
}

// openMenu shows a small popup menu next to anchor. items: {label, onclick,
// title, danger} or "-" for a separator. It closes on a click elsewhere,
// Escape, scrolling or resizing.
function openMenu(anchor, items) {
  document.querySelectorAll(".menu").forEach((m) => m.remove());
  const menu = h("div", { class: "menu", role: "menu" },
    ...items.filter(Boolean).map((it) => it === "-" ? h("div", { class: "menu-sep", role: "separator" })
      : h("button", { class: "menu-item" + (it.danger ? " danger" : ""), role: "menuitem", title: it.title || "", type: "button",
          onclick: () => { close(); it.onclick(); } }, it.label)));
  document.body.append(menu);
  const rect = anchor.getBoundingClientRect();
  const width = menu.offsetWidth, height = menu.offsetHeight;
  menu.style.left = `${Math.max(8, Math.min(rect.right - width, window.innerWidth - width - 8))}px`;
  menu.style.top = `${rect.bottom + 4 + height > window.innerHeight ? Math.max(8, rect.top - height - 4) : rect.bottom + 4}px`;
  anchor.setAttribute("aria-expanded", "true");
  const onDown = (e) => { if (!menu.contains(e.target) && e.target !== anchor) close(); };
  const onKey = (e) => { if (e.key === "Escape") { close(); anchor.focus(); } };
  function close() {
    menu.remove();
    anchor.setAttribute("aria-expanded", "false");
    document.removeEventListener("mousedown", onDown, true);
    document.removeEventListener("keydown", onKey, true);
    window.removeEventListener("resize", close);
    window.removeEventListener("scroll", close, true);
  }
  document.addEventListener("mousedown", onDown, true);
  document.addEventListener("keydown", onKey, true);
  window.addEventListener("resize", close);
  window.addEventListener("scroll", close, true);
  const first = menu.querySelector(".menu-item");
  if (first) first.focus();
}

// shortVersion drops the "server-" prefix so a build fits on one line.
const shortVersion = (v) => v.replace(/^server-/, "");

function machinesTable(list) {
  const rows = list.map((m) => {
    const outdated = isOutdated(m);
    return h("tr", null,
      h("td", { class: "col-name" }, h("span", { class: "dot" + (m.online ? " on" : ""), title: m.online ? "online" : "offline" }),
        h("strong", null, m.name), h("div", { class: "mono muted" }, m.id),
        !m.online && h("div", { class: "muted small-note" }, "visto " + timeAgo(m.last_seen))),
      h("td", { class: "col-group" }, m.group_id != null ? h("span", { class: "tag" }, groupName(m.group_id)) : h("span", { class: "muted" }, "—")),
      h("td", { class: "col-host" }, m.hostname ? `${m.hostname}` : "—", h("div", { class: "muted" }, [m.os, m.arch].filter(Boolean).join("/")),
        versionBadge(m),
        m.shell_user && h("div", { class: "muted small-note", title: "Terminais e arquivos abrem como este usuário" }, "terminal como ", h("span", { class: "mono" }, m.shell_user)),
        isAdmin() && m.can_self_update && outdated && h("button", { class: "btn small primary update-now", title: `Atualiza o agent de ${m.version} para ${state.serverVersion} sem você colar nada: ele baixa a versão nova do servidor, confere o checksum, reinicia e volta sozinho. Se não reconectar em 2 minutos, volta para a versão anterior.`, onclick: () => updateNow(m) }, "Atualizar agora"),
        isAdmin() && outdated && !m.can_self_update && state.downloads && m.os === "linux" && h("button", { class: "btn small primary update-now", title: "Este agent não consegue se atualizar sozinho (o motivo está no Histórico, no evento de início do agent). Use o comando manual, que roda com sudo na máquina.", onclick: () => updateModal(m) }, "Atualizar agent")),
      h("td", { class: "col-actions" }, h("div", { class: "actions" },
        h("button", { class: "btn small primary", disabled: !m.online, title: m.online ? "" : "Máquina offline", onclick: () => openTerminal(m) }, "Terminal"),
        state.fileTransfer && h("button", { class: "btn small", disabled: !m.can_files,
          title: !m.online ? "Máquina offline" : m.can_files ? "Navegar, enviar e baixar arquivos" : "Este agent é antigo e não faz transferência de arquivos: atualize-o", onclick: () => openFiles(m) }, "Arquivos"),
        isAdmin() && h("button", { class: "btn small", title: "Quedas, suspensões e reconexões desta máquina", onclick: () => historyModal(m) }, "Histórico"),
        isAdmin() && h("button", { class: "btn small icon", "aria-label": `Mais ações para ${m.name}`, "aria-haspopup": "menu", "aria-expanded": "false", title: "Mais ações",
          onclick: (e) => openMenu(e.currentTarget, [
            { label: "Editar", onclick: () => machineModal(m) },
            state.downloads && m.os === "linux" && { label: "Atualizar agent (comando manual)", title: "Comando para atualizar o agent desta máquina, sem trocar a credencial", onclick: () => updateModal(m) },
            { label: "Nova credencial", title: "Gera uma nova credencial e desconecta o agent atual", onclick: () => rotate(m) },
            "-",
            { label: "Excluir máquina", danger: true, onclick: () => removeMachine(m) },
          ]) }, "⋯"),
      )));
  });
  return h("table", { class: "machines" },
    h("thead", null, h("tr", null, ["Máquina", "Grupo", "Host", ""].map((t) => h("th", null, t)))),
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

// historyModal shows why a machine went offline: what its agent noticed
// (suspension, lost network, ...) next to what the server observed.
const EVENT_LABEL = {
  agent_start: ["Agent iniciou", ""], agent_stop: ["Agent parou", ""],
  connected: ["Conectou", "ok"], online: ["Online", "ok"],
  lost: ["Conexão perdida", "warn"], offline: ["Offline", "warn"],
  connect_failed: ["Falha ao conectar", "warn"], suspend: ["Máquina suspensa", "warn"],
  update_started: ["Atualização iniciada", ""], update_ok: ["Atualizado", "ok"],
  update_failed: ["Atualização falhou", "warn"], update_rollback: ["Atualização desfeita", "warn"],
};
function historyModal(machine) {
  const body = h("tbody");
  const load = () => api("GET", `/api/machines/${encodeURIComponent(machine.id)}/events?limit=300`).then((events) => {
    if (!events.length) {
      body.replaceChildren(h("tr", null, h("td", { colspan: "4", class: "muted" }, "Nenhum evento registrado ainda. Agents antigos não enviam histórico: atualize o agent desta máquina.")));
      return;
    }
    body.replaceChildren(...events.map((e) => {
      const [label, kind] = EVENT_LABEL[e.kind] || [e.kind, ""];
      return h("tr", null,
        h("td", { class: "mono", style: "white-space:nowrap" }, new Date(e.at).toLocaleString()),
        h("td", { class: "muted" }, e.source === "agent" ? "agent" : "servidor"),
        h("td", null, h("span", { class: "tag " + kind }, label)),
        h("td", { class: "muted" }, e.detail));
    }));
  }).catch(fail);
  load();
  const close = modal(
    h("h2", null, `Histórico de ${machine.name}`),
    h("p", { class: "muted" }, "O agent anota sozinho o que acontece com a conexão (suspensão, rede, reinício) e envia ao reconectar; o servidor anota quando viu a máquina ficar online ou offline. Guardado por 30 dias, no máximo 1000 eventos por máquina."),
    h("div", { class: "history" }, h("table", { class: "audit" },
      h("thead", null, h("tr", null, ["Quando", "Origem", "Evento", "Detalhe"].map((x) => h("th", null, x)))),
      body)),
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", onclick: load }, "Atualizar"),
      h("button", { class: "btn primary", onclick: () => close() }, "Fechar")));
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
  const shellUser = h("input", { id: "m-shell-user", value: machine ? machine.shell_user || "" : "", maxlength: "32", placeholder: "ex: thiago (vazio = como o próprio agent)", autocomplete: "off", spellcheck: "false", pattern: "[a-z_][a-z0-9_\\-]{0,31}", title: "Nome de login: minúsculas, números, _ e -" });
  const error = h("p", { class: "error", role: "alert" });
  const close = modal(h("form", { onsubmit: async (e) => {
    e.preventDefault();
    const body = { name: name.value, group_id: group.value === "" ? null : Number(group.value), shell_user: shellUser.value.trim() };
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
    h("label", { for: "m-shell-user" }, "Usuário do terminal (opcional)"), shellUser,
    h("p", { class: "muted small-note" }, "O serviço do agent roda como root e abre cada terminal e cada transferência de arquivos como este usuário da máquina, com o shell, o HOME e as permissões dele. Isso também deixa o agent atualizar a si mesmo. Vale para os comandos de instalação gerados; em uma máquina já instalada, use o comando de “Atualizar agent”."),
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

// --- live sessions (spectator) ---
function liveView() {
  const body = h("tbody");
  const load = () => api("GET", "/api/sessions").then((list) => {
    if (!list.length) {
      body.replaceChildren(h("tr", null, h("td", { colspan: "5", class: "muted" }, "Nenhum terminal aberto agora.")));
      return;
    }
    body.replaceChildren(...list.map((s) => h("tr", null,
      h("td", null, s.username),
      h("td", null, s.machine_name),
      h("td", { class: "muted" }, timeAgo(s.started_at)),
      h("td", { class: "muted" }, s.watchers ? `${s.watchers} assistindo` : "—"),
      h("td", null, h("div", { class: "actions" }, h("button", { class: "btn small primary", onclick: () => watchSession(s) }, "Assistir"))))));
  }).catch(fail);
  load();
  // The list refreshes itself until the user leaves this screen.
  const timer = setInterval(() => { if (!body.isConnected) clearInterval(timer); else load(); }, 4000);
  return h("div", { class: "content" }, h("main", { class: "main", style: "grid-column: 1 / -1" },
    h("div", { class: "main-head" }, h("h2", null, "Terminais ao vivo"), h("span", { class: "spacer" }),
      h("button", { class: "btn small", onclick: load }, "Atualizar")),
    h("p", { class: "muted" }, "Assista, só leitura, a um terminal aberto agora. Você vê o que o terminal mostra, nunca o que a pessoa digita, e não consegue digitar nele. Cada visualização fica registrada em Auditoria."),
    h("table", { class: "audit" },
      h("thead", null, h("tr", null, ["Quem", "Máquina", "Aberto", "", ""].map((x) => h("th", null, x)))),
      body)));
}

// watchSession shows a running terminal, read-only, in a full-screen overlay.
async function watchSession(info) {
  const host = h("div", { class: "term" });
  const status = h("span", { class: "tag ok" }, "conectando…");
  const overlay = h("div", { class: "player-overlay" },
    h("div", { class: "term-bar" },
      h("span", { class: "brand" }, "Ao vivo"),
      h("span", { class: "muted" }, `${info.username} em ${info.machine_name}`),
      status,
      h("span", { class: "muted" }, "somente leitura"),
      h("span", { class: "spacer" }),
      h("button", { class: "btn small", onclick: () => close() }, "Fechar")),
    h("div", { class: "player-host" }, host));
  document.body.append(overlay);

  await loadTerminalFonts();
  const term = new Terminal({
    cols: 80, rows: 24, disableStdin: true, cursorBlink: false, scrollback: 5000,
    fontFamily: termFontFamily(), fontSize: termFontSize(),
    theme: { background: "#1a1c2c", foreground: "#f4f4f4", cursor: "#ffcd75" },
  });
  term.open(host);
  term.attachCustomKeyEventHandler(terminalKeyHandler(term)); // Ctrl+C copies the selection

  let ws = null, closed = false;
  const finish = (text, kind) => {
    status.textContent = text;
    status.className = "tag " + (kind || "");
  };
  const proto = location.protocol === "https:" ? "wss:" : "ws:";
  ws = new WebSocket(`${proto}//${location.host}/ws/watch?session=${encodeURIComponent(info.id)}`);
  ws.binaryType = "arraybuffer";
  ws.onmessage = (ev) => {
    if (typeof ev.data !== "string") { term.write(new Uint8Array(ev.data)); return; }
    let msg = null;
    try { msg = JSON.parse(ev.data); } catch (_) { return; }
    if (msg.type === "info") finish("ao vivo", "ok");
    else if (msg.type === "resize" && msg.cols > 0 && msg.rows > 0) term.resize(msg.cols, msg.rows);
    else if (msg.type === "end") {
      finish(msg.reason === "session ended" ? "sessão encerrada" : msg.reason, "warn");
      term.write("\r\n\x1b[33m[sessão encerrada]\x1b[0m\r\n");
    }
  };
  ws.onclose = () => { if (!closed && status.textContent !== "sessão encerrada") finish("desconectado", "warn"); };
  ws.onerror = () => finish("erro de conexão", "warn");

  const onKey = (e) => { if (e.key === "Escape") close(); };
  document.addEventListener("keydown", onKey);
  function close() {
    closed = true;
    document.removeEventListener("keydown", onKey);
    if (ws) ws.close();
    term.dispose();
    overlay.remove();
  }
}

// --- preferences (kept in this browser) ---
const PREFS_KEY = "rt.prefs";
const PREF_DEFAULTS = {
  autoCloseTransfers: true, // hide the transfer panel by itself once everything succeeded
  autoCloseSeconds: 5,      // ...after this many seconds
};
const CLOSE_CHOICES = [3, 5, 10, 30];

function prefs() {
  let saved = {};
  try { saved = JSON.parse(stored(PREFS_KEY) || "{}") || {}; } catch (_) { /* damaged: use the defaults */ }
  const out = { ...PREF_DEFAULTS };
  if (typeof saved.autoCloseTransfers === "boolean") out.autoCloseTransfers = saved.autoCloseTransfers;
  const secs = Number(saved.autoCloseSeconds);
  if (Number.isFinite(secs) && secs >= 1 && secs <= 60) out.autoCloseSeconds = Math.round(secs);
  return out;
}
function savePrefs(patch) { store(PREFS_KEY, JSON.stringify({ ...prefs(), ...patch })); }

function preferencesModal() {
  const p = prefs();
  const auto = h("input", { type: "checkbox", id: "pf-auto", checked: p.autoCloseTransfers });
  const choices = CLOSE_CHOICES.includes(p.autoCloseSeconds) ? CLOSE_CHOICES : [...CLOSE_CHOICES, p.autoCloseSeconds].sort((a, b) => a - b);
  const secs = h("select", { id: "pf-secs", disabled: !p.autoCloseTransfers },
    ...choices.map((v) => h("option", { value: String(v), selected: v === p.autoCloseSeconds }, `${v} segundos`)));
  auto.addEventListener("change", () => { secs.disabled = !auto.checked; });
  const close = modal(
    h("h2", null, "Preferências"),
    h("p", { class: "muted" }, "Valem para este navegador e ficam guardadas nele."),
    h("section", { class: "security" },
      h("h3", null, "Transferência de arquivos"),
      h("label", { class: "check", for: "pf-auto" }, auto, "Fechar sozinho o painel de envios quando terminar sem erros"),
      h("label", { for: "pf-secs" }, "Fechar depois de"), secs,
      h("p", { class: "muted small-note" }, "Com a opção desligada o painel fica aberto até você fechá-lo. Se algum arquivo falhar, ele nunca fecha sozinho.")),
    h("div", { class: "modal-actions" },
      h("button", { class: "btn", type: "button", onclick: () => close() }, "Cancelar"),
      h("button", { class: "btn primary", type: "button", onclick: () => {
        savePrefs({ autoCloseTransfers: auto.checked, autoCloseSeconds: Number(secs.value) });
        close(); toast("Preferências salvas", "ok");
      } }, "Salvar")));
}

// --- file transfer ---
const FILE_CHUNK = 64 * 1024;
const joinPath = (dir, name) => (dir === "/" ? "" : dir.replace(/\/+$/, "")) + "/" + name;
const parentPath = (p) => p.replace(/\/+$/, "").replace(/\/[^/]*$/, "") || "/";

// cleanRel keeps a path chosen in the browser relative and free of "." and "..".
const cleanRel = (rel) => String(rel).split("/").filter((s) => s && s !== "." && s !== "..").join("/");

// What the user picked or dropped, as { files: [{file, rel}], dirs: [rel] }.
// rel keeps the folder structure ("Photos/2026/a.jpg"); dirs lists folders,
// which is the only way an empty folder can be sent.
async function readAllEntries(reader) {
  const all = [];
  for (;;) {
    const batch = await new Promise((resolve, reject) => reader.readEntries(resolve, reject));
    if (!batch.length) return all; // readEntries returns a few at a time until it is empty
    all.push(...batch);
  }
}
async function walkEntry(entry, prefix, out) {
  if (entry.isFile) {
    const file = await new Promise((resolve, reject) => entry.file(resolve, reject));
    out.files.push({ file, rel: cleanRel(prefix + entry.name) });
  } else if (entry.isDirectory) {
    const rel = cleanRel(prefix + entry.name);
    if (rel) out.dirs.push(rel);
    for (const child of await readAllEntries(entry.createReader())) await walkEntry(child, rel + "/", out);
  }
}
async function collectDropped(dataTransfer) {
  const out = { files: [], dirs: [] };
  // webkitGetAsEntry only works while the drop event is being handled, so all
  // entries are taken before the first await.
  const entries = [];
  for (const item of dataTransfer.items || []) {
    const entry = item.kind === "file" && item.webkitGetAsEntry ? item.webkitGetAsEntry() : null;
    if (entry) entries.push(entry);
  }
  if (!entries.length) {
    for (const f of dataTransfer.files || []) out.files.push({ file: f, rel: cleanRel(f.name) });
    return out;
  }
  for (const entry of entries) await walkEntry(entry, "", out);
  out.files = out.files.filter((f) => f.rel);
  return out;
}
function collectPicked(fileList) {
  const out = { files: [], dirs: [] };
  for (const f of fileList) {
    const rel = cleanRel(f.webkitRelativePath || f.name);
    if (rel) out.files.push({ file: f, rel });
  }
  return out;
}
// emptyFolders lists the folders that need their own request: the ones that
// are not an ancestor of another folder or of a file (those get created on the way).
function emptyFolders(collected) {
  const ancestors = new Set();
  const mark = (rel) => {
    const parts = rel.split("/");
    for (let i = 1; i < parts.length; i++) ancestors.add(parts.slice(0, i).join("/"));
  };
  collected.files.forEach((f) => mark(f.rel));
  collected.dirs.forEach(mark);
  return [...new Set(collected.dirs.map(cleanRel).filter(Boolean))].filter((d) => !ancestors.has(d));
}

// chooseConflict asks what to do with a file that already exists.
function chooseConflict(path) {
  return new Promise((resolve) => {
    const finish = (value) => { overlay.remove(); document.removeEventListener("keydown", onKey, true); resolve(value); };
    const onKey = (e) => { if (e.key === "Escape") { e.stopPropagation(); finish("skip"); } };
    const btn = (label, value, cls) => h("button", { class: "btn" + (cls ? " " + cls : ""), type: "button", onclick: () => finish(value) }, label);
    const overlay = h("div", { class: "overlay" },
      h("div", { class: "modal", role: "dialog", "aria-modal": "true", style: "width:min(540px,100%)" },
        h("h2", null, "O arquivo já existe"),
        h("p", { class: "muted mono", style: "word-break:break-all" }, path),
        h("p", null, "Substituir o arquivo que está na máquina pelo que você está enviando?"),
        h("div", { class: "modal-actions", style: "flex-wrap:wrap" },
          btn("Pular", "skip"), btn("Pular todos", "skipAll"), btn("Substituir", "replace"), btn("Substituir todos", "replaceAll", "primary"))));
    document.addEventListener("keydown", onKey, true);
    document.body.append(overlay);
    overlay.querySelector("button").focus();
  });
}

// openFiles shows a file manager for one machine. Browsing, downloads and
// uploads travel over a WebSocket channel to the agent, one operation at a time.
function openFiles(machine) {
  let ws = null, cwd = "", closed = false;
  const inbox = [];
  let waiter = null, dead = null;
  const push = (f) => { if (waiter) { const w = waiter; waiter = null; w(f); } else inbox.push(f); };
  const next = () => inbox.length ? Promise.resolve(inbox.shift()) : dead ? Promise.resolve(dead) : new Promise((r) => { waiter = r; });

  // connect opens the channel and resolves once the agent has joined.
  function connect() {
    if (closed) return Promise.reject(new Error("tela de arquivos fechada"));
    if (ws && ws.readyState === WebSocket.OPEN) return Promise.resolve();
    inbox.length = 0; dead = null;
    const proto = location.protocol === "https:" ? "wss:" : "ws:";
    ws = new WebSocket(`${proto}//${location.host}/ws/files?machine=${encodeURIComponent(machine.id)}`);
    ws.binaryType = "arraybuffer";
    const mine = ws;
    return new Promise((resolve, reject) => {
      let paired = false;
      mine.onmessage = (ev) => {
        if (typeof ev.data !== "string") { push({ binary: new Uint8Array(ev.data) }); return; }
        let msg; try { msg = JSON.parse(ev.data); } catch (_) { return; }
        if (msg.type === "paired") { paired = true; resolve(); return; }
        push({ msg });
      };
      mine.onclose = (ev) => {
        if (ws !== mine) return; // a connection that was already replaced
        dead = { closed: true, reason: ev.reason || "conexão encerrada" };
        if (waiter) { const w = waiter; waiter = null; w(dead); }
        if (!paired) reject(new Error(ev.reason || "não foi possível abrir o canal de arquivos"));
      };
      mine.onerror = () => {};
    });
  }
  const sendOp = (m) => ws.send(JSON.stringify(m));
  const fail = (frame) => { throw new Error(frame.closed ? frame.reason : frame.msg && frame.msg.error ? frame.msg.error : "resposta inesperada"); };

  // Operations never overlap: each waits for the previous one.
  let chain = Promise.resolve();
  const serial = (fn) => { const run = chain.then(fn, fn); chain = run.catch(() => {}); return run; };

  // --- layout ---
  const pathInput = h("input", { class: "files-path", "aria-label": "Caminho", spellcheck: "false" });
  const body = h("tbody");
  const picker = h("input", { type: "file", multiple: true, hidden: true });
  const folderPicker = h("input", { type: "file", multiple: true, webkitdirectory: true, hidden: true });
  const drop = h("div", { class: "files-drop" }, h("table", { class: "files-table" },
    h("thead", null, h("tr", null, ["Nome", "Tamanho", "Modificado", ""].map((x) => h("th", null, x)))), body));
  const overlay = h("div", { class: "player-overlay files-overlay" },
    h("div", { class: "term-bar" },
      h("span", { class: "brand" }, "Arquivos"), h("span", { class: "muted" }, machine.name),
      h("span", { class: "spacer" }),
      h("button", { class: "btn small", onclick: () => close() }, "Fechar")),
    h("div", { class: "files-body" },
      h("div", { class: "files-toolbar" },
        h("button", { class: "btn small", title: "Pasta acima", "aria-label": "Pasta acima", onclick: () => go(parentPath(cwd)) }, "↑"),
        h("button", { class: "btn small", title: "Recarregar", "aria-label": "Recarregar", onclick: () => go(cwd) }, "⟳"),
        pathInput,
        h("button", { class: "btn small", onclick: () => makeFolder() }, "Nova pasta"),
        h("button", { class: "btn small", title: "Envia uma pasta inteira, com as subpastas e os arquivos, mantendo a estrutura", onclick: () => folderPicker.click() }, "Enviar pasta"),
        h("button", { class: "btn small primary", onclick: () => picker.click() }, "Enviar arquivos"),
        picker, folderPicker),
      h("p", { class: "muted files-hint" }, "Arraste arquivos ou pastas para cá para enviá-los à pasta aberta; as pastas chegam com todas as subpastas, como na origem. Os arquivos enviados pertencem ao usuário do terminal desta máquina. Cada envio e download fica registrado em Auditoria."),
      drop));
  document.body.append(overlay);

  // --- transfer panel (docked at the bottom) ---
  const jobs = [];           // everything of the current batch
  let cancelled = false, startedAt = 0, conflictPolicy = null, closeTimer = null, collapsed = false, renderQueued = false;
  const title = h("strong");
  const detail = h("span", { class: "muted xfer-detail" });
  const fill = h("div", { class: "bar-fill" });
  const list = h("div", { class: "xfer-list" });
  const countdown = h("span", { class: "muted small-note" });
  const toggleBtn = h("button", { class: "btn small", type: "button", "aria-label": "Mostrar ou esconder a lista", onclick: () => { collapsed = !collapsed; render(); } });
  const cancelBtn = h("button", { class: "btn small danger", type: "button", onclick: () => cancelAll() }, "Cancelar");
  const closePanelBtn = h("button", { class: "btn small", type: "button", onclick: () => resetPanel() }, "Fechar");
  const autoBox = h("input", { type: "checkbox", id: "xfer-auto" });
  autoBox.addEventListener("change", () => { savePrefs({ autoCloseTransfers: autoBox.checked }); maybeAutoClose(); });
  const panel = h("section", { class: "xfer-panel", hidden: true, "aria-live": "polite", "aria-label": "Transferências" },
    h("div", { class: "xfer-head" }, h("div", { class: "xfer-title" }, title, detail), h("span", { class: "spacer" }), toggleBtn, cancelBtn, closePanelBtn),
    h("div", { class: "bar xfer-total" }, fill),
    list,
    h("div", { class: "xfer-foot" },
      h("label", { class: "check", for: "xfer-auto" }, autoBox, "Fechar sozinho quando terminar"), countdown,
      h("button", { class: "linklike", type: "button", onclick: () => preferencesModal() }, "Preferências")));
  overlay.append(panel);

  const counts = () => {
    const c = { total: jobs.length, finished: 0, ok: 0, err: 0, skipped: 0, cancelled: 0, running: 0, queued: 0, bytes: 0, done: 0 };
    for (const j of jobs) {
      c[j.state]++;
      if (j.state !== "queued" && j.state !== "running") c.finished++;
      c.bytes += j.size; c.done += j.state === "ok" ? j.size : j.done;
    }
    c.active = c.queued + c.running > 0;
    return c;
  };
  const scheduleRender = () => { if (!renderQueued) { renderQueued = true; requestAnimationFrame(() => { renderQueued = false; render(); }); } };

  function render() {
    overlay.classList.toggle("has-panel", jobs.length > 0);
    if (!jobs.length) { panel.hidden = true; return; }
    panel.hidden = false;
    const c = counts();
    const uploading = jobs.some((j) => j.kind !== "down");
    const pct = c.bytes ? c.done / c.bytes : c.finished / c.total;
    if (c.active) {
      const secs = Math.max(0.5, (Date.now() - startedAt) / 1000);
      title.textContent = `${uploading ? "Enviando" : "Baixando"} ${Math.min(c.finished + 1, c.total)} de ${c.total}`;
      detail.textContent = ` · ${Math.round(pct * 100)}%` + (c.bytes ? ` · ${formatSize(c.done)} de ${formatSize(c.bytes)} · ${formatSize(c.done / secs)}/s` : "") + (c.queued > 0 ? ` · ${c.queued} na fila` : "");
    } else {
      title.textContent = c.err ? `Terminou com ${c.err} erro${c.err > 1 ? "s" : ""}` : cancelled ? "Cancelado" : c.total === 1 ? "Concluído" : `${c.ok + c.skipped} itens concluídos`;
      detail.textContent = " · " + [c.ok && `${c.ok} ok`, c.skipped && `${c.skipped} pulado${c.skipped > 1 ? "s" : ""}`, c.err && `${c.err} com erro`, c.cancelled && `${c.cancelled} cancelado${c.cancelled > 1 ? "s" : ""}`, c.bytes && formatSize(c.bytes)].filter(Boolean).join(" · ");
    }
    fill.style.width = `${Math.min(100, pct * 100)}%`;
    fill.className = "bar-fill" + (!c.active && c.err ? " bad" : !c.active ? " ok" : "");
    toggleBtn.textContent = collapsed ? "▴" : "▾";
    list.hidden = collapsed;
    cancelBtn.hidden = !c.active;
    closePanelBtn.hidden = c.active;
    autoBox.checked = prefs().autoCloseTransfers;
    for (const j of jobs) if (j.row) updateRow(j);
  }

  const ICON = { up: "↑", down: "↓", dir: "📁" };
  function updateRow(j) {
    const text = { queued: "na fila", running: j.size ? `${Math.round((j.done / j.size) * 100)}% · ${formatSize(j.done)} de ${formatSize(j.size)}` : "…", ok: j.kind === "dir" ? "pasta criada" : `${j.kind === "down" ? "baixado" : "enviado"} · ${formatSize(j.size)}`, skipped: j.note, err: j.note, cancelled: "cancelado" }[j.state];
    j.row.stat.textContent = text;
    j.row.stat.className = "xfer-stat " + (j.state === "err" ? "error-text" : "muted");
    j.row.fill.style.width = j.state === "ok" || j.state === "err" ? "100%" : j.size ? `${Math.min(100, (j.done / j.size) * 100)}%` : j.state === "running" ? "30%" : "0";
    j.row.fill.className = "bar-fill" + (j.state === "ok" ? " ok" : j.state === "err" ? " bad" : "");
    j.row.el.dataset.state = j.state;
  }
  function ensureRow(j) {
    if (j.row) return;
    const fillEl = h("div", { class: "bar-fill" });
    const stat = h("span", { class: "xfer-stat muted" });
    const el = h("div", { class: "xfer" }, h("span", { class: "xfer-name", title: j.label }, `${ICON[j.kind]} ${j.label}`), h("div", { class: "bar" }, fillEl), stat);
    j.row = { el, fill: fillEl, stat };
    list.append(el);
    list.scrollTop = list.scrollHeight;
    // Keep the list light on huge folders: drop the oldest rows that succeeded.
    const done = [...list.children].filter((x) => x.dataset.state === "ok");
    for (let i = 0; i < done.length - 150; i++) done[i].remove();
  }

  function pauseAutoClose() { clearInterval(closeTimer); closeTimer = null; countdown.textContent = ""; }
  function maybeAutoClose() {
    pauseAutoClose();
    const c = counts();
    const p = prefs();
    if (c.active || !jobs.length || !p.autoCloseTransfers || c.err || cancelled) return;
    let left = p.autoCloseSeconds;
    countdown.textContent = `fecha em ${left} s`;
    closeTimer = setInterval(() => {
      left--;
      if (left <= 0) resetPanel(); else countdown.textContent = `fecha em ${left} s`;
    }, 1000);
  }
  panel.addEventListener("mouseenter", pauseAutoClose);
  panel.addEventListener("mouseleave", maybeAutoClose);

  // resetPanel forgets the finished batch and hides the panel.
  function resetPanel() {
    pauseAutoClose();
    jobs.length = 0; list.replaceChildren();
    cancelled = false; conflictPolicy = null; collapsed = false;
    render();
  }
  function cancelAll() {
    cancelled = true;
    for (const j of jobs) if (j.state === "queued") { j.state = "cancelled"; if (j.row) updateRow(j); }
    render();
  }
  // addJobs starts a new batch, or extends the running one.
  function addJobs(added) {
    if (!counts().active) { resetPanel(); startedAt = Date.now(); }
    pauseAutoClose();
    jobs.push(...added);
    render();
  }
  function newJob(kind, label, size, run) { return { kind, label, size, done: 0, state: "queued", note: "", row: null, run }; }

  // execute runs one job; it never throws, failures become the job's state.
  async function execute(j) {
    if (cancelled) { j.state = "cancelled"; ensureRow(j); updateRow(j); scheduleRender(); return; }
    j.state = "running"; ensureRow(j); render();
    try {
      await connect();
      await j.run(j);
      if (j.state === "running") j.state = "ok";
    } catch (e) {
      if (e && e.cancelled) j.state = "cancelled";
      else { j.state = "err"; j.note = e.message || String(e); }
    }
    updateRow(j);
    render();
    if (!counts().active) maybeAutoClose();
  }

  // --- listing ---
  async function list_(path) {
    await connect();
    sendOp({ op: "list", path });
    const f = await next();
    if (!f.msg || f.msg.op !== "list") fail(f);
    return f.msg;
  }
  async function go(path) {
    if (closed) return;
    try {
      const r = await serial(() => list_(path));
      cwd = r.path; pathInput.value = cwd;
      const rows = (r.entries || []).map((e) => {
        const name = e.dir
          ? h("button", { class: "linklike", onclick: () => go(joinPath(cwd, e.name)) }, "📁 " + e.name)
          : h("span", null, "📄 " + e.name);
        return h("tr", null,
          h("td", null, name, e.link && h("span", { class: "muted", title: "Link simbólico" }, " →")),
          h("td", { class: "muted mono" }, e.dir ? "" : formatSize(e.size)),
          h("td", { class: "muted" }, e.mtime ? new Date(e.mtime * 1000).toLocaleString() : ""),
          h("td", null, !e.dir && h("button", { class: "btn small", onclick: () => download(joinPath(cwd, e.name), e.name, e.size) }, "Baixar")));
      });
      body.replaceChildren(...(rows.length ? rows : [h("tr", null, h("td", { colspan: "4", class: "muted" }, "Pasta vazia."))]));
    } catch (e) { toast(`${machine.name}: ${e.message}`, "err"); pathInput.value = cwd; }
  }
  pathInput.addEventListener("keydown", (e) => { if (e.key === "Enter") go(pathInput.value.trim()); });

  async function makeFolder() {
    const name = prompt("Nome da nova pasta:");
    if (!name) return;
    try {
      await serial(async () => {
        await connect();
        sendOp({ op: "mkdir", path: joinPath(cwd, name) });
        const f = await next();
        if (!f.msg || f.msg.op !== "done") fail(f);
      });
      go(cwd);
    } catch (e) { toast(e.message, "err"); }
  }

  // --- downloads ---
  function download(path, name, size) {
    const job = newJob("down", name, size || 0, async (j) => {
      sendOp({ op: "get", path });
      let f = await next();
      if (!f.msg || f.msg.op !== "get") fail(f);
      const total = f.msg.size, parts = [];
      j.size = total;
      for (;;) {
        f = await next();
        if (f.binary) { parts.push(f.binary); j.done += f.binary.length; scheduleRender(); continue; }
        if (f.msg && f.msg.op === "done") break;
        fail(f);
      }
      if (j.done !== total) throw new Error(`o arquivo mudou durante o download (${j.done} de ${total} bytes)`);
      const url = URL.createObjectURL(new Blob(parts));
      const a = h("a", { href: url, download: name });
      document.body.append(a); a.click(); a.remove();
      setTimeout(() => URL.revokeObjectURL(url), 60000);
    });
    addJobs([job]);
    serial(() => execute(job));
  }

  // --- uploads (files and whole folders) ---
  async function putFile(j, file, dest) {
    let overwrite = conflictPolicy === "replace";
    for (;;) {
      sendOp({ op: "put", path: dest, size: file.size, overwrite, parents: true });
      const f = await next();
      if (f.msg && f.msg.op === "ready") break;
      if (f.msg && f.msg.op === "error" && /já existe/.test(f.msg.error) && !overwrite) {
        let choice = conflictPolicy === "skip" ? "skip" : await chooseConflict(dest);
        if (choice === "replaceAll") { conflictPolicy = "replace"; choice = "replace"; }
        if (choice === "skipAll") { conflictPolicy = "skip"; choice = "skip"; }
        if (choice === "replace") { overwrite = true; continue; }
        j.state = "skipped"; j.note = "já existe, pulado";
        return;
      }
      fail(f);
    }
    for (let off = 0; off < file.size; off += FILE_CHUNK) {
      if (cancelled) { if (ws) ws.close(); throw { cancelled: true }; } // the agent drops the half-written file
      if (dead) fail(dead);
      while (ws.bufferedAmount > 4 * 1024 * 1024) await new Promise((r) => setTimeout(r, 25));
      ws.send(await file.slice(off, off + FILE_CHUNK).arrayBuffer());
      j.done = Math.min(off + FILE_CHUNK, file.size);
      scheduleRender();
    }
    sendOp({ op: "end" });
    const f = await next();
    if (!f.msg || f.msg.op !== "done") fail(f);
    j.done = file.size;
  }

  // enqueueUploads sends what was picked or dropped into the open folder,
  // keeping its structure: folders are created on the way (or, for empty
  // ones, explicitly) and files go to the same relative path.
  function enqueueUploads(collected) {
    const base = cwd;
    if (!base) { toast("Abra uma pasta antes de enviar.", "err"); return; }
    const added = [];
    for (const rel of emptyFolders(collected)) {
      added.push(newJob("dir", rel + "/", 0, async () => {
        sendOp({ op: "mkdir", path: joinPath(base, rel), parents: true });
        const f = await next();
        if (!f.msg || f.msg.op !== "done") fail(f);
      }));
    }
    for (const { file, rel } of collected.files) {
      added.push(newJob("up", rel, file.size, (j) => putFile(j, file, joinPath(base, rel))));
    }
    if (!added.length) { toast("Nada para enviar.", "err"); return; }
    addJobs(added);
    let last = null;
    for (const j of added) last = serial(() => execute(j));
    last.then(() => go(cwd));
  }
  picker.addEventListener("change", () => { enqueueUploads(collectPicked(picker.files)); picker.value = ""; });
  folderPicker.addEventListener("change", () => { enqueueUploads(collectPicked(folderPicker.files)); folderPicker.value = ""; });
  for (const ev of ["dragenter", "dragover"]) overlay.addEventListener(ev, (e) => { e.preventDefault(); drop.classList.add("over"); });
  overlay.addEventListener("dragleave", (e) => { if (e.target === overlay || e.target === drop) drop.classList.remove("over"); });
  overlay.addEventListener("drop", (e) => {
    e.preventDefault(); drop.classList.remove("over");
    collectDropped(e.dataTransfer).then(enqueueUploads).catch((err) => toast(`Não consegui ler o que foi solto: ${err.message || err}`, "err"));
  });

  const onKey = (e) => { if (e.key === "Escape" && !document.querySelector(".overlay")) close(); };
  document.addEventListener("keydown", onKey);
  function close() {
    if (counts().active && !confirm("Há transferências em andamento. Fechar agora cancela o que falta. Fechar mesmo?")) return;
    closed = true; cancelled = true;
    pauseAutoClose();
    document.removeEventListener("keydown", onKey);
    if (ws) ws.close();
    overlay.remove();
  }
  go("");
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
  if (bytes < 1073741824) return `${(bytes / 1048576).toFixed(1)} MB`;
  return `${(bytes / 1073741824).toFixed(2)} GB`;
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
