"use strict";

// Set when running as a Home Assistant add-on behind Ingress.
let ingress = false;

const $ = (sel, root = document) => root.querySelector(sel);
const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));

let currentTab = "status";
let pollTimer = null;
let snapURL = null;
let snapTried = false;

async function api(method, path, body) {
  const opts = { method, headers: { "X-Requested-With": "rpb" }, credentials: "same-origin" };
  if (body !== undefined) {
    opts.headers["Content-Type"] = "application/json";
    opts.body = JSON.stringify(body);
  }
  const res = await fetch(path, opts);
  let data = null;
  if ((res.headers.get("Content-Type") || "").includes("application/json")) data = await res.json();
  if (res.status === 401 && path !== "api/login") {
    showView("login");
    throw new Error("not logged in");
  }
  if (!res.ok) throw new Error((data && data.error) || res.statusText);
  return data;
}

function el(tag, attrs = {}, ...children) {
  const e = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (k === "class") e.className = v;
    else if (k.startsWith("on")) e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v);
  }
  for (const c of children) e.append(c);
  return e;
}

function setText(id, text, cls) {
  const e = document.getElementById(id);
  e.textContent = text;
  e.className = (e.classList.contains("mono") ? "mono " : "") + (cls || "");
}

function since(t) {
  if (!t) return "";
  const s = Math.max(0, Math.round((Date.now() - new Date(t).getTime()) / 1000));
  if (s < 60) return s + " s";
  if (s < 3600) return Math.round(s / 60) + " min";
  if (s < 86400) return Math.round(s / 3600) + " h";
  return Math.round(s / 86400) + " d";
}

function formError(form, msg) {
  $("[data-error]", form).textContent = msg || "";
}

function flashOK(form, msg) {
  const ok = $("[data-ok]", form);
  ok.textContent = msg;
  setTimeout(() => { if (ok.textContent === msg) ok.textContent = ""; }, 4000);
}

async function busy(button, fn) {
  button.disabled = true;
  try { return await fn(); } finally { button.disabled = false; }
}

// --- views and tabs ---

function showView(name) {
  const loggedIn = !["setup", "login"].includes(name);
  $("#tabs").hidden = !loggedIn;
  $("#logout").hidden = !loggedIn || ingress; // Home Assistant handles logins
  for (const v of ["setup", "login", "status", "connections", "options"]) {
    $("#view-" + v).hidden = v !== name;
  }
  for (const b of $$("#tabs button")) b.classList.toggle("active", b.dataset.tab === name);
  if (loggedIn) currentTab = name;
  clearInterval(pollTimer);
  pollTimer = null;
  if (name === "status") {
    refreshStatus();
    pollTimer = setInterval(refreshStatus, 3000);
  } else if (name === "connections" || name === "options") {
    loadConfig();
  }
  const first = $("#view-" + name + " input");
  if (!loggedIn && first) first.focus();
}

$$("#tabs button").forEach(b => b.addEventListener("click", () => {
  location.hash = b.dataset.tab;
  showView(b.dataset.tab);
}));

$("#logout").addEventListener("click", async () => {
  try { await api("POST", "api/logout"); } catch (_) { /* ignore */ }
  showView("login");
});

async function init() {
  let s;
  try {
    s = await api("GET", "api/session");
    ingress = !!s.ingress;
  } catch (e) {
    document.body.textContent = "Cannot reach the bridge: " + e.message;
    return;
  }
  if (s.setup) return showView("setup");
  if (!s.authenticated) return showView("login");
  const tab = location.hash.slice(1);
  showView(["status", "connections", "options"].includes(tab) ? tab : "status");
}

// --- setup and login ---

$("#setup-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = ev.target;
  const pw = f.password.value;
  if (pw.length < 8) return formError(f, "The password needs at least 8 characters.");
  if (pw !== f.password2.value) return formError(f, "The passwords do not match.");
  formError(f, "");
  await busy($("button", f), async () => {
    try {
      await api("POST", "api/setup", { password: pw });
      f.reset();
      showView("connections");
    } catch (e) { formError(f, e.message); }
  });
});

$("#login-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  await busy($("button", f), async () => {
    try {
      await api("POST", "api/login", { password: f.password.value });
      f.reset();
      const tab = location.hash.slice(1);
      showView(["status", "connections", "options"].includes(tab) ? tab : "status");
    } catch (e) { formError(f, e.message); }
  });
});

// --- status ---

function streamText(s) {
  if (!s) return ["–", ""];
  if (!s.connected) return ["Disconnected" + (s.error ? ": " + s.error : ""), "bad"];
  let t = "Connected";
  if (s.codec) t += ` · ${s.codec === "h265" ? "H.265" : "H.264"}`;
  if (s.width) t += ` · ${s.width}×${s.height}`;
  if (s.fps) t += ` · ${s.fps} fps`;
  t += s.audio ? " · audio" : " · no audio";
  if (s.since) t += ` · ${since(s.since)}`;
  return [t, "good"];
}

