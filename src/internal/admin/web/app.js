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

// --- shared subscription status ----------------------------------------------------

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

function credentialText(sub) {
  const c = sub.credential || {};
  if (c.source === "login") {
    const until = c.expires_about ? `, valid until about ${fmtDay(c.expires_about)}` : "";
    return `Credential ${c.hint || "(hidden)"} saved ${fmtDate(c.saved_at)}${until}.`;
  }
  if (c.source === "env") return `Using ${c.name} from the environment (${c.hint || "set"}).`;
  if (sub.provider === "cursor") {
    if (c.saved_at) return `Signed in through the browser ${fmtDate(c.saved_at)} (kept by the Cursor CLI).`;
    return "No API key saved. A browser sign-in is kept by the Cursor CLI itself.";
  }
  return "No credential saved yet.";
}

function authText(sub) {
  const auth = sub.auth || {};
  if (sub.disabled) return "Disabled: not used for requests.";
  if (auth.state === "valid") return `Signed in and working (verified ${fmtAgo(auth.checked_at)}).`;
  if (auth.state === "login_required") return "Sign-in required: this subscription is skipped until you sign in again.";
  if (sub.credential?.source) return "Signed in (not verified since the server started).";
  return "Not verified yet. Open Sign-in to check it.";
}

const subPath = (id) => `/admin/api/subscriptions/${encodeURIComponent(id)}`;

// --- admin page --------------------------------------------------------------------

let subsCache = [];
let keysCache = [];

async function loadSubscriptions() {
  const data = await api("GET", "/admin/api/subscriptions");
  subsCache = data.subscriptions;
  const select = $("#sub-provider");
  if (!select.options.length) {
    select.replaceChildren(...data.providers.map((p) => el("option", { value: p.id, text: p.label })));
  }
  const list = $("#sub-list");
  list.replaceChildren();
  $("#no-subs").hidden = subsCache.length > 0;
  subsCache.forEach((sub, i) => list.append(subscriptionItem(sub, i, subsCache.length)));
  renderModels(data.providers);
  renderAccess();
}

function subscriptionItem(sub, index, count) {
  const state = sub.disabled ? "disabled" : (sub.auth?.state || "unknown");
  const reload = () => loadSubscriptions();
  const act = (label, fn, cls = "small") => el("button", {
    type: "button", class: cls, text: label,
    onclick: (e) => withBusy(e.currentTarget, async () => {
      try { await fn(); } catch (ex) { $("#sub-error").textContent = ex.message; }
      await reload();
    }),
  });
  const rt = sub.runtime || {};
  const usage = el("div", { class: "usage" });
  renderUsage(usage, rt.rate_limit);
  const meta = [
    el("span", { text: `Short name ${sub.slug}` }),
    el("span", { text: `${sub.keys.length} key${sub.keys.length === 1 ? "" : "s"}` }),
    el("span", { text: `${fmtNum(rt.completed)} ok, ${fmtNum(rt.failed)} failed` }),
  ];
  if (sub.cli_version) meta.push(el("span", { text: `CLI ${sub.cli_version}` }));
  const item = el("li", { class: sub.disabled ? "disabled" : "" },
    el("div", { class: "row space" },
      el("span", { class: "name" }, sub.name, " ", el("span", { class: `badge ${sub.provider}`, text: sub.provider_label || sub.provider })),
      el("span", { class: "muted small-text", text: `#${index + 1}` })),
    el("p", { class: "sub-state", "data-state": state, text: authText(sub) }),
    el("p", { class: "muted small-text", text: credentialText(sub) }),
    usage,
    el("div", { class: "meta" }, ...meta),
    rt.cooldown_until ? el("p", { class: "muted small-text", text: `Resting until ${fmtDate(rt.cooldown_until)} after a usage limit or overload; other subscriptions answer first.` }) : null,
    rt.last_error ? el("p", { class: "muted small-text", text: `Last error ${fmtDate(rt.last_error_at)}: ${rt.last_error}` }) : null,
    el("div", { class: "actions" },
      el("a", { class: `button small${state === "login_required" ? " primary" : ""}`, href: `/login?subscription=${encodeURIComponent(sub.id)}`, text: "Sign-in" }),
      index > 0 ? act("Move up", () => api("POST", `${subPath(sub.id)}/move`, { direction: "up" })) : null,
      index < count - 1 ? act("Move down", () => api("POST", `${subPath(sub.id)}/move`, { direction: "down" })) : null,
      act("Rename", async () => {
        const name = await promptDialog(sub.name);
        if (name && name !== sub.name) await api("PATCH", subPath(sub.id), { name });
      }),
      act(sub.disabled ? "Enable" : "Disable", () => api("PATCH", subPath(sub.id), { disabled: !sub.disabled })),
      act("Delete", async () => {
        if (!(await confirmDialog(`Delete subscription "${sub.name}"? Its saved sign-in is removed and keys lose access to it.`))) return;
        await api("DELETE", subPath(sub.id));
      }, "small danger-outline")));
  return item;
}

