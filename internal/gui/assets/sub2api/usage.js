import { data, states, accounts, accountOf, monitorOf, configOf, currentProvider, keyBindings, keyBinding, keyName, keySiteMatches,
  writable, readOnlyReason, button, request, read, reloadProviders, failure, picker, logout } from "./common.js";
import { accountSignIn, cancelAccountForms } from "./editor.js";
let pane = null, site = "", changed = false, linkedOnly = true, renderedLang = "";
const signingIn = new Set(), previews = new Map(), queries = new Map();
let observedPane;
const visibility = new MutationObserver(() => {
  if (!pane?.closest("[hidden]")) return;
  signingIn.clear(); previews.clear(); queries.clear(); cancelAccountForms();
});
const sampleTime = (monitor) => Math.max(0, ...[
  monitor?.lastSuccess, monitor?.groupsLastSuccess, monitor?.billingLastSuccess,
  ...(monitor?.sources || []).flatMap((source) => (source.keys || []).map((key) => key.lastSuccess)),
].map((value) => Date.parse(value) || 0));
const displayedMonitor = (id) => {
  const preview = previews.get(id), monitored = monitorOf(id), account = accountOf(id), config = configOf(id);
  const previewAt = sampleTime(preview);
  if (preview && (preview.generation !== account?.generation || preview.settingsRevision !== config.revision ||
    config.enabled && monitored?.enabled && monitored.generation === account?.generation &&
    monitored.settingsRevision === config.revision && previewAt > 0 && sampleTime(monitored) > previewAt)) {
    previews.delete(id);
  }
  return previews.get(id) || monitored;
};
const changeNames = {
  group_added: "Group became visible", group_unavailable: "Group no longer visible",
  group_renamed: "Panel group renamed", group_rate: "Declared rate changed",
  group_enabled: "Group status changed", key_rate: "Key rate changed",
};
const number = (n) => typeof n === "number" && Number.isFinite(n);
const rate = (n) => number(n) ? n.toLocaleString(intlLang(), { maximumFractionDigits: 20 }) + "×" : t("Rate not confirmed");
const time = (s) => s && !String(s).startsWith("0001-") && Number.isFinite(Date.parse(s))
  ? new Date(s).toLocaleString(intlLang(), { hour12: false }) : t("No successful check");
const snapshotCurrent = (source, session) => {
  const p = currentProvider(source.id), binding = p?.sub2api, monitor = displayedMonitor(session);
  return !!binding && monitor?.generation === accountOf(session)?.generation &&
    monitor?.settingsRevision === configOf(session).revision && binding.session === session &&
    !!source.bindingVersion && source.bindingVersion === binding.version &&
    !!source.revision && source.revision === p.sub2apiRevision;
};
const keyCurrent = (source, key, session) => snapshotCurrent(source, session) &&
  !!keyBinding(currentProvider(source.id), key.id)?.groupId &&
  keyBinding(currentProvider(source.id), key.id).groupId ===
    (key.groupId ?? (!Array.isArray(currentProvider(source.id)?.sub2api?.keys) ? source.groupId : undefined));
function rateApplication(source, key, session) {
  const p = currentProvider(source.id), monitor = displayedMonitor(session), account = accountOf(session);
  const latest = monitor?.sources?.find((s) => s.id === source.id), lastKey = latest?.keys?.find((k) => k.id === key.id);
  const bound = keyBinding(p, key.id), configured = p?.keyList?.find((k) => k.id === key.id);
  if (!writable() || !snapshotCurrent(source, session) || !latest || !snapshotCurrent(latest, session) ||
    !keyCurrent(source, key, session) || !lastKey || !keyCurrent(latest, lastKey, session) ||
    !configured || configured.on === false || !latest.activeKeyIds?.includes(key.id) ||
    !account || account.state === "logged_out" ||
    monitor.generation !== account.generation || monitor.settingsRevision !== configOf(session).revision ||
    key.error || lastKey.error ||
    !number(key.rate) || key.rate <= 0 || key.rate > 1000 || lastKey.rate !== key.rate ||
    Math.abs(key.rate * 1000 - Math.round(key.rate * 1000)) > 1e-6) return null;
  if (key.rate === (bound.priceRate > 0 ? bound.priceRate : p.priceRate > 0 ? p.priceRate : 1)) return null;
  return {
    provider: p.id, keyId: key.id, session, groupId: bound.groupId,
    rate: key.rate, expectedRate: bound.priceRate > 0 ? bound.priceRate : p.priceRate > 0 ? p.priceRate : 1,
    bindingVersion: source.bindingVersion, expectedRevision: source.revision, expectedGeneration: account.generation,
    settingsRevision: monitor.settingsRevision,
  };
}
const historical = (value, at) => el("span", "s2-muted s2-historical",
  t("Last confirmed: {value} ({time})", { value, time: time(at) }));
