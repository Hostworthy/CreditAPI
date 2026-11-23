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

function fmtAgo(value) {
  if (!value) return "never";
  const secs = Math.round((Date.now() - new Date(value).getTime()) / 1000);
  if (secs < 45) return "just now";
  const mins = Math.round(secs / 60);
  if (mins < 60) return `${mins} minute${mins === 1 ? "" : "s"} ago`;
  const hours = Math.round(mins / 60);
  if (hours < 24) return `${hours} hour${hours === 1 ? "" : "s"} ago`;
  return fmtDate(value);
}

function fmtDay(value) {
  const d = new Date(value);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleDateString(undefined, { dateStyle: "medium" });
}

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

function tokenText(token) {
  if (token.source === "login") {
    const until = token.expires_about ? `, valid until about ${fmtDay(token.expires_about)}` : "";
    return `Token ${token.hint} saved ${fmtDate(token.saved_at)}${until}.`;
  }
  if (token.source === "env") return `Using CLAUDE_CODE_OAUTH_TOKEN from the environment (${token.hint}).`;
  return "No token saved yet.";
}

function authText(auth, token) {
  if (auth?.state === "valid") return `Signed in and working (verified ${fmtAgo(auth.checked_at)}).`;
  if (auth?.state === "login_required") return "Sign-in required: requests fail until you sign in again.";
  if (token.source) return "Signed in (not verified since the server started).";
  return "Not signed in. Requests will fail until you sign in to Claude.";
}

// --- admin page --------------------------------------------------------------------