function renderModels(providers) {
  const used = new Set(subsCache.map((s) => s.provider));
  const parts = [];
  for (const p of providers) {
    if (!used.has(p.id)) continue;
    const models = [p.default_model, ...(p.models || [])].filter((m, i, a) => m && a.indexOf(m) === i);
    if (parts.length) parts.push("; ");
    parts.push(`${p.label}: `);
    models.forEach((m, i) => parts.push(i ? ", " : "", el("code", { text: m })));
  }
  if (subsCache[0]) {
    parts.push(". Pin a subscription with ", el("code", { text: `${subsCache[0].slug}/<model>` }), ".");
  }
  $("#model-list").replaceChildren(...(parts.length ? parts : ["Add a subscription first."]));
}

async function loadKeys() {
  const { keys } = await api("GET", "/admin/api/keys");
  keysCache = keys;
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
        await loadSubscriptions();
      },
    });
    const names = k.subscriptions.map((id) => subsCache.find((s) => s.id === id)?.name).filter(Boolean);
    list.append(el("li", { class: k.disabled ? "disabled" : "" },
      el("div", { class: "row space" }, el("span", { class: "name", text: k.name }), el("code", { text: k.hint })),
      el("div", { class: "meta" },
        el("span", { text: k.disabled ? "Disabled" : "Active" }),
        el("span", { text: names.length ? `Uses ${names.join(", ")}` : "No subscriptions" }),
        el("span", { text: `Created ${fmtDate(k.created_at)}` }),
        el("span", { text: `Last used ${fmtDate(k.last_used_at)}` }),
        el("span", { text: `${fmtNum(k.requests)} requests` }),
        el("span", { text: `${fmtNum(k.input_tokens)} in / ${fmtNum(k.output_tokens)} out tokens` })),
      el("div", { class: "actions" }, toggle, del)));
  }
  renderAccess();
}

