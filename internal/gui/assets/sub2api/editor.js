import { editors, states, accounts, accountOf, monitorOf, configOf, currentProvider, keyBinding, keyName, sameOrigin, keySiteMatches,
  writable, readOnlyReason, browserAvailable, browserReason, button, message, request, read, reloadProviders, failure, picker, logout } from "./common.js";

function mountBalance(host, slot, p, custom, name) {
  // Saved account tokens and managed Sub2API sessions share the balance editor.
  const saved = !!p?.balanceToken?.set;
  const tok = input(draft.balanceToken || "", saved && !draft.clearBalanceToken ? t("saved · paste a new one to replace it") : t("optional · the account's system access token"), "password");
  tok.oninput = () => { draft.balanceToken = tok.value.trim(); };
  tok.onkeydown = (e) => { e.stopPropagation(); if (e.key === "Escape") cancelEdit(); };
  const pair = el("div", "pair");
  pair.append(tok);
  let fix;
  if (saved && !draft.clearBalanceToken) {
    const side = el("div", "side");
    const drop = el("button", "text", t("Remove"));
    drop.onclick = () => { draft.clearBalanceToken = true; draft.balanceToken = ""; tok.value = ""; tok.placeholder = t("optional · the account's system access token"); drop.remove(); fix?.refresh(); };
    side.append(drop);
    pair.append(side);
  }
  const help = custom || p.balanceURL
    ? t("What the Balance URL is asked with in place of the key, when it wants the account's own token: a new-api relay's System Access Token, or a sub2api panel's login token (a JWT, sent as a Bearer); it is used for nothing else.")
    : t("A key tells only what is left on itself. For the whole account's balance on the Usage page, generate a System Access Token in {p}'s settings and paste it here; it is used for nothing else.", { p: name });
  const [label, wrap] = field(t("Account balance"), pair, help);
  if (custom) {
    fix = balanceFix(p);
    wrap.append(fix);
    host.addEventListener("input", () => fix.refresh());
  }
  slot.replaceWith(label, wrap);
}

// Key-only balance endpoints reject account tokens; leave unrelated URLs alone.
function balanceFix(p) {
  const box = el("div", "bal-fix");
  const origin = (u) => { try { const x = new URL(u); return /^https?:$/.test(x.protocol) ? x.origin : ""; } catch { return ""; } };
  box.refresh = () => {
    box.replaceChildren();
    box.className = "bal-fix";
    if (currentProvider(p?.id)?.sub2api) return;
    const tok = !!draft.balanceToken || (!!p?.balanceToken?.set && !draft.clearBalanceToken);
    if (!tok) return;
    const u = (draft.balanceURL || "").trim();
    let path = "";
    try { path = new URL(u).pathname.replace(/\/+$/, ""); } catch {}
    let why = "";
    if (path === "/api/usage/token") why = t("…/api/usage/token takes the API key, not this token: for the account's balance, new-api uses /api/user/self; sub2api uses /api/v1/user/profile.");
    else if (path === "/v1/usage") why = t("…/v1/usage takes the API key, not this token: for the account's balance, a sub2api panel uses /api/v1/user/profile.");
    else if (!u) why = t("The token needs a Balance URL: a new-api relay uses /api/user/self; a sub2api panel uses /api/v1/user/profile to read the account's balance.");
    if (why) {
      box.classList.add("warn");
      box.append(el("span", "", why));
      const site = origin(u) || origin(draft.chat || draft.anthropic || draft.responses || "");
      if (!site) return;
      for (const to of (path === "/v1/usage" ? ["/api/v1/user/profile"] : ["/api/user/self", "/api/v1/user/profile"]).map((q) => site + q)) {
        const use = el("button", "text action", t("Use {url}", { url: to }));
        use.onclick = () => {
          draft.balanceURL = to;
          // Preserve an unrelated expression when changing the endpoint.
          if (!(draft.balancePath || "").trim() || /total_(available|granted|used)|unlimited_quota|^\s*\$?remaining\s*$/.test(draft.balancePath)) draft.balancePath = balanceFieldOf(to);
          const ed = box.closest(".editor");
          const bal = ed?.querySelector(".bal-url");
          const field = ed?.querySelector(".bal-path");
          if (bal) bal.value = draft.balanceURL;
          if (field) { field.value = draft.balancePath; field.placeholder = balanceFieldOf(draft.balanceURL); }
          if (bal) shown(bal);
          box.refresh();
        };
        box.append(use);
      }
      return;
    }
    if (path === "/api/user/self" && !(draft.headers || []).some((h) => (h[0] || "").trim().toLowerCase() === "new-api-user" && (h[1] || "").trim())) {
      box.append(el("span", "", t("A new-api relay also wants the header New-Api-User = your user ID (shown in the site's personal settings): add it under Headers.")));
    }
  };
  box.refresh();
  return box;
}