async function loadClaudeSummary() {
  const s = await api("GET", "/admin/api/claude");
  const summary = $("#claude-summary");
  summary.textContent = authText(s.auth, s.token);
  summary.dataset.state = s.auth?.state || "unknown";
  renderUsage($("#usage"), s.runtime.rate_limit);
  const rt = s.runtime;
  const facts = [
    ["Token", tokenText(s.token)],
    ["Claude CLI", rt.cli_version || "unknown"],
    ["Requests", `${fmtNum(rt.completed)} ok, ${fmtNum(rt.failed)} failed, ${rt.in_flight} running`],
    ["Default model", s.config.default_model],
  ];
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
//
// Opening /login first resumes a sign-in that is already in progress (so a
// reload never invalidates a code), otherwise checks whether the current login
// works. Only when Claude says a login is required does it start a new one.

let pollTimer = null;
let lastFlowState = "";
let lastAuthReason = "";
const activeStates = new Set(["starting", "waiting_for_code", "submitting"]);
const flowLabels = {
  starting: "Starting…",
  waiting_for_code: "Waiting for your code",
  submitting: "Checking the code…",
  success: "Signed in",
  failed: "Sign-in failed",
};

function setAuth(state, detail = "") {
  const box = $("#auth-box");
  box.dataset.state = state;
  const titles = {
    checking: "Checking your Claude login…",
    valid: "You're all set. Claude is signed in and working.",
    login_required: "Sign-in required.",
    signing_in: "Finish signing in below.",
    error: "Couldn't verify the Claude login right now.",
  };
  $("#auth-title").textContent = titles[state] || state;
  $("#auth-detail").textContent = detail;
  const startBtn = $("[data-action=start-login]");
  startBtn.hidden = state === "checking" || state === "signing_in";
  startBtn.textContent = state === "valid" ? "Sign in again anyway" : "Sign in";
  startBtn.className = state === "valid" ? "" : "primary";
  $("[data-action=check]").hidden = state === "signing_in";
}

function renderToken(token) {
  $("#token-info").textContent = tokenText(token);
  $("[data-action=clear-token]").hidden = token.source !== "login";
}

function renderFlow(login) {
  const flow = $("#flow");
  flow.hidden = login.state === "idle" || login.state === "success";
  $("#flow-state").textContent = flowLabels[login.state] || login.state;
  $("#url-wait").hidden = Boolean(login.url) || login.state === "failed";
  $("#url-box").hidden = !login.url || login.state === "failed";
  if (login.url) {
    $("#login-link").href = login.url;
    $("#login-url").textContent = login.url;
  }
  $("#flow-error").textContent = login.error || "";
  $("#cli-output").textContent = login.output || "";
  $("#output-box").hidden = !login.output;
  $("#code-form").hidden = login.state === "failed";
  $("#code-form button[type=submit]").disabled = login.state !== "waiting_for_code";
  $("[data-action=restart-login]").hidden = login.state !== "failed";
}

async function runCheck(force, { autoStart = true } = {}) {
  setAuth("checking");
  let r;
  try {
    r = await api("POST", "/admin/api/claude/check", { force });
  } catch (ex) {
    if (ex.status !== 401) setAuth("error", ex.message);
    return;
  }
  renderToken(r.token);
  if (r.state === "valid") {
    const how = r.source === "request" ? "by a real request" : "by a test prompt";
    setAuth("valid", `Verified ${fmtAgo(r.checked_at)} ${how}. Nothing to do.`);
    $("#flow").hidden = true;
  } else if (r.state === "login_required") {
    lastAuthReason = r.reason || "";
    if (autoStart) {
      await startLogin();
    } else {
      setAuth("login_required", lastAuthReason);
    }
  } else {
    setAuth("error", `${r.reason || "Unknown error"}. Your login may still be fine; try again in a moment.`);
  }
}

async function startLogin() {
  setAuth("signing_in", lastAuthReason ? `Reason: ${lastAuthReason}` : "");
  renderFlow({ state: "starting" });
  try {
    await api("POST", "/admin/api/claude/login");
  } catch (ex) {
    $("#flow-error").textContent = ex.message;
  }
  lastFlowState = "starting";
  await refreshLogin();
}

async function refreshLogin() {
  clearTimeout(pollTimer);
  let s;
  try {
    s = await api("GET", "/admin/api/claude");
  } catch (ex) {
    if (ex.status !== 401) pollTimer = setTimeout(refreshLogin, 3000);
    return;
  }
  renderToken(s.token);
  const state = s.login.state;
  const justSucceeded = state === "success" && activeStates.has(lastFlowState);
  lastFlowState = state;
  renderFlow(s.login);
  if (activeStates.has(state)) {
    setAuth("signing_in", lastAuthReason ? `Reason: ${lastAuthReason}` : "");
    pollTimer = setTimeout(refreshLogin, 1500);
  } else if (justSucceeded) {
    await runCheck(true, { autoStart: false });
  } else if (state === "failed") {
    setAuth("login_required", "The last sign-in attempt did not finish. Start over to get a fresh link.");
  }
}

async function boot() {
  let s;
  try {
    s = await api("GET", "/admin/api/claude");
  } catch {
    return;
  }
  renderToken(s.token);
  if (activeStates.has(s.login.state)) {
    lastFlowState = s.login.state;
    await refreshLogin();
    return;
  }
  await runCheck(false);
}

function setupLogin() {
  $("[data-action=start-login]").addEventListener("click", (e) => withBusy(e.currentTarget, startLogin));
  $("[data-action=restart-login]").addEventListener("click", (e) => withBusy(e.currentTarget, startLogin));
  $("[data-action=check]").addEventListener("click", (e) => withBusy(e.currentTarget, () => runCheck(true, { autoStart: false })));

  $("#code-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#flow-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        await api("POST", "/admin/api/claude/login/code", { code: $("#code").value });
        $("#code").value = "";
        lastFlowState = "submitting";
      } catch (ex) {
        $("#flow-error").textContent = ex.message;
      }
      await refreshLogin();
    });
  });

  // One-tap paste (the Clipboard API needs HTTPS; otherwise the button stays hidden).
  const pasteBtn = $("[data-action=paste-code]");
  if (window.isSecureContext && navigator.clipboard?.readText) {
    pasteBtn.hidden = false;
    pasteBtn.addEventListener("click", async () => {
      try {
        const text = (await navigator.clipboard.readText()).trim();
        if (!text) return;
        $("#code").value = text;
        $("#code-form").requestSubmit();
      } catch {
        $("#flow-error").textContent = "Could not read the clipboard. Long-press the box and paste instead.";
      }
    });
  }

  // Coming back from the Claude tab: put the cursor in the code box.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible" && lastFlowState === "waiting_for_code" && !$("#code").value) {
      $("#code").focus();
    }
  });

  $("[data-action=cancel-login]").addEventListener("click", async () => {
    clearTimeout(pollTimer);
    await api("DELETE", "/admin/api/claude/login").catch(() => {});
    lastFlowState = "idle";
    $("#flow").hidden = true;
    setAuth("login_required", "Sign-in cancelled.");
  });

  $("[data-action=clear-token]").addEventListener("click", async (e) => {
    await withBusy(e.currentTarget, async () => {
      await api("DELETE", "/admin/api/claude/token");
      await runCheck(true, { autoStart: false });
    });
  });

  $("#token-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#token-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        await api("PUT", "/admin/api/claude/token", { token: $("#token").value });
        $("#token").value = "";
        await runCheck(true, { autoStart: false });
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
  if (page === "login") boot();
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