// renderAccess draws the key × subscription grid. Each checkbox saves the
// key's full list of subscriptions.
function renderAccess() {
  const table = $("#access");
  $("#access-card").hidden = !keysCache.length || !subsCache.length;
  const head = el("tr", {}, el("th", { scope: "col", text: "Key" }),
    ...subsCache.map((s) => el("th", { scope: "col" }, s.name, el("br"), el("span", { class: `badge ${s.provider}`, text: s.provider_label || s.provider }))));
  const rows = keysCache.map((k) => el("tr", {},
    el("th", { scope: "row", text: k.name }),
    ...subsCache.map((s) => {
      const box = el("input", {
        type: "checkbox", "aria-label": `${k.name} may use ${s.name}`, checked: k.subscriptions.includes(s.id),
        onchange: async (e) => {
          $("#access-error").textContent = "";
          const want = subsCache.filter((x) => (x.id === s.id ? e.target.checked : k.subscriptions.includes(x.id))).map((x) => x.id);
          e.target.disabled = true;
          try {
            await api("PATCH", `/admin/api/keys/${encodeURIComponent(k.id)}`, { subscriptions: want });
          } catch (ex) {
            $("#access-error").textContent = ex.message;
          }
          await Promise.all([loadKeys(), loadSubscriptions()]);
        },
      });
      return el("td", {}, box);
    })));
  table.replaceChildren(el("thead", {}, head), el("tbody", {}, ...rows));
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

function promptDialog(value) {
  const dlg = $("#rename-dialog");
  const input = $("#rename-input");
  input.value = value;
  dlg.returnValue = "";
  dlg.showModal();
  input.select();
  return new Promise((resolve) => {
    dlg.addEventListener("close", () => resolve(dlg.returnValue === "ok" ? input.value.trim() : ""), { once: true });
  });
}

function setupAdmin() {
  const base = `${location.origin}/v1`;
  $("#base-url").textContent = base;
  $("#curl-example").textContent =
`curl ${base}/chat/completions \\
  -H "Authorization: Bearer sk-credit-..." \\
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
        await loadSubscriptions();
      } catch (ex) {
        $("#key-error").textContent = ex.message;
      }
    });
  });

  $("#create-sub").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#sub-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        const sub = await api("POST", "/admin/api/subscriptions", { name: $("#sub-name").value, provider: $("#sub-provider").value, keys: [] });
        $("#sub-name").value = "";
        await loadSubscriptions();
        await loadKeys();
        $("#sub-error").textContent = "";
        const hint = el("p", { class: "result ok" }, `Added "${sub.name}". Tick the keys that may use it under Access, then `,
          el("a", { href: `/login?subscription=${encodeURIComponent(sub.id)}`, text: "sign it in" }), ".");
        $("#create-sub").after(hint);
        setTimeout(() => hint.remove(), 15000);
      } catch (ex) {
        $("#sub-error").textContent = ex.message;
      }
    });
  });
}

// --- login page --------------------------------------------------------------------
//
// /login?subscription=<id> manages one subscription's sign-in. Opening it
// first resumes a sign-in that is already in progress (so a reload never
// invalidates a code), otherwise checks whether the current login works. Only
// when the check says a login is required does it start a new one.

let pollTimer = null;
let lastFlowState = "";
let lastAuthReason = "";
let current = null; // the selected subscription
const activeStates = new Set(["starting", "waiting_for_code", "waiting_for_browser", "submitting"]);
const flowLabels = {
  starting: "Starting…",
  waiting_for_code: "Waiting for your code",
  waiting_for_browser: "Waiting for you to approve in the browser",
  submitting: "Checking the code…",
  success: "Signed in",
  failed: "Sign-in failed",
};

function label() {
  return current ? `${current.name} (${current.provider_label})` : "the subscription";
}

function setAuth(state, detail = "") {
  const box = $("#auth-box");
  box.dataset.state = state;
  const titles = {
    checking: `Checking ${label()}…`,
    valid: `You're all set. ${label()} is signed in and working.`,
    login_required: "Sign-in required.",
    signing_in: "Finish signing in below.",
    error: `Couldn't verify ${label()} right now.`,
    disabled: `${label()} is disabled on the admin page.`,
  };
  $("#auth-title").textContent = titles[state] || state;
  $("#auth-detail").textContent = detail;
  const startBtn = $("[data-action=start-login]");
  startBtn.hidden = state === "checking" || state === "signing_in";
  startBtn.textContent = state === "valid" ? "Sign in again anyway" : "Sign in";
  startBtn.className = state === "valid" ? "" : "primary";
  $("[data-action=check]").hidden = state === "signing_in";
}

function renderCredential(sub) {
  current = { ...current, ...sub };
  $("#token-info").textContent = credentialText(sub);
  // Cursor keeps browser sign-ins itself, so signing out is always offered.
  const c = sub.credential || {};
  $("[data-action=clear-token]").hidden = c.source !== "login" && sub.provider !== "cursor";
}

function renderProvider(sub) {
  const claude = sub.provider === "claude";
  $("#flow-title").textContent = `Sign in to ${sub.provider_label}`;
  $("#step-open").textContent = `Open the ${sub.provider_label} sign-in page (it opens in a new tab) and approve access${claude ? "" : " with the account you want to use"}.`;
  $("#login-link").textContent = `Open ${sub.provider_label} sign-in`;
  $("#step-code").hidden = !claude;
  $("#step-browser").hidden = claude;
  $("#token-card").hidden = false;
  if (claude) {
    $("#token-summary").textContent = "Already have a token?";
    $("#token-help").replaceChildren("Run ", el("code", { text: "claude setup-token" }), " on any computer with a browser and paste the resulting ", el("code", { text: "sk-ant-oat01-…" }), " token here.");
    $("#token").placeholder = "sk-ant-oat01-…";
    $("#token-label").textContent = "OAuth token";
  } else {
    $("#token-summary").textContent = "Use a Cursor API key instead?";
    $("#token-help").textContent = "Create an API key in your Cursor dashboard and paste it here. It is used instead of a browser sign-in.";
    $("#token").placeholder = "Cursor API key";
    $("#token-label").textContent = "Cursor API key";
  }
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
  if (!current) return;
  setAuth("checking");
  let r;
  try {
    r = await api("POST", `${subPath(current.id)}/check`, { force });
  } catch (ex) {
    if (ex.status !== 401) setAuth("error", ex.message);
    return;
  }
  renderCredential(r.subscription);
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
    setAuth("error", `${r.reason || "Unknown error"}. The login may still be fine; try again in a moment.`);
  }
}