async function refreshStatus() {
  let st;
  try { st = await api("GET", "api/status"); } catch (_) { return; }

  const missing = $("#missing");
  missing.hidden = st.configured;
  if (!st.configured) {
    missing.textContent = "The bridge is not running. Missing: " + st.missing.join(", ") + ". Fill these in under Connections.";
  }
  const se = $("#start-error");
  se.hidden = !st.error;
  se.textContent = st.error ? "The bridge could not start: " + st.error : "";

  const b = st.bridge;
  setText("st-main", ...streamText(b && b.doorbell.main));
  setText("st-sub", ...streamText(b && b.doorbell.sub));
  if (b) {
    setText("st-events", b.doorbell.events ? "Connected" : "Not connected", b.doorbell.events ? "good" : "bad");
    const p = b.protect;
    setText("st-protect", p.connected ? "Connected" + (p.since ? " · " + since(p.since) : "") : "Not connected", p.connected ? "good" : "bad");
    setText("st-controller", p.controller || "–");
    setText("st-perror", p.error || "–", p.error ? "bad" : "");
    setText("st-talk", p.talkback ? "Active" : "Idle");
    setText("st-channels", b.channels && b.channels.length ? b.channels.join(", ") : "None");
  } else {
    for (const id of ["st-events", "st-protect", "st-controller", "st-perror", "st-talk", "st-channels"]) setText(id, "–");
  }
  setText("st-name", st.name);
  setText("st-model", st.model);
  setText("st-mac", st.mac.replace(/(..)(?!$)/g, "$1:"));
  setText("st-version", st.version);

  const dot = $("#brand-dot");
  dot.className = "dot " + (!b ? "bad" : b.protect.connected && b.doorbell.main.connected ? "ok" : "warn");

  const ul = $("#st-recent");
  ul.replaceChildren();
  const recent = (b && b.recent) || [];
  if (!recent.length) ul.append(el("li", { class: "muted" }, "None yet."));
  for (const ev of recent.slice().reverse()) {
    ul.append(el("li", {}, el("span", {}, ev.kind), el("span", { class: "muted" }, new Date(ev.time).toLocaleString())));
  }

  if ($("#log-follow").checked) refreshLogs();
  if (b && !snapTried) { snapTried = true; refreshSnapshot(); }
}

async function refreshLogs() {
  try {
    const d = await api("GET", "api/logs?n=200");
    const pre = $("#log");
    const atBottom = pre.scrollTop + pre.clientHeight >= pre.scrollHeight - 20;
    pre.textContent = d.lines.join("\n");
    if (atBottom) pre.scrollTop = pre.scrollHeight;
  } catch (_) { /* ignore */ }
}

async function refreshSnapshot() {
  const btn = $("#snap-refresh");
  const msg = $("#snap-msg");
  await busy(btn, async () => {
    msg.textContent = "Loading…";
    try {
      const res = await fetch("api/snapshot.jpg?t=" + Date.now(), { credentials: "same-origin" });
      if (!res.ok) {
        let err = res.statusText;
        try { err = (await res.json()).error; } catch (_) { /* not JSON */ }
        throw new Error(err);
      }
      const blob = await res.blob();
      if (snapURL) URL.revokeObjectURL(snapURL);
      snapURL = URL.createObjectURL(blob);
      $("#snap").src = snapURL;
      $("#snap").hidden = false;
      msg.textContent = "";
      msg.hidden = true;
    } catch (e) {
      msg.hidden = false;
      msg.textContent = "No snapshot: " + e.message;
    }
  });
}
$("#snap-refresh").addEventListener("click", refreshSnapshot);

// --- config ---

let config = null;

async function loadConfig() {
  try { config = await api("GET", "api/config"); } catch (_) { return; }
  const c = $("#conn-form");
  c["d.host"].value = config.doorbell.host;
  c["d.port"].value = config.doorbell.port || "";
  c["d.username"].value = config.doorbell.username;
  c["d.channel"].value = config.doorbell.channel || "";
  c["p.host"].value = config.protect.host;
  c["p.username"].value = config.protect.username;
  setSecret(c, "d.password", config.doorbell.password.set);
  setSecret(c, "p.password", config.protect.password.set);
  setSecret(c, "p.token", config.protect.token.set);

  const o = $("#opt-form");
  o.cname.value = config.camera.name;
  o.ring.value = config.camera.ring;
  o.events.checked = config.camera.events;
  o.talkback.checked = config.camera.talkback;
  o.settings.checked = config.camera.settings;
  $("#opt-mac").textContent = config.identity.mac.replace(/(..)(?!$)/g, "$1:");

  const a = $("#adv-form");
  a.listen.value = config.advanced.listen;
  a.ffmpeg.value = config.advanced.ffmpeg;
  // in Home Assistant the page is only reached through Ingress
  $("#pw-form").hidden = config.ingress;
  a.listen.disabled = config.ingress;
}

