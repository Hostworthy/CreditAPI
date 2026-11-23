"use strict";

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => [...root.querySelectorAll(sel)];
const page = document.body.dataset.page;

class ApiError extends Error {
  constructor(status, message) {
    super(message);
    this.status = status;
  }
}

async function api(method, path, body) {
  const opts = { method, credentials: "same-origin", headers: {} };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let data = {};
  try { data = await res.json(); } catch { /* empty body */ }
  if (!res.ok) {
    if (res.status === 401 && path !== "/admin/api/session") showSignin();
    throw new ApiError(res.status, data.error || `Request failed (${res.status})`);
  }
  return data;
}

function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") node.className = v;
    else if (k === "text") node.textContent = v;
    else if (k.startsWith("on")) node.addEventListener(k.slice(2), v);
    else if (v !== false && v != null) node.setAttribute(k, v === true ? "" : v);
  }
  for (const c of children) if (c != null) node.append(c);
  return node;
}

function fmtDate(value) {
  if (!value) return "never";
  const d = typeof value === "number" ? new Date(value * 1000) : new Date(value);
  if (Number.isNaN(d.getTime())) return "unknown";
  return d.toLocaleString(undefined, { dateStyle: "medium", timeStyle: "short" });
}

function fmtNum(n) {
  return new Intl.NumberFormat().format(n || 0);
}

async function withBusy(button, fn) {
  if (button) button.disabled = true;
  try { return await fn(); } finally { if (button) button.disabled = false; }
}

// --- auth shell ----------------------------------------------------------------

function showSignin() {
  $("#signin").hidden = false;
  $$("[data-auth-only]").forEach((n) => { n.hidden = true; });
  $("#password")?.focus();
}

function showApp() {
  $("#signin").hidden = true;
  $$("[data-auth-only]").forEach((n) => { n.hidden = false; });
}

$("#signin-form")?.addEventListener("submit", async (e) => {
  e.preventDefault();
  const err = $("#signin-error");
  err.textContent = "";
  const btn = e.submitter;
  await withBusy(btn, async () => {
    try {
      await api("POST", "/admin/api/session", { password: $("#password").value });
      $("#password").value = "";
      showApp();
      startPage();
    } catch (ex) {
      err.textContent = ex.message;
    }
  });
});

$$("[data-action=signout]").forEach((b) => b.addEventListener("click", async () => {
  await api("DELETE", "/admin/api/session").catch(() => {});
  showSignin();
}));

// --- copy buttons --------------------------------------------------------------

document.addEventListener("click", async (e) => {
  const btn = e.target.closest("[data-copy]");
  if (!btn) return;
  const src = $(btn.dataset.copy);
  if (!src) return;
  const text = src.textContent.trim();
  const original = btn.textContent;
  try {
    await navigator.clipboard.writeText(text);
    btn.textContent = "Copied";
  } catch {
    // Clipboard API needs HTTPS; fall back to selecting the text.
    const range = document.createRange();
    range.selectNodeContents(src);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
    btn.textContent = "Selected, copy it";
  }
  setTimeout(() => { btn.textContent = original; }, 1800);
});

// --- shared claude status --------------------------------------------------------

const windowNames = { five_hour: "5-hour window", seven_day: "7-day window", seven_day_opus: "7-day Opus", seven_day_sonnet: "7-day Sonnet" };

function renderUsage(container, rateLimit) {
  container.replaceChildren();
  const windows = rateLimit?.unifiedWindows;
  if (!windows) return;
  for (const [name, w] of Object.entries(windows)) {
    if (typeof w?.utilization !== "number") continue;
    const pct = Math.max(0, Math.min(100, Math.round(w.utilization * 100)));
    const bar = el("div", { class: "bar", role: "progressbar", "aria-valuenow": pct, "aria-valuemin": 0, "aria-valuemax": 100, "aria-label": windowNames[name] || name });
    const fill = el("span");
    fill.style.width = `${pct}%`;
    bar.append(fill);
    container.append(el("div", {},
      el("div", { class: "label" },
        el("span", { text: `${windowNames[name] || name}: ${pct}% used` }),
        el("span", { text: w.resetsAt ? `resets ${fmtDate(w.resetsAt)}` : "" })),
      bar));
  }
}