async function setMonitor(account, enabled, groups, settingsRevision) {
  await request("monitor", { session: account.id, enabled, groups: groups.map((g) => ({ id: g.id, name: g.name })),
    expectedGeneration: account.generation, settingsRevision });
  previews.delete(account.id);
  const config = configOf(account.id);
  if (accountOf(account.id)?.generation === account.generation && config.revision === settingsRevision) {
    Object.assign(config, { enabled, groups });
  }
  try { await read(); paint(); }
  catch (e) { paint(); status(e.message, "err"); }
}
async function bindGroup(group, account, anchor) {
  if (!providers) providers = await api("providers");
  const options = (providers.providers || []).filter((p) => !p.account && p.key?.set &&
    keySiteMatches(p, account) && (!p.sub2api || p.sub2api.session === account.id)).flatMap((p) =>
    (p.keyList || []).map((key) => ({
      v: JSON.stringify([p.id, key.id]), name: p.name + " · " + keyName(key),
      selected: keyBinding(p, key.id)?.groupId === group.id,
      note: keyBinding(p, key.id)?.groupName || "", literalName: true,
    })));
  const selected = options.filter((option) => option.selected).map((option) => option.v);
  openProtoMenu(anchor, options, selected, async (values) => {
    closeProtoMenu();
    const value = options.find((option) => selected.includes(option.v) !== values.includes(option.v))?.v;
    if (!value) return;
    const [id, keyId] = JSON.parse(value), p = currentProvider(id);
    if (!p) return;
    const linked = values.includes(value);
    if (!linked && keyBinding(p, keyId)?.groupId !== group.id) { paint(); return; }
    anchor.disabled = true;
    try {
      await request("bind-key", { provider: id, session: account.id, keyId,
        groupId: linked ? group.id : "", groupName: linked ? group.name : "",
        expectedVersion: p.sub2api?.version || "", expectedRevision: p.sub2apiRevision, expectedGeneration: account.generation });
      await reloadProviders(); paint();
    } catch (e) { status(await failure(e, paint), "err"); }
    finally { if (anchor.isConnected) anchor.disabled = !writable() || !group.id; }
  }, "Link key", "sess-menu", "right", true, "Search");
}
async function queryAccount(account, anchor, source) {
  const previous = queries.get(account.id);
  if (previous?.anchor.isConnected) previous.anchor.disabled = !writable();
  const query = { host: pane, anchor };
  queries.set(account.id, query);
  const current = () => queries.get(account.id) === query && pane === query.host &&
    pane?.isConnected && !pane.closest("[hidden]");
  anchor.disabled = true;
  const config = configOf(account.id);
  try {
    const p = source && currentProvider(source.id), binding = p?.sub2api;
    if (source && (!binding || binding.session !== account.id)) throw new Error(t("The provider or monitored rate changed. Review it before applying."));
    const result = await request("preview", { session: account.id, expectedGeneration: account.generation,
      settingsRevision: config.revision, ...(source && { provider: p.id,
        expectedRevision: p.sub2apiRevision, bindingVersion: binding.version }) });
    if (!current()) return;
    previews.set(account.id, result);
    paint();
  } catch (e) {
    if (!current()) return;
    const text = await failure(e, () => { if (current()) paint(); });
    if (current()) status(text, "err");
  } finally {
    if (current() && anchor.isConnected) anchor.disabled = !writable();
    if (queries.get(account.id) === query) queries.delete(account.id);
  }
}
async function applyRate(source, key, session, anchor, expected) {
  const p = currentProvider(source.id);
  const unchanged = () => expected && JSON.stringify(rateApplication(source, key, session)) === JSON.stringify(expected);
  const reread = async () => {
    try { await reloadProviders(); paint(); } catch (e) { status(e.message, "err"); return; }
    status(t("The provider or monitored rate changed. Review it before applying."), "warn");
  };
  if (!unchanged()) return reread();
  if (!await confirmAction(t("Apply {rate} to {provider}?", { rate: rate(expected.rate), provider: p.name + " · " + keyName(p.keyList.find((k) => k.id === key.id)) }),
    t("Historical estimates may be recalculated at this rate. Prices set for individual models stay as set; recorded costs stay unchanged."), t("Apply rate"))) return;
  if (!unchanged()) return reread();
  anchor.disabled = true;
  try {
    const { provider, keyId, rate, expectedRate, bindingVersion, expectedRevision, expectedGeneration } = expected;
    await request("apply-key-rate", { provider, keyId, rate, expectedRate, bindingVersion, expectedRevision, expectedGeneration });
    await reloadProviders(); paint();
  } catch (e) { status(await failure(e, paint), "err"); }
  finally { if (anchor.isConnected) anchor.disabled = !rateApplication(source, key, session); }
}
function observation(group) {
  const o = group.observation, history = group.observationHistory || {};
  const percent = (n) => (n * 100).toLocaleString(intlLang(), { maximumFractionDigits: 2 }) + "%";
  const metrics = [
    ["availability", "Availability", percent], ["probeLatencyMs", "Probe latency", (n) => n + " ms"],
    ["ttftP50Ms", "TTFT p50", (n) => n + " ms"], ["cacheRate", "Cache rate", percent],
  ];
  if (!o && !metrics.some(([key]) => number(history[key]?.[key]))) return null;
  const box = el("details", "s2-observation");
  box.append(el("summary", "", t("Monitor details")));
  const row = (host, label, value) => {
    const row = el("div", "s2-meta");
    row.append(el("span", "", t(label)), el("span", "", value));
    host.append(row);
  };
  const provenance = (host, sample) => {
    row(host, "Monitor source", sample.source);
    row(host, "Monitor scope", t(sample.scope === "group" ? "Group scope" : "Global scope"));
    if (sample.window) row(host, "Observation window", sample.window);
    row(host, "Sample time", time(sample.sampledAt));
    if (sample.matchedName) row(host, "Matched name", sample.matchedName);
    if (sample.approximate || sample.match !== "id") {
      row(host, "Group matching", t(sample.approximate ? "Approximate match" : "Name match"));
    }
  };
  if (o) provenance(box, o);
  for (const [key, label, format] of metrics) {
    if (number(o?.[key])) row(box, label, format(o[key]));
    else if (number(history[key]?.[key])) {
      const last = history[key], detail = el("details", "s2-metric-history");
      detail.dataset.metric = key;
      detail.append(el("summary", "", t(label)), historical(format(last[key]), last.observedAt));
      provenance(detail, last);
      box.append(detail);
    }
  }
  return box;
}
function paint() {
  if (!pane?.isConnected || pane.closest("[hidden]")) return;
  renderedLang = intlLang();
  pane.replaceChildren();
  if (!writable()) pane.append(el("p", "usage-note", readOnlyReason()));
  const tools = el("div", "sess-tools s2-tools");
  const sites = [...new Set(accounts().map((a) => a.base))];
  if (site && !sites.includes(site)) site = "";
  tools.append(picker("All Sub2API sites", [{ v: "", name: t("All Sub2API sites"), note: "" },
    ...sites.map((s) => ({ v: s, name: s, note: "", literalName: true }))], site, (v) => { site = v; paint(); }));
  tools.append(segs([["linked", t("Linked keys")], ["all", t("All panel groups")]], linkedOnly ? "linked" : "all",
    (value) => { linkedOnly = value === "linked"; paint(); }));
  const [only, cb] = tick(t("Only changed groups"), changed);
  cb.onchange = () => { changed = cb.checked; paint(); };
  tools.append(only);
  pane.append(tools);
  const events = (data?.monitor?.changes || []).filter((c) => !site || c.site === site);
  const list = accounts().filter((a) => !site || a.base === site);
  if (!list.length) {
    pane.append(el("div", "empty-state", t("No Sub2API accounts")),
      el("p", "usage-note", t("Connect a Sub2API account in a provider's balance settings.")));
    return;
  }
  for (const account of list) {
    const observed = displayedMonitor(account.id);
    const monitor = { ...observed, sources: [...(observed?.sources || [])], groups: [...(observed?.groups || [])] };
    for (const p of providers?.providers || []) {
      const binding = p.sub2api;
      if (binding?.session !== account.id) continue;
      if (!monitor.sources.some((s) => s.id === p.id)) monitor.sources.push({
        id: p.id, name: p.name, groupId: binding.groupId || "", groupName: binding.groupName || "", keys: [],
      });
      for (const key of keyBindings(binding)) {
        if (key.groupId && !monitor.groups.some((g) => g.id === key.groupId)) {
          monitor.groups.push({ id: key.groupId, name: key.groupName || key.groupId });
        }
      }
    }
    const config = configOf(account.id);
    const section = el("section", "s2-account");
    const head = el("div", "s2-account-head");
    const name = el("div", "s2-account-name");
    const address = el("div", "s2-site");
    address.append(el("span", "", account.base));
    for (const source of monitor.sources) {
      const p = currentProvider(source.id);
      if (p?.sub2api?.session !== account.id) continue;
      address.append(document.createTextNode(" · "));
      const link = button("", () => openProvider(p.id), "s2-source-name");
      link.textContent = p.name;
      address.append(link);
    }
    name.append(el("b", "s2-identity", account.email || account.subject || account.id), address);
    const controls = el("div", "s2-actions");
    const [watch, on] = tick(t("Monitor this account"), !!config.enabled);
    watch.classList.add("s2-monitor");
    on.setAttribute("role", "switch");
    on.setAttribute("aria-label", t("Monitor this account"));
    watch.lastChild.textContent = t("Background monitoring");
    const toggle = el("span", "lib-switch" + (config.enabled ? " on" : ""));
    toggle.append(el("i"));
    on.after(toggle);
    on.disabled = !writable();
    on.onchange = async () => {
      on.disabled = true;
      toggle.classList.toggle("on", on.checked);
      try { await setMonitor(account, on.checked, config.groups || [], config.revision); }
      catch (e) { on.checked = !on.checked; toggle.classList.toggle("on", on.checked); status(await failure(e, paint), "err"); }
      finally { if (on.isConnected) on.disabled = !writable(); }
    };
    const query = button("Query once", () => queryAccount(account, query));
    query.disabled = !writable();
    const refreshIcon = document.querySelector("#usageReload svg");
    if (refreshIcon) query.prepend(refreshIcon.cloneNode(true));
    query.classList.add("s2-query");
    const more = button("Account actions", () => {
      if (more.classList.contains("open")) return closeProtoMenu();
      openProtoMenu(more, [{ v: "logout", name: "Sign out", note: "" }], "", async () => {
        try { if (await logout(account)) paint(); } catch (e) { status(e.message, "err"); }
      }, "Account actions", "sess-menu", "right");
    }, "s2-more");
    more.replaceChildren(svg(DOTS, 16, 2));
    more.setAttribute("aria-label", t("Account actions"));
    more.setAttribute("aria-expanded", "false");
    more.title = t("Account actions");
    more.disabled = !writable();
    controls.append(watch, query, more);
    if (writable() && account.state !== "ready") {
      controls.append(button("Sign in again", () => { signingIn.add(account.id); paint(); }));
    }
    head.append(name, controls);
    section.append(head);
    if (signingIn.has(account.id) && writable()) section.append(accountSignIn(account, () => { signingIn.delete(account.id); paint(); }));
    if (previews.has(account.id)) section.append(el("p", "usage-note", t("One-time query")));
    const state = el("div", "s2-account-status");
    state.append(el("span", "s2-state" + (account.state === "ready" ? " ready" : ""), t(states[account.state] || "Sign in again")),
      el("span", "", t("Last successful check: {time}", { time: time(monitor?.lastSuccess) })));
    const scheduling = !config.enabled ? "Monitoring is off" : data?.monitor?.running === false ? "Background monitoring is stopped" :
      data?.monitor?.polling ? "Checking Sub2API accounts…" : "Waiting for next check";
    state.append(el("span", "", t(scheduling)));
    const details = el("details", "s2-account-details");
    details.append(el("summary", "", t("Details")));
    if (monitor?.lastAttempt) details.append(el("div", "", t("Last attempt: {time}", { time: time(monitor.lastAttempt) })));
    if (monitor?.nextPoll) details.append(el("div", "", t("Next check: {time}", { time: time(monitor.nextPoll) })));
    const global = observation(monitor);
    if (global) details.append(global);
    if (details.childElementCount > 1) state.append(details);
    section.append(state);
    const resourceErrors = [
      ["Groups: {error}", monitor?.groupsError], ["Key billing: {error}", monitor?.billingError],
      ["Monitor: {error}", monitor?.observationError],
    ].filter(([, error]) => error);
    for (const [label, error] of resourceErrors) section.append(el("p", "s2-error", t(label, { error })));
    if (account.lastError || !resourceErrors.length && monitor?.error) section.append(el("p", "s2-error", account.lastError || monitor.error));
    if (monitor && !monitor.groupsKnown) section.append(el("p", "usage-note", t("Group list not yet confirmed")));
    else if (monitor && !monitor.groupsComplete) section.append(el("p", "usage-note", t("Partial group list")));
    const relevant = events.filter((c) => c.sessionId === account.id).flatMap((c) => c.items || []);
    const sourceKeys = (source) => {
      const p = currentProvider(source.id);
      if (!p?.sub2api || p.sub2api.session !== account.id) return [];
      return [...new Map((p.keyList || []).map((key) => [key.id, {
        ...(source.keys || []).find((sample) => sample.id === key.id), id: key.id,
      }])).values()];
    };
    const groupOf = (source, key) => keyBinding(currentProvider(source.id), key.id)?.groupId || "";
    const groupChanged = (g) => relevant.some((i) => i.groupId === g.id ||
      i.sourceId && (monitor?.sources || []).some((s) => s.id === i.sourceId &&
        sourceKeys(s).some((key) => (!i.keyId || key.id === i.keyId) && groupOf(s, key) === g.id)));
    const allGroups = [...(monitor?.groups || [])];
    if (monitor?.sources?.some((s) => sourceKeys(s).some((key) => !groupOf(s, key)))) allGroups.push({ id: "", name: t("No group selected") });
    const linkedKeys = monitor.sources.flatMap((source) => sourceKeys(source).filter((key) => groupOf(source, key)));
    const differences = monitor.sources.flatMap((source) => sourceKeys(source).filter((key) => {
      const p = currentProvider(source.id), bound = keyBinding(p, key.id);
      return keyCurrent(source, key, account.id) && !key.error && number(key.rate) &&
        key.rate !== (bound?.priceRate > 0 ? bound.priceRate : p.priceRate > 0 ? p.priceRate : 1);
    })).length;
    const summary = el("div", "s2-account-summary");
    summary.append(el("span", "", t("{n} panel groups", { n: allGroups.filter((g) => g.id).length })),
      el("span", "", t("{n} linked keys", { n: linkedKeys.length })));
    if (differences) summary.append(el("span", "s2-differences", t("{n} rate differences", { n: differences })));
    section.append(summary);
    const groups = allGroups.filter((g) => (!linkedOnly || g.id &&
      monitor.sources.some((s) => sourceKeys(s).some((key) => groupOf(s, key) === g.id))) && (!changed || groupChanged(g)));
    if (!groups.length) section.append(el("p", "usage-note",
      t(changed ? "No recorded changes" : linkedOnly ? "No groups with linked keys" : "No visible groups yet")));
    else {
      const table = el("div", "s2-table");
      const header = el("div", "s2-table-head");
      for (const label of ["Panel group", "Key", "Declared rate", "Key rate", "Local rate", "Actions"]) {
        header.append(el("span", "", t(label)));
      }
      table.append(header);
      for (const group of groups) {
        const row = el("div", "s2-group");
        row.dataset.group = group.id;
        const identity = el("div", "s2-group-name");
        identity.append(el("b", "", group.name || group.id), el("span", "s2-muted", group.id));
        if (group.visible === false) identity.append(el("span", "s2-muted", t("Group no longer visible")));
        if (typeof group.visible !== "boolean" && typeof group.lastVisible === "boolean") {
          identity.append(historical(t(group.lastVisible ? "Group became visible" : "Group no longer visible"), group.lastVisibleAt));
        }
        if (typeof group.enabled === "boolean") identity.append(el("span", "s2-state" + (group.enabled ? " ready" : ""), t(group.enabled ? "Panel enabled" : "Panel disabled")));
        else if (typeof group.lastEnabled === "boolean") {
          identity.append(historical(t(group.lastEnabled ? "Panel enabled" : "Panel disabled"), group.lastEnabledAt));
        }
        const sources = (monitor?.sources || []).filter((s) => sourceKeys(s).some((key) => groupOf(s, key) === group.id));
        const keys = el("div", "s2-cell s2-keys");
        const declared = el("div", "s2-cell s2-declared");
        declared.append(el("span", "s2-label", t("Declared rate")), el("span", "", rate(group.rate)));
        if (!number(group.rate) && number(group.lastRate)) declared.append(historical(rate(group.lastRate), group.lastRateAt));
        const rates = el("div", "s2-cell s2-billing");
        const local = el("div", "s2-cell s2-local");
        let keyRow = 1;
        for (const source of sources) {
          const keyList = el("div", "s2-source");
          keyList.dataset.source = source.id;
          const line = el("div", "s2-source");
          line.dataset.source = source.id;
          const p = currentProvider(source.id);
          const applied = el("div", "s2-source");
          applied.dataset.source = source.id;
          for (const key of sourceKeys(source).filter((key) => groupOf(source, key) === group.id)) {
            const current = keyCurrent(source, key, account.id);
            const k = el("div", "s2-key");
            k.dataset.key = key.id;
            const display = p.keyList.find((x) => x.id === key.id);
            const keyLabel = el("div", "s2-key");
            keyLabel.dataset.key = key.id;
            keyLabel.append(el("span", "s2-label", t("Key")), el("span", "s2-value", keyName(display)));
            keyLabel.title = p.name + " · " + keyName(display);
            keyList.append(keyLabel);
            k.append(el("span", "s2-label", t("Key rate")), el("span", "s2-value", rate(current ? key.rate : undefined)));
            if ((!current || !number(key.rate)) && number(key.lastRate ?? key.rate)) {
              k.append(historical(rate(key.lastRate ?? key.rate), key.lastRateAt || key.lastSuccess));
            }
            k.title = time(key.lastSuccess);
            if (key.error) k.append(el("span", "s2-error", key.error));
            line.append(k);
            const localKey = el("div", "s2-key"), bound = keyBinding(p, key.id);
            localKey.dataset.key = key.id;
            localKey.append(el("span", "s2-label", t("Local rate")),
              el("span", "s2-value", rate(bound?.priceRate > 0 ? bound.priceRate : p.priceRate > 0 ? p.priceRate : 1)));
            for (const cell of [keyLabel, k, localKey]) cell.style.setProperty("--s2-row", keyRow);
            keyRow++;
            const expected = rateApplication(source, key, account.id);
            const apply = button("Apply rate", () => applyRate(source, key, account.id, apply, expected), "s2-apply");
            apply.textContent = t("Set locally to {rate}", { rate: rate(current ? key.rate : undefined) });
            apply.prepend(svg(CLI_UP, 12, 1.6));
            apply.setAttribute("aria-label", t("Apply rate"));
            apply.disabled = !expected;
            apply.title = apply.textContent;
            if (key.rate === 0) apply.title = t("A zero panel rate cannot be applied as a free price.");
            localKey.append(apply);
            applied.append(localKey);
          }
          rates.append(line);
          const query = button("Query once", () => queryAccount(account, query, source), "s2-query-source");
          query.disabled = !writable() || !p?.sub2api || p.sub2api.session !== account.id;
          query.style.setProperty("--s2-row", keyRow++);
          keyList.append(query);
          keys.append(keyList);
          local.append(applied);
        }
        if (!sources.length) {
          for (const [host, label, value] of [[keys, "Key", t("No linked key")],
            [rates, "Key rate", t("Rate not confirmed")], [local, "Local rate", "-"]]) {
            const cell = el("div", "s2-key s2-muted");
            cell.style.setProperty("--s2-row", keyRow);
            cell.append(el("span", "s2-label", t(label)), el("span", "s2-value", value));
            host.append(cell);
          }
          keyRow++;
        }
        const updated = el("div", "s2-group-details");
        updated.style.setProperty("--s2-row", keyRow + 1);
        const detail = observation(group);
        if (detail) updated.append(detail);
        if (group.observation) updated.append(el("span", "s2-muted", t("Last successful observation: {time}", { time: time(group.monitorLastSuccess) })));
        if (group.error) updated.append(el("span", "s2-error", group.error));
        const actions = el("div", "s2-cell s2-group-actions");
        actions.style.setProperty("--s2-row", keyRow);
        const link = button("Link key", () => bindGroup(group, account, link).catch((e) => status(e.message, "err")));
        link.append(svg(CHEV, 12, 1.6));
        link.setAttribute("aria-expanded", "false");
        link.disabled = !writable() || !group.id;
        actions.append(link);
        row.append(identity, keys, declared, rates, local, actions);
        if (updated.childElementCount) row.append(updated);
        table.append(row);
      }
      table.title = t("Local group binding; the panel has not verified which group each key belongs to.");
      section.append(table);
    }
    pane.append(section);
  }
  const history = el("details", "s2-history");
  history.append(el("summary", "", t("Recent Sub2API changes")));
  if (!events.length) history.append(el("p", "usage-note", t("No recorded changes")));
  for (const event of events) {
    const entry = el("div", "s2-change");
    entry.append(el("time", "s2-muted", time(event.at)), el("b", "", event.site));
    for (const item of event.items || []) {
      const line = el("div");
      line.append(el("span", "", t(changeNames[item.kind] || "Sub2API change")),
        el("span", "", [item.groupName || item.groupId, item.sourceId, item.before, item.after].filter(Boolean).join(" · ")));
      entry.append(line);
    }
    history.append(entry);
  }
  pane.append(history);
}
async function renderUsage(host, refresh = false, opened = false) {
  pane = host;
  if (!pane?.isConnected || pane.closest("[hidden]")) return;
  if (observedPane !== host) {
    visibility.disconnect();
    observedPane = host;
    for (const node of [host, host.closest("#view-usage")].filter(Boolean)) visibility.observe(node, { attributes: true, attributeFilter: ["hidden"] });
  }
  if (!data) pane.replaceChildren(el("div", "empty-state", t("Loading…")));
  try {
    if (refresh && writable()) await request("refresh", {});
    const previous = JSON.stringify(data);
    await Promise.all([read(), opened || refresh || !providers ? reloadProviders() : Promise.resolve()]);
    if (opened || refresh || renderedLang !== intlLang() || !host.childElementCount || host.querySelector(".empty-state") || previous !== JSON.stringify(data)) paint();
  } catch (e) {
    if (!host.isConnected || host.closest("[hidden]") || pane !== host) return;
    if (!data) host.replaceChildren(el("p", "s2-error", e.message));
    else status(e.message, "err");
  }
}
export { renderUsage };
