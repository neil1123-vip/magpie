// Panel credentials stay in Go; these modules receive safe account and monitor views.
let data = null, flight = null;
const editors = new Map();
const states = {
  ready: "Signed in", two_factor: "Two-factor verification", challenge: "Waiting for verification",
  login_required: "Sign in again", temporary: "Temporarily unavailable",
  refresh_unknown: "Refresh outcome unknown", logged_out: "Signed out",
};
const accounts = () => data?.accounts || [];
const monitors = () => data?.monitor?.accounts || [];
const accountOf = (id) => accounts().find((a) => a.id === id);
const monitorOf = (id) => monitors().find((a) => a.sessionId === id);
const configOf = (id) => data?.configs?.[id] || { configured: false, enabled: false, groups: [], revision: "" };
const writable = () => data?.capability?.writable === true;
const readOnlyReason = () => t(data?.capability?.reason || "Sub2API changes require the Windows desktop app.");
const browserAvailable = () => writable() && data?.capability?.browser === true;
const browserReason = () => t(data?.capability?.browserReason || "Browser verification is unavailable in this build.");
const currentProvider = (id) => providers?.providers?.find((p) => p.id === id);
const keyBindings = (binding) => Array.isArray(binding?.keys) ? binding.keys :
  (binding?.keyIds || []).map((id) => ({ id, groupId: binding.groupId || "", groupName: binding.groupName || "" }));
const keyBinding = (p, id) => keyBindings(p?.sub2api).find((key) => key.id === id);
const keyName = (key) => key.name || key.masked || key.id;
const sameOrigin = (left, right) => {
  try {
    const site = new URL(left), source = new URL(right);
    return ["https:", "http:"].includes(site.protocol) && source.protocol === site.protocol && !site.username && !site.password &&
      !source.username && !source.password && source.origin === site.origin;
  } catch { return false; }
};
const keySiteMatches = (p, account) =>
  sameOrigin(p?.chat || p?.responses || p?.anthropic || p?.gemini, account?.base);
const button = (name, action, cls = "") => {
  const b = el("button", "text " + cls, t(name));
  b.type = "button";
  b.onclick = action;
  return b;
};
const message = (box, text, bad = false) => {
  box.textContent = text;
  box.classList.toggle("bad", bad);
  box.setAttribute("role", bad ? "alert" : "status");
};
async function request(path, body, signal) {
  let result;
  // Finished panel actions invalidate earlier reads, including on a conflict.
  try { result = await api("sub2api" + (path ? "/" + path : ""), body, undefined, signal); }
  catch (error) {
    if (error.message === "Sub2API session renewed; query again") error.message = t(error.message);
    throw error;
  } finally { if (path) flight = null; }
  if (result?.error) throw new Error(result.error);
  return result;
}
async function read() {
  if (flight) return flight;
  const pending = request("").then((next) => {
    if (flight !== pending) return read();
    data = next || {};
    return data;
  }, (error) => {
    if (flight !== pending) return read();
    throw error;
  });
  flight = pending;
  try { return await pending; } finally { if (flight === pending) flight = null; }
}
async function reloadProviders() {
  const previous = typeof editing === "string" && !providerDirty() ? draft : null;
  providers = await api("providers");
  const provider = previous && currentProvider(editing);
  if (provider && draft === previous && !providerDirty()) {
    const editor = editors.get(editing);
    if (editor) editor.scrollTop = editor.host.querySelector(":scope > .ebody")?.scrollTop;
    draft = draftOf(provider);
    renderProviders();
  }
  return read();
}
async function failure(error, redraw) {
  if (error.status === 409) {
    try { await reloadProviders(); redraw(); }
    catch (e) { return error.message + " " + e.message; }
  }
  return error.message;
}
function picker(name, options, value, choose) {
  const b = el("button", "sess-pick s2-picker");
  b.type = "button";
  const selected = options.find((o) => o.v === value);
  b.append(el("span", "", selected?.name || t(name)), svg(CHEV, 11, 1.6));
  b.setAttribute("aria-label", t(name));
  b.setAttribute("aria-expanded", "false");
  b.disabled = !options.length;
  b.onclick = (e) => {
    e.stopPropagation();
    if (b.classList.contains("open")) return closeProtoMenu();
    openProtoMenu(b, options, value, choose, name, "sess-menu", "left", false, options.length > 8 ? "Search" : "");
  };
  return b;
}
async function logout(account) {
  if (!writable()) return false;
  if (!await confirmAction(t("Sign out of {account}?", { account: account.email || account.base }),
    t("This signs out every provider sharing this panel account. API keys remain available."), t("Sign out"))) return false;
  const result = await request("logout", { session: account.id, expectedGeneration: account.generation });
  await reloadProviders();
  if (result?.cleanupPending) status(t("Signed out; local credential cleanup is pending.") +
    (result.cleanupError ? " " + result.cleanupError : ""), "warn");
  return true;
}
export { data, editors, states, accounts, accountOf, monitorOf, configOf, currentProvider, keyBindings, keyBinding, keyName, sameOrigin, keySiteMatches,
  writable, readOnlyReason, browserAvailable, browserReason, button, message, request, read, reloadProviders, failure, picker, logout };