function panelBase(p) {
  try {
    const url = new URL(p?.balanceURL || p?.chat || p?.responses || p?.anthropic || p?.gemini || "");
    return url.origin + url.pathname.replace(/\/(?:api\/v1|v1)(?:\/.*)?\/?$/, "").replace(/\/+$/, "");
  } catch { return ""; }
}
function dirty() {
  if (typeof providerDirty === "function" && providerDirty()) {
    status(t("Save your provider changes before changing its Sub2API account."), "warn");
    return true;
  }
  return false;
}
function signInForm(body, state, { provider, account, note, work, blocked, finished, cancelled, redraw, baseChanged }) {
  const form = el("div", "s2-fields"), actions = el("div", "s2-actions");
  const line = (name, control) => form.append(...field(t(name), control));
  const target = account
    ? { session: account.id, expectedGeneration: state.generation }
    : { provider: provider.id, expectedRevision: provider.sub2apiRevision };
  const authenticate = async (path, input) => {
    state.loginController?.abort();
    const controller = new AbortController();
    state.loginController = controller;
    try {
      const result = await request(path, input, controller.signal);
      if (controller.signal.aborted || state.loginController !== controller) {
        if (result?.attempt) await request("cancel", { attempt: result.attempt });
        return;
      }
      await finished(result, controller.signal);
    } catch (error) {
      if (!controller.signal.aborted) throw error;
    } finally {
      if (state.loginController === controller) state.loginController = null;
    }
  };
  if (state.browserController) {
    form.append(el("p", "hint", t("Complete sign-in in the verification window.")));
    const cancel = button("Cancel", () => work(cancel, async () => { await cancelAttempt(state); await cancelled(); }));
    actions.append(cancel);
    body.append(form, actions);
    return;
  }
  if (state.attempt) {
    const code = input("", t("Six-digit verification code"));
    code.inputMode = "numeric";
    code.maxLength = 6;
    code.autocomplete = "one-time-code";
    code.setAttribute("aria-label", t("Six-digit verification code"));
    line("Two-factor verification", code);
    const verify = button("Verify code", () => work(verify, async () => {
      if (blocked?.()) return;
      if (!/^\d{6}$/.test(code.value.trim())) { message(note, t("Enter the six-digit verification code."), true); code.focus({ preventScroll: true }); return; }
      const verification = code.value.trim();
      code.value = "";
      await authenticate(account ? "2fa-account" : "2fa", {
        ...(account ? target : { provider: provider.id }), attempt: state.attempt, code: verification,
      });
    }), "primary");
    const cancel = button("Cancel", () => work(cancel, async () => { await cancelAttempt(state); await cancelled(); }));
    code.onkeydown = (e) => { e.stopPropagation(); if (e.key === "Enter") verify.click(); else if (e.key === "Escape") cancel.click(); };
    actions.append(verify, cancel);
  } else {
    const base = account ? null : input(state.base, "https://…", "url");
    const email = input(state.email || account?.email || "", t("Panel email"), "email");
    const password = input("", t("Panel password"), "password");
    const [remember, cb] = tick(t("Remember this password on this computer"), state.remember);
    if (base) { base.oninput = () => { state.base = base.value; state.baseEdited = true; baseChanged?.(); }; line("Panel address", base); }
    email.oninput = () => { state.email = email.value; };
    cb.onchange = () => { state.remember = cb.checked; };
    line("Panel email", email); line("Panel password", password);
    form.append(el("span"), remember);
    const login = button("Sign in", () => work(login, async () => {
      if (blocked?.()) return;
      if ((!account && !base.value.trim()) || !email.value.trim() || !password.value) {
        message(note, t("Enter the panel address, email and password."), true); return;
      }
      const secret = password.value;
      password.value = "";
      await authenticate(account ? "login-account" : "login", {
        ...target, ...(!account && { base: base.value.trim() }), email: email.value.trim(), password: secret, remember: cb.checked,
      });
    }), "primary");
    password.onkeydown = (e) => { e.stopPropagation(); if (e.key === "Enter") login.click(); };
    actions.append(login);
    if (account) {
      const cancel = button("Cancel", () => work(cancel, cancelled));
      actions.append(cancel);
    }
  }
  const browser = button("Sign in with browser verification", () => work(browser, async () => {
    if (!browserAvailable() || blocked?.()) return;
    if (!account && !state.base?.trim()) {
      message(note, t("Enter the panel address."), true);
      return;
    }
    for (const control of form.querySelectorAll('input[type="password"], input[autocomplete="one-time-code"]')) control.value = "";
    const previous = cancelAttempt(state);
    const controller = new AbortController();
    state.browserController = controller;
    redraw();
    try {
      await previous;
      if (controller.signal.aborted || state.browserController !== controller) return;
      const result = await request(account ? "browser-account" : "browser", {
        ...target, ...(!account && { base: state.base.trim() }),
      }, controller.signal);
      if (controller.signal.aborted || state.browserController !== controller) return;
      await finished(result, controller.signal);
    } catch (error) {
      if (controller.signal.aborted) return;
      throw error;
    } finally {
      if (state.browserController === controller) {
        state.browserController = null;
        redraw();
      }
    }
  }));
  browser.disabled = !browserAvailable();
  if (browser.disabled) browser.title = browserReason();
  actions.append(browser);
  if (browser.disabled) body.append(el("p", "hint", browserReason()));
  body.append(form, actions);
}
const accountForms = new Map();
function accountSignIn(account, redraw) {
  let state = accountForms.get(account.id);
  // A successful sign-in can be newer than the account snapshot still on screen.
  if (state && state.generation < account.generation) {
    cancelAttempt(state).catch(() => {});
    accountForms.delete(account.id);
    state = null;
  }
  if (state?.host) {
    if (state.redrawPending) queueMicrotask(state.draw);
    return state.host;
  }
  if (!state) {
    state = { generation: account.generation, email: account.email, remember: !!account.remember };
    accountForms.set(account.id, state);
  }
  const body = el("div", "s2-editor-body"), note = el("div", "s2-message");
  state.host = body;
  const owned = () => accountForms.get(account.id) === state;
  const active = () => owned() && body.isConnected && !body.closest("[hidden]");
  const draw = () => {
    if (!active()) { state.redrawPending = owned(); return; }
    state.redrawPending = false;
    body.replaceChildren();
    signInForm(body, state, { account, note, redraw: draw,
      work: async (b, fn) => {
        b.disabled = true;
        try { await fn(); }
        catch (e) {
          if (!owned()) return;
          let reset = false;
          const text = await failure(e, () => { reset = true; });
          if (!owned()) return;
          if (reset) { accountForms.delete(account.id); redraw(); status(text, "err"); }
          else if (active()) message(note, text, true); else status(text, "err");
        }
        finally { if (owned()) b.disabled = false; }
      },
      cancelled: async () => {
        if (!owned()) return;
        await cancelAttempt(state);
        if (!owned()) return;
        accountForms.delete(account.id); redraw();
      },
      finished: async (result, signal) => {
        if (signal?.aborted || !owned()) { if (result?.attempt) await request("cancel", { attempt: result.attempt }); return; }
        state.attempt = result?.attempt || "";
        if (result?.state === "ready") {
          state.generation = result.account.generation;
          await reloadProviders();
          if (signal?.aborted || !owned() || accountOf(account.id)?.generation > state.generation) return;
          accountForms.delete(account.id);
          redraw(); status(t("Signed in"), "ok");
        } else if (result?.state === "two_factor") draw();
        else message(note, t(states[result?.state] || "Temporarily unavailable"), true);
      },
    });
    body.append(note);
  };
  state.draw = draw;
  queueMicrotask(draw);
  return body;
}
async function cancelAccountForms() {
  const states = [...accountForms.values()];
  accountForms.clear();
  await Promise.all(states.map((s) => cancelAttempt(s).catch(() => {})));
}
function mountEditor(host, provider, balance, custom, name) {
  if (balance) mountBalance(host, balance, provider, custom, name);
  if (!provider || (!custom && !provider.sub2api)) return;
  const key = provider?.id || "";
  const state = editors.get(key) || { base: panelBase(provider), email: "", remember: false };
  const top = state.scrollTop;
  delete state.scrollTop;
  state.host = host;
  editors.set(key, state);
  const attach = (fields) => {
    const scrolling = host.querySelector(":scope > .ebody");
    if (scrolling) scrolling.append(...fields);
    else for (const node of fields) host.insertBefore(node, host.querySelector(":scope > .bar"));
  };
  // Keep pending password and verification inputs through a provider refresh.
  if (state.fields) {
    attach(state.fields);
    if (state.redrawPending) state.draw();
    const scrolling = host.querySelector(":scope > .ebody");
    if (scrolling && top !== undefined) scrolling.scrollTop = top;
    return;
  }
  const owned = () => editors.get(key) === state;
  const active = () => owned() && state.host.isConnected;
  const details = el("details", "s2-editor");
  details.open = !!provider?.sub2api || !!state.attempt;
  const summary = el("summary", "", t("Sub2API panel"));
  const body = el("div", "s2-editor-body");
  details.append(summary, body);
  const fields = field(t("Sub2API account"), details);
  state.fields = fields;
  attach(fields);
  let loaded = false;
  const refresh = async () => {
    try { await read(); loaded = true; draw(); }
    catch (e) { if (owned()) body.replaceChildren(el("p", "s2-error", e.message)); }
  };
  const fail = async (error) => {
    if (!owned()) return;
    if (error.status === 409 && state.attempt) { state.attempt = ""; state.login = true; }
    const text = await failure(error, draw);
    if (!owned()) return;
    const note = !state.redrawPending && body.querySelector(".s2-message");
    if (note) message(note, text, true); else status(text, "err");
  };
  const draw = () => {
    if (!active()) { state.redrawPending = owned(); return; }
    state.redrawPending = false;
    body.replaceChildren();
    if (!loaded) { body.append(el("p", "hint", t("Loading…"))); return; }
    if (!writable()) {
      body.append(el("p", "hint", readOnlyReason()));
      return;
    }
    const p = currentProvider(provider.id) || provider;
    const binding = p.sub2api;
    const account = accountOf(binding?.session);
    if (account && !state.baseEdited) state.base = account.base;
    if (account && !state.email) state.email = account.email;
    const note = el("div", "s2-message");
    if (state.linkError) message(note, t("Signed in; account linking failed: {error}", { error: state.linkError }), true);
    const work = async (b, fn) => {
      b.disabled = true;
      try { await fn(); } catch (e) { await fail(e); }
      finally { if (owned()) b.disabled = false; }
    };
    if (state.browserController) {
      signInForm(body, state, { provider: p, note, work, blocked: dirty, finished, cancelled: draw, redraw: draw });
      body.append(note);
      return;
    }
    if (binding) {
      const info = el("div", "s2-account-line");
      const label = el("span", "s2-identity", account?.email || account?.base || binding.session);
      label.title = account?.base || "";
      info.append(label, el("span", "s2-state" + (account?.state === "ready" ? " ready" : ""), t(states[account?.state] || "Sign in again")));
      body.append(info);
      if (account?.lastError) body.append(el("div", "s2-error", account.lastError));
      const actions = el("div", "s2-actions");
      const unlink = button("Unlink provider", () => work(unlink, async () => {
        if (dirty()) return;
        await request("unbind", { provider: p.id, expectedVersion: binding.version, expectedRevision: p.sub2apiRevision });
        await reloadProviders(); draw();
      }));
      actions.append(button("Sub2API groups", async () => { await cancelEditor(key); usageTab = "sub2api"; await show("usage"); }), unlink);
      if (account) {
        const out = button("Sign out", () => work(out, async () => { if (!dirty() && await logout(account)) draw(); }));
        actions.append(out);
      }
      const again = button("Sign in again", () => work(again, async () => {
        await cancelAttempt(state);
        if (owned()) { state.login = true; draw(); }
      }));
      actions.append(again);
      body.append(actions);
    }
    let refreshAccounts;
    if (state.attempt || !binding || state.login) {
      signInForm(body, state, { provider: p, note, work, blocked: dirty, finished, cancelled: draw, redraw: draw,
        baseChanged: () => { closeProtoMenu(); refreshAccounts?.(); } });
      if (state.attempt || state.browserController) { body.append(note); return; }
    }
    if (loaded && accounts().length && !state.login) {
      const link = el("div", "s2-actions");
      const eligible = () => accounts().filter((a) => a.state !== "logged_out" &&
        sameOrigin(state.base, a.base) &&
        sameOrigin(p.balanceURL || p.chat || p.responses || p.anthropic || p.gemini, a.base));
      refreshAccounts = () => {
        const matches = eligible(), candidate = state.session || binding?.session || "";
        const selectedAccount = matches.find((a) => a.id === candidate);
        const selected = state.session = selectedAccount?.id || "";
        const choices = matches.map((a) => ({
          v: a.id, name: a.email || a.subject || a.id, note: a.base, literalName: true,
        }));
        link.replaceChildren(picker("Signed-in accounts", choices, selected, (id) => { state.session = id; refreshAccounts(); }));
        const use = button("Link account", () => work(use, async () => {
          if (!active() || dirty() || !selected || state.session !== selected ||
            selected === binding?.session || !eligible().some((a) => a.id === selected)) return;
          await request("bind", { provider: p.id, session: selected, groupId: "", groupName: "",
            keyIds: [], expectedVersion: binding?.version || "", expectedRevision: p.sub2apiRevision,
            expectedGeneration: selectedAccount.generation });
          state.linkError = "";
          await reloadProviders(); draw();
        }));
        use.disabled = !selected || selected === binding?.session;
        link.append(use);
      };
      refreshAccounts();
      body.append(link);
    }
    if (binding && account) {
      const config = configOf(account.id), monitored = monitorOf(account.id);
      const matches = (sample) => sample?.sessionId === account.id &&
        sample.generation === account.generation && sample.settingsRevision === config.revision;
      if (state.groupQuery && !matches(state.groupQuery)) {
        state.groupQuery.controller.abort();
        state.groupQuery = null;
      }
      const sampled = (sample) => Date.parse(sample?.groupsLastSuccess || sample?.lastSuccess) || 0;
      if (state.groupsPreview && (!matches(state.groupsPreview) ||
        matches(monitored) && sampled(monitored) > sampled(state.groupsPreview))) state.groupsPreview = null;
      const mon = state.groupsPreview || monitored;
      const options = [{ v: "", name: t("No group selected"), note: "" },
        ...(mon?.groups || []).filter((g) => g.visible !== false).map((g) => ({ v: g.id, name: g.name || g.id, note: g.id, literalName: true }))];
      const groups = el("div", "s2-actions");
      const query = button("Query once", () => work(query, async () => {
        if (dirty() || state.groupQuery) return;
        const pending = { sessionId: account.id, generation: account.generation,
          settingsRevision: config.revision, controller: new AbortController() };
        state.groupQuery = pending;
        const current = () => owned() && state.groupQuery === pending &&
          currentProvider(p.id)?.sub2api?.session === account.id &&
          accountOf(account.id)?.generation === pending.generation &&
          configOf(account.id).revision === pending.settingsRevision;
        try {
          const result = await request("preview", { session: account.id,
            expectedGeneration: pending.generation, settingsRevision: pending.settingsRevision }, pending.controller.signal);
          if (current() && !pending.controller.signal.aborted) state.groupsPreview = result;
        } catch (error) {
          if (current() && !pending.controller.signal.aborted) throw error;
        } finally {
          if (state.groupQuery === pending) {
            state.groupQuery = null;
            if (owned()) draw();
          }
        }
      }));
      query.disabled = !!state.groupQuery;
      groups.append(query);
      body.append(groups);
      if (keySiteMatches(p, account)) for (const key of p.keyList || []) {
        const selected = keyBinding(p, key.id), choices = [...options];
        if (selected?.groupId && !choices.some((g) => g.v === selected.groupId)) {
          choices.push({ v: selected.groupId, name: selected.groupName || selected.groupId, note: selected.groupId, literalName: true });
        }
        const row = el("div", "s2-actions");
        row.dataset.key = key.id;
        row.append(el("span", "s2-identity", keyName(key)), picker("Panel group", choices, selected?.groupId || "", async (id) => {
          if (dirty()) return;
          const group = choices.find((g) => g.v === id);
          try {
            await request("bind-key", { provider: p.id, session: binding.session, keyId: key.id,
              groupId: id, groupName: id ? group.name : "", expectedVersion: binding.version,
              expectedRevision: p.sub2apiRevision, expectedGeneration: account.generation });
            await reloadProviders(); draw();
          } catch (e) { await fail(e); }
        }));
        body.append(row);
      }
      body.append(el("p", "hint", t("Local group binding; the panel has not verified which group each key belongs to.")));
      if (mon?.groupsError) body.append(el("p", "s2-error", t("Groups: {error}", { error: mon.groupsError })));
      else if (mon?.groupsKnown && options.length === 1) body.append(el("p", "hint", t("No visible groups yet")));
    }
    body.append(note);
    async function finished(result, signal) {
      if (signal?.aborted || !owned()) {
        if (result?.attempt) await request("cancel", { attempt: result.attempt });
        return;
      }
      state.attempt = result?.attempt || "";
      if (result?.state === "ready") {
        state.login = false;
        state.session = result.account?.id || "";
        state.linkError = result.binding?.linked === false ? result.binding.error : "";
        await reloadProviders();
        if (signal?.aborted || !owned()) return;
        draw();
        status(state.linkError ? t("Signed in; account linking failed: {error}", { error: state.linkError }) : t("Signed in"),
          state.linkError ? "warn" : "ok");
      } else if (result?.state === "two_factor") draw();
      else message(note, t(states[result?.state] || "Temporarily unavailable"), true);
    }
  };
  state.draw = draw;
  details.ontoggle = () => { if (details.open && !loaded) refresh(); };
  draw();
  if (details.open) refresh();
}
async function cancelAttempt(state) {
  state.loginController?.abort();
  state.loginController = null;
  state.browserController?.abort();
  state.browserController = null;
  const attempt = state.attempt;
  state.attempt = "";
  if (attempt) await request("cancel", { attempt });
}
async function cancelEditor(id) {
  const selected = id === undefined ? [...editors.entries()] : [[id, editors.get(id)]];
  await Promise.all(selected.filter(([, s]) => s).map(async ([key, state]) => {
    editors.delete(key);
    state.groupQuery?.controller.abort();
    try { await cancelAttempt(state); } catch {}
  }));
}
export { mountEditor, cancelEditor, accountSignIn, cancelAccountForms };