function tokenSummary(token) {
  if (token.source === "login") return `Signed in (token ${token.hint}, saved ${fmtDate(token.saved_at)}).`;
  if (token.source === "env") return `Using CLAUDE_CODE_OAUTH_TOKEN from the environment (${token.hint}).`;
  return "Not signed in. Requests will fail until you sign in to Claude.";
}

// --- admin page --------------------------------------------------------------------

async function loadClaudeSummary() {
  const s = await api("GET", "/admin/api/claude");
  $("#claude-summary").textContent = tokenSummary(s.token);
  renderUsage($("#usage"), s.runtime.rate_limit);
  const rt = s.runtime;
  const facts = [
    ["Claude CLI", rt.cli_version || "unknown"],
    ["Requests", `${fmtNum(rt.completed)} ok, ${fmtNum(rt.failed)} failed, ${rt.in_flight} running`],
    ["Default model", s.config.default_model],
  ];
  if (s.config.mcp_servers?.length) facts.push(["MCP servers", s.config.mcp_servers.join(", ")]);
  if (rt.last_error) facts.push(["Last error", `${fmtDate(rt.last_error_at)}: ${rt.last_error}`]);
  $("#runtime").replaceChildren(...facts.flatMap(([k, v]) => [el("dt", { text: k }), el("dd", { text: v })]));
  const models = [s.config.default_model, ...s.config.models].filter((m, i, a) => m && a.indexOf(m) === i);
  $("#model-list").replaceChildren(...models.map((m, i) => [i ? ", " : "", el("code", { text: m })]).flat());
}

async function loadKeys() {
  const { keys } = await api("GET", "/admin/api/keys");
  const list = $("#key-list");
  list.replaceChildren();
  $("#no-keys").hidden = keys.length > 0;
  for (const k of keys) {
    const toggle = el("button", {
      type: "button", class: "small", text: k.disabled ? "Enable" : "Disable",
      onclick: (e) => withBusy(e.currentTarget, async () => {
        await api("PATCH", `/admin/api/keys/${encodeURIComponent(k.id)}`, { disabled: !k.disabled });
        await loadKeys();
      }),
    });
    const del = el("button", {
      type: "button", class: "small danger-outline", text: "Delete",
      onclick: async () => {
        if (!(await confirmDialog(`Delete key "${k.name}"? Apps using it will stop working.`))) return;
        await api("DELETE", `/admin/api/keys/${encodeURIComponent(k.id)}`);
        await loadKeys();
      },
    });
    list.append(el("li", { class: k.disabled ? "disabled" : "" },
      el("div", { class: "row space" }, el("span", { class: "name", text: k.name }), el("code", { text: k.hint })),
      el("div", { class: "meta" },
        el("span", { text: k.disabled ? "Disabled" : "Active" }),
        el("span", { text: `Created ${fmtDate(k.created_at)}` }),
        el("span", { text: `Last used ${fmtDate(k.last_used_at)}` }),
        el("span", { text: `${fmtNum(k.requests)} requests` }),
        el("span", { text: `${fmtNum(k.input_tokens)} in / ${fmtNum(k.output_tokens)} out tokens` })),
      el("div", { class: "actions" }, toggle, del)));
  }
}

function confirmDialog(text) {
  const dlg = $("#confirm-dialog");
  $("#confirm-text").textContent = text;
  dlg.returnValue = "";
  dlg.showModal();
  return new Promise((resolve) => {
    dlg.addEventListener("close", () => resolve(dlg.returnValue === "ok"), { once: true });
  });
}

function setupAdmin() {
  const base = `${location.origin}/v1`;
  $("#base-url").textContent = base;
  $("#curl-example").textContent =
`curl ${base}/chat/completions \\
  -H "Authorization: Bearer sk-cmta-..." \\
  -H "Content-Type: application/json" \\
  -d '{"model":"sonnet","messages":[{"role":"user","content":"Hello!"}]}'`;

  $("#create-key").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#key-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        const res = await api("POST", "/admin/api/keys", { name: $("#key-name").value });
        $("#key-name").value = "";
        $("#new-secret-value").textContent = res.secret;
        $("#new-secret").hidden = false;
        await loadKeys();
      } catch (ex) {
        $("#key-error").textContent = ex.message;
      }
    });
  });
}

// --- login page --------------------------------------------------------------------

let pollTimer = null;
const activeStates = new Set(["starting", "waiting_for_code", "submitting"]);
const stateLabels = {
  idle: "Not started",
  starting: "Starting…",
  waiting_for_code: "Waiting for your code",
  submitting: "Checking the code…",
  success: "Signed in",
  failed: "Failed",
};