// secrets are write-only: the field stays empty and a hint says one is stored
function setSecret(form, name, isSet) {
  const input = form[name];
  input.value = "";
  input.dataset.clear = "";
  input.placeholder = isSet ? "•••••• (stored)" : "";
  const hint = $(`[data-hint="${name}"]`, form);
  hint.replaceChildren();
  if (!isSet) return;
  hint.append("Stored. Leave empty to keep it. ");
  if (name !== "d.password") {
    hint.append(el("button", {
      type: "button", class: "link", onclick: ev => {
        input.dataset.clear = "1";
        input.placeholder = "";
        ev.target.parentElement.textContent = "Will be removed when you save.";
      },
    }, "Remove"));
  }
}

$("#conn-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  const body = {
    doorbell: {
      host: f["d.host"].value.trim(),
      port: parseInt(f["d.port"].value, 10) || 0,
      username: f["d.username"].value.trim(),
      password: f["d.password"].value,
      channel: parseInt(f["d.channel"].value, 10) || 0,
    },
    protect: {
      host: f["p.host"].value.trim(),
      username: f["p.username"].value.trim(),
      password: f["p.password"].value,
      token: f["p.token"].value.trim(),
      clearPassword: f["p.password"].dataset.clear === "1",
      clearToken: f["p.token"].dataset.clear === "1",
    },
  };
  await busy($("button[type=submit]", f), async () => {
    try {
      await api("PUT", "api/config", body);
      await loadConfig();
      flashOK(f, "Saved. The bridge restarts.");
    } catch (e) { formError(f, e.message); }
  });
});

$("#opt-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  const body = {
    camera: {
      name: f.cname.value.trim(),
      ring: f.ring.value,
      events: f.events.checked,
      talkback: f.talkback.checked,
      settings: f.settings.checked,
    },
  };
  await busy($("button[type=submit]", f), async () => {
    try {
      await api("PUT", "api/config", body);
      await loadConfig();
      flashOK(f, "Saved. The bridge restarts.");
    } catch (e) { formError(f, e.message); }
  });
});

$("#pw-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  if (f.password.value !== f.password2.value) return formError(f, "The new passwords do not match.");
  await busy($("button[type=submit]", f), async () => {
    try {
      await api("POST", "api/password", { current: f.current.value, password: f.password.value });
      f.reset();
      flashOK(f, "Password changed. Other sessions were logged out.");
    } catch (e) { formError(f, e.message); }
  });
});

$("#adv-form").addEventListener("submit", async ev => {
  ev.preventDefault();
  const f = ev.target;
  formError(f, "");
  const body = { advanced: { listen: f.listen.value.trim(), ffmpeg: f.ffmpeg.value.trim() } };
  await busy($("button[type=submit]", f), async () => {
    try {
      await api("PUT", "api/config", body);
      await loadConfig();
      flashOK(f, "Saved.");
    } catch (e) { formError(f, e.message); }
  });
});

$("#new-identity").addEventListener("click", async ev => {
  const ok = confirm(
    "Create a new camera identity?\n\n" +
    "Protect will see a brand-new camera: you must adopt it again in Protect, " +
    "and the old camera stays there as offline (delete it in Protect). " +
    "Recordings and settings of the old camera are not carried over.");
  if (!ok) return;
  $("#id-error").textContent = "";
  await busy(ev.target, async () => {
    try {
      await api("POST", "api/identity");
      await loadConfig();
    } catch (e) { $("#id-error").textContent = e.message; }
  });
});

// --- discovery (only on click) ---

$$("[data-discover]").forEach(btn => btn.addEventListener("click", () => discover(btn)));

async function discover(btn) {
  const kind = btn.dataset.discover;
  const box = $("#found-" + kind);
  box.hidden = false;
  box.replaceChildren(el("span", { class: "muted" }, kind === "doorbell" ? "Scanning the local network…" : "Searching…"));
  await busy(btn, async () => {
    try {
      const d = await api("GET", "api/discover?kind=" + kind);
      const list = d.devices || [];
      box.replaceChildren();
      if (!list.length) {
        box.append(el("span", { class: "muted" }, "Nothing found. Enter the address manually."));
        return;
      }
      const field = kind === "doorbell" ? "d.host" : "p.host";
      for (const dev of list) {
        let label;
        if (kind === "doorbell") {
          label = `${dev.ip} · Reolink`;
        } else {
          label = [dev.ip, dev.name, dev.platform].filter(Boolean).join(" · ");
        }
        const b = el("button", {
          type: "button",
          class: kind === "protect" && !dev.console ? "other" : "",
          onclick: () => { $("#conn-form")[field].value = dev.ip; box.hidden = true; },
        }, label);
        box.append(b);
      }
      box.append(el("span", { class: "muted" }, "Click an entry to use it."));
    } catch (e) {
      box.replaceChildren(el("span", { class: "error" }, e.message));
    }
  });
}

init();