async function startLogin() {
  if (!current) return;
  setAuth("signing_in", lastAuthReason ? `Reason: ${lastAuthReason}` : "");
  renderFlow({ state: "starting" });
  try {
    await api("POST", `${subPath(current.id)}/login`);
  } catch (ex) {
    $("#flow-error").textContent = ex.message;
  }
  lastFlowState = "starting";
  await refreshLogin();
}

async function refreshLogin() {
  clearTimeout(pollTimer);
  if (!current) return;
  const id = current.id;
  let s;
  try {
    s = await api("GET", subPath(id));
  } catch (ex) {
    if (ex.status !== 401 && ex.status !== 404) pollTimer = setTimeout(refreshLogin, 3000);
    return;
  }
  if (current?.id !== id) return; // switched subscriptions meanwhile
  renderCredential(s);
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

async function selectSubscription(id) {
  clearTimeout(pollTimer);
  lastFlowState = "";
  lastAuthReason = "";
  $("#flow").hidden = true;
  let s;
  try {
    s = await api("GET", subPath(id));
  } catch {
    return;
  }
  current = s;
  const url = new URL(location.href);
  url.searchParams.set("subscription", id);
  history.replaceState(null, "", url);
  renderProvider(s);
  renderCredential(s);
  if (activeStates.has(s.login.state)) {
    lastFlowState = s.login.state;
    await refreshLogin();
    return;
  }
  if (s.disabled) {
    setAuth("disabled", "Enable it on the admin page to use it for requests. You can still sign it in.");
    return;
  }
  await runCheck(false);
}

async function boot() {
  let data;
  try {
    data = await api("GET", "/admin/api/subscriptions");
  } catch {
    return;
  }
  const subs = data.subscriptions;
  $("#no-subscriptions").hidden = subs.length > 0;
  $("#sub-card").hidden = subs.length === 0;
  if (!subs.length) return;
  const select = $("#sub-select");
  select.replaceChildren(...subs.map((s) => el("option", { value: s.id, text: `${s.name} · ${s.provider_label}${s.disabled ? " (disabled)" : ""}` })));
  // The link in a "sign-in required" error names the subscription; otherwise
  // pick one that is signing in or known to need it.
  const wanted = new URLSearchParams(location.search).get("subscription");
  const pick = subs.find((s) => s.id === wanted)
    || subs.find((s) => activeStates.has(s.login?.state))
    || subs.find((s) => !s.disabled && s.auth?.state === "login_required")
    || subs[0];
  select.value = pick.id;
  await selectSubscription(pick.id);
}

function setupLogin() {
  $("#sub-select").addEventListener("change", (e) => selectSubscription(e.target.value));
  $("[data-action=start-login]").addEventListener("click", (e) => withBusy(e.currentTarget, startLogin));
  $("[data-action=restart-login]").addEventListener("click", (e) => withBusy(e.currentTarget, startLogin));
  $("[data-action=check]").addEventListener("click", (e) => withBusy(e.currentTarget, () => runCheck(true, { autoStart: false })));

  $("#code-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#flow-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        await api("POST", `${subPath(current.id)}/login/code`, { code: $("#code").value });
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

  // Coming back from the sign-in tab: put the cursor in the code box, or
  // check right away whether the browser approval went through.
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState !== "visible") return;
    if (lastFlowState === "waiting_for_code" && !$("#code").value) $("#code").focus();
    if (lastFlowState === "waiting_for_browser") refreshLogin();
  });

  $("[data-action=cancel-login]").addEventListener("click", async () => {
    clearTimeout(pollTimer);
    if (current) await api("DELETE", `${subPath(current.id)}/login`).catch(() => {});
    lastFlowState = "idle";
    $("#flow").hidden = true;
    setAuth("login_required", "Sign-in cancelled.");
  });

  $("[data-action=clear-token]").addEventListener("click", async (e) => {
    await withBusy(e.currentTarget, async () => {
      await api("DELETE", `${subPath(current.id)}/credential`);
      await runCheck(true, { autoStart: false });
    });
  });

  $("#token-form").addEventListener("submit", async (e) => {
    e.preventDefault();
    $("#token-error").textContent = "";
    await withBusy(e.submitter, async () => {
      try {
        await api("PUT", `${subPath(current.id)}/credential`, { credential: $("#token").value });
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
    loadSubscriptions().then(loadKeys).catch(() => {});
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