function renderLogin(s) {
  $("#token-status").textContent = tokenSummary(s.token);
  $("[data-action=clear-token]").hidden = s.token.source !== "login";

  const login = s.login;
  const flow = $("#flow");
  flow.hidden = login.state === "idle";
  $("#flow-state").textContent = stateLabels[login.state] || login.state;
  $("#url-wait").hidden = Boolean(login.url) || login.state === "failed";
  $("#url-box").hidden = !login.url;
  if (login.url) {
    $("#login-link").href = login.url;
    $("#login-url").textContent = login.url;
  }
  $("#flow-error").textContent = login.error || "";
  $("#cli-output").textContent = login.output || "";
  $("#output-box").hidden = !login.output;
  $("#code-form button").disabled = login.state !== "waiting_for_code";

  clearTimeout(pollTimer);
  if (activeStates.has(login.state)) {
    pollTimer = setTimeout(refreshLogin, 1500);
  }
}

let lastState = "";
async function refreshLogin() {
  try {
    const s = await api("GET", "/admin/api/claude");
    const becameSuccess = s.login.state === "success" && lastState && lastState !== "success";
    lastState = s.login.state;
    renderLogin(s);
    if (becameSuccess) runTest();
  } catch (ex) {
    if (ex.status !== 401) pollTimer = setTimeout(refreshLogin, 3000);
  }
}

async function runTest(button) {
  const out = $("#test-result");
  out.className = "result";
  out.textContent = "Sending a test prompt to Claude…";
  await withBusy(button, async () => {
    try {
      const r = await api("POST", "/admin/api/claude/test", {});
      if (r.ok) {
        out.className = "result ok";
        out.textContent = `Working. Claude (${r.model}) replied "${r.reply}" in ${(r.duration_ms / 1000).toFixed(1)}s.`;
      } else {
        out.className = "result bad";
        out.textContent = `Test failed: ${r.error}`;
      }
    } catch (ex) {
      out.className = "result bad";
      out.textContent = `Test failed: ${ex.message}`;
    }
  });
}

function setupLogin() {
  $("[data-action=start-login]").addEventListener("click", (e) => withBusy(e.currentTarget, async () => {
    $("#test-result").textContent = "";
    $("#flow").hidden = false;
    $("#flow-state").textContent = stateLabels.starting;
    $("#url-wait").hidden = false;
    $("#url-box").hidden = true;
    $("#flow-error").textContent = "";
    try {
      await api("POST", "/admin/api/claude/login");
    } catch (ex) {
      $("#flow-error").textContent = ex.message;
    }
    lastState = "starting";
    await refreshLogin();
  }));

  $("#code-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#flow-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        await api("POST", "/admin/api/claude/login/code", { code: $("#code").value });
        $("#code").value = "";
      } catch (ex) {
        $("#flow-error").textContent = ex.message;
      }
      await refreshLogin();
    });
  });

  $("[data-action=cancel-login]").addEventListener("click", async () => {
    await api("DELETE", "/admin/api/claude/login").catch(() => {});
    lastState = "idle";
    await refreshLogin();
  });

  $("[data-action=test]").addEventListener("click", (e) => runTest(e.currentTarget));

  $("[data-action=clear-token]").addEventListener("click", async (e) => {
    await withBusy(e.currentTarget, async () => {
      await api("DELETE", "/admin/api/claude/token");
      $("#test-result").textContent = "";
      await refreshLogin();
    });
  });

  $("#token-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#token-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        await api("PUT", "/admin/api/claude/token", { token: $("#token").value });
        $("#token").value = "";
        await refreshLogin();
        await runTest();
      } catch (ex) {
        $("#token-error").textContent = ex.message;
      }
    });
  });
}

// --- boot ------------------------------------------------------------------------

let pageReady = false;
function startPage() {
  if (!pageReady) {
    pageReady = true;
    if (page === "admin") setupAdmin();
    if (page === "login") setupLogin();
  }
  if (page === "admin") {
    loadKeys().catch(() => {});
    loadClaudeSummary().catch(() => {});
  }
  if (page === "login") refreshLogin();
}

(async () => {
  try {
    const me = await api("GET", "/admin/api/me");
    if (me.authenticated) {
      showApp();
      startPage();
    } else {
      showSignin();
    }
  } catch {
    showSignin();
  }
})();
