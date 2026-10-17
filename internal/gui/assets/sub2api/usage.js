import { data, states, accounts, accountOf, monitorOf, configOf, currentProvider, keyBindings, keyBinding, keyName,
  writable, readOnlyReason, button, request, read, reloadProviders, failure, picker, logout } from "./common.js";
import { accountSignIn, cancelAccountForms } from "./editor.js";
let pane = null, site = "", changed = false, linkedOnly = true, renderedLang = "", providerSnapshot = "";
const signingIn = new Set(), previews = new Map(), queries = new Map();
let observedPane;
const visibility = new MutationObserver(() => {
  if (!pane?.closest("[hidden]")) return;
  for (const query of queries.values()) query.controller.abort();
  signingIn.clear(); previews.clear(); queries.clear(); cancelAccountForms();
});
const sampleTime = (monitor) => Math.max(0, ...[
  monitor?.lastSuccess, monitor?.groupsLastSuccess, monitor?.billingLastSuccess, monitor?.observationLastSuccess,
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
  group_renamed: "Panel group renamed", group_rate: "Group rate changed",
  group_enabled: "Group status changed", key_rate: "Key billing rate changed",
};
const number = (n) => typeof n === "number" && Number.isFinite(n);
const rate = (n) => number(n) ? n.toLocaleString(intlLang(), { maximumFractionDigits: 20 }) + "×" : t("Rate not confirmed");
const changeRate = (value) => typeof value === "string" && value.trim() ? Number(value) : NaN;
const sameRateChange = (a, b) => a.groupId && a.groupId === b.groupId &&
  [a.before, a.after].every((value) => number(changeRate(value))) &&
  a.before === b.before && a.after === b.after;
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
  source.activeKeyIds?.includes(key.id) &&
  currentProvider(source.id)?.keyList?.some((configured) => configured.id === key.id && configured.on !== false);
const localRate = (p, key) => keyBinding(p, key.id)?.priceRate > 0 ? keyBinding(p, key.id).priceRate : p.priceRate > 0 ? p.priceRate : 1;
const syncState = (source, key, session) => keyCurrent(source, key, session) && (key.syncError || key.groupError || key.error) ? "Sync failed" :
  keyCurrent(source, key, session) && key.groupVerified === true && !key.error && number(key.rate) && key.rate > 0 &&
  key.rate === key.localRate && key.rate === localRate(currentProvider(source.id), key) ? "Synced" : "Pending sync";
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
async function queryAccount(account, anchor, source) {
  const previous = queries.get(account.id);
  previous?.controller.abort();
  if (previous?.anchor.isConnected) previous.anchor.disabled = !writable();
  const query = { host: pane, anchor, controller: new AbortController() };
  queries.set(account.id, query);
  const current = () => queries.get(account.id) === query && pane === query.host &&
    pane?.isConnected && !pane.closest("[hidden]");
  anchor.disabled = true;
  const config = configOf(account.id);
  try {
    const p = source && currentProvider(source.id), binding = p?.sub2api;
    if (source && (!binding || binding.session !== account.id)) throw new Error(t("The provider changed; query again."));
    const result = await request("preview", { session: account.id, expectedGeneration: account.generation,
      settingsRevision: config.revision, ...(source && { provider: p.id,
        expectedRevision: p.sub2apiRevision, bindingVersion: binding.version }) }, query.controller.signal);
    if (!current()) return;
    await reloadProviders();
    if (!current()) return;
    previews.set(account.id, result);
    paint();
  } catch (e) {
    if (!current() || query.controller.signal.aborted) return;
    const text = await failure(e, () => { if (current()) paint(); });
    if (current()) status(text, "err");
  } finally {
    if (current() && anchor.isConnected) anchor.disabled = !writable();
    if (queries.get(account.id) === query) queries.delete(account.id);
  }
}
function observation(group, scope) {
  const o = group.observation?.scope === scope ? group.observation : null;
  const history = Object.fromEntries(Object.entries(group.observationHistory || {}).filter(([, sample]) => sample.scope === scope));
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
    const metricLabel = (sample) => key === "ttftP50Ms" && sample.ttftStatistic === "trimmed_mean" ? "TTFT trimmed mean" : label;
    if (number(o?.[key])) row(box, metricLabel(o), format(o[key]));
    else if (number(history[key]?.[key])) {
      const last = history[key], detail = el("details", "s2-metric-history");
      detail.dataset.metric = key;
      detail.append(el("summary", "", t(metricLabel(last))), historical(format(last[key]), last.observedAt));
      provenance(detail, last);
      box.append(detail);
    }
  }
  return box;
}
function metricTone(key, value) {
  if (key === "ttftP50Ms") return value <= 10000 ? "good" : value <= 20000 ? "warn" : value <= 30000 ? "slow" : "bad";
  const [good, warn, slow] = key === "availability" ? [.99, .95, .90] : [.85, .70, .55];
  return value >= good ? "good" : value >= warn ? "warn" : value >= slow ? "slow" : "bad";
}
function groupMetrics(group) {
  const box = el("div", "s2-group-metrics"), current = group.observation;
  const percent = (value) => (value * 100).toLocaleString(intlLang(), { maximumFractionDigits: 2 }) + "%";
  for (const [key, label, format] of [
    ["cacheRate", "Cache rate", percent], ["availability", "Availability", percent],
    ["ttftP50Ms", "First token", (value) => (value / 1000).toLocaleString(intlLang(),
      { minimumFractionDigits: 1, maximumFractionDigits: 2 }) + " s"],
  ]) {
    const known = (sample) => sample?.scope === "group" && number(sample[key]) && sample[key] >= 0 &&
      (key === "ttftP50Ms" || sample[key] <= 1);
    const metric = el("div", "s2-group-metric");
    metric.dataset.metric = key;
    const value = el("span", "s2-metric-value", known(current) ? format(current[key]) : "\u2014");
    if (known(current)) value.dataset.tone = metricTone(key, current[key]);
    metric.append(el("span", "s2-muted", t(label)), value);
    const last = group.observationHistory?.[key], sample = known(current) ? current : known(last) ? last : null;
    if (!known(current) && known(last)) {
      const history = button("Has history", () => {
        const details = box.closest(".s2-group-details")?.querySelector(":scope > .s2-observation");
        if (!details) return;
        details.open = true;
        const reading = details.querySelector(`.s2-metric-history[data-metric="${key}"]`);
        if (reading) reading.open = true;
        (reading || details).querySelector("summary")?.focus({ preventScroll: true });
      }, "s2-historical s2-history-link");
      history.title = t("Last confirmed: {value} ({time})", { value: format(last[key]), time: time(last.observedAt) });
      metric.append(history);
    }
    if (sample) metric.title = [
      t(key === "ttftP50Ms" ? sample.ttftStatistic === "trimmed_mean" ? "TTFT trimmed mean" : "TTFT p50" : label),
      t("Monitor source") + ": " + sample.source,
      t("Monitor scope") + ": " + t("Group scope"),
      sample.window && t("Observation window") + ": " + sample.window,
      t("Sample time") + ": " + time(sample.sampledAt),
      !known(current) && t("Last successful observation: {time}", { time: time(last.observedAt) }),
    ].filter(Boolean).join("\n");
    box.append(metric);
  }
  return box;
}
function paint() {
  if (!pane?.isConnected || pane.closest("[hidden]")) return;
  const historyOpen = pane.querySelector(".s2-history")?.open;
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
  if (accounts().some((account) => configOf(account.id).enabled) && data?.monitor?.running !== false) {
    tools.append(el("span", "s2-muted s2-poll-status",
      t(data?.monitor?.polling ? "Background polling" : "Waiting for next check")));
  }
  pane.append(tools);
  const events = (data?.monitor?.changes || []).filter((c) => !site || c.site === site).sort((a, b) => {
    const first = Date.parse(a.at), second = Date.parse(b.at);
    return (number(second) ? second : -Infinity) - (number(first) ? first : -Infinity);
  });
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
      for (const key of monitor.sources.find((s) => s.id === p.id)?.keys || []) {
        if (key.groupVerified === true && key.groupId && !monitor.groups.some((g) => g.id === key.groupId)) {
          monitor.groups.push({ id: key.groupId, name: key.groupName || key.groupId });
        }
      }
    }
    const config = configOf(account.id);
    const section = el("section", "s2-account");
    const head = el("div", "s2-account-head");
    const name = el("div", "s2-account-name");
    const address = el("div", "s2-site");
    address.append(el("span", "", account.base.replace(/^https?:\/\//, "")));
    for (const source of monitor.sources) {
      const p = currentProvider(source.id);
      if (p?.sub2api?.session !== account.id) continue;
      address.append(document.createTextNode(" · "));
      const link = button("", () => openProvider(p.id), "s2-source-name");
      link.textContent = p.name;
      address.append(link);
    }
    name.append(address, el("span", "s2-identity", account.email || account.subject || account.id));
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
    if (signingIn.has(account.id) && writable()) section.append(accountSignIn(account,
      () => { signingIn.delete(account.id); paint(); }, (preview) => previews.set(account.id, preview)));
    if (previews.has(account.id)) section.append(el("p", "usage-note", t("One-time query")));
    const state = el("div", "s2-account-status");
    state.append(el("span", "s2-state" + (account.state === "ready" ? " ready" : ""), t(states[account.state] || "Sign in again")));
    const checked = el("span", "s2-muted", t("Last successful check: {time}", {
      time: monitor?.lastSuccess && !String(monitor.lastSuccess).startsWith("0001-") &&
        Number.isFinite(Date.parse(monitor.lastSuccess)) ? ago(monitor.lastSuccess) : time(),
    }));
    checked.title = time(monitor?.lastSuccess);
    const scheduling = !config.enabled ? "Monitoring is off" :
      data?.monitor?.running === false ? "Background monitoring is stopped" : "";
    if (scheduling) state.append(el("span", "", t(scheduling)));
    const details = el("details", "s2-account-details");
    details.append(el("summary", "", t("Account details")));
    details.append(el("div", "", t("Last successful check: {time}", { time: time(monitor?.lastSuccess) })));
    if (monitor?.lastAttempt) details.append(el("div", "", t("Last attempt: {time}", { time: time(monitor.lastAttempt) })));
    if (monitor?.nextPoll) details.append(el("div", "", t("Next check: {time}", { time: time(monitor.nextPoll) })));
    const global = observation(monitor, "global");
    if (global) details.append(global);
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
    const groupOf = (source, key) => keyCurrent(source, key, account.id) && key.groupVerified === true ? key.groupId || "" : "";
    const groupChanged = (g) => relevant.some((i) => i.groupId === g.id ||
      i.sourceId && (monitor?.sources || []).some((s) => s.id === i.sourceId &&
        sourceKeys(s).some((key) => (!i.keyId || key.id === i.keyId) && groupOf(s, key) === g.id)));
    const allGroups = [...(monitor?.groups || [])];
    if (monitor?.sources?.some((s) => sourceKeys(s).some((key) => !groupOf(s, key)))) allGroups.push({ id: "", name: t("Group not identified") });
    const linkedKeys = monitor.sources.flatMap((source) => sourceKeys(source).filter((key) => groupOf(source, key)));
    const differences = monitor.sources.flatMap((source) => sourceKeys(source)
      .filter((key) => syncState(source, key, account.id) !== "Synced")).length;
    const summary = el("div", "s2-account-summary");
    summary.append(el("span", "", t("{linked} linked / {total} total groups", {
      linked: new Set(linkedKeys.map((key) => key.groupId)).size, total: allGroups.filter((g) => g.id).length,
    })),
      el("span", "", t("{n} linked keys", { n: linkedKeys.length })));
    if (differences) summary.append(el("span", "s2-differences", t("{n} keys pending sync", { n: differences })));
    state.append(summary, checked);
    if (details.childElementCount > 1) state.append(details);
    const groups = allGroups.filter((g) => (!linkedOnly ||
      monitor.sources.some((s) => sourceKeys(s).some((key) => groupOf(s, key) === g.id))) && (!changed || groupChanged(g)));
    if (!groups.length) section.append(el("p", "usage-note",
      t(changed ? "No recorded changes" : linkedOnly ? "No groups with linked keys" : "No visible groups yet")));
    else {
      const table = el("div", "s2-table");
      for (const group of groups) {
        const row = el("div", "s2-group");
        row.dataset.group = group.id;
        const identity = el("div", "s2-group-name");
        const groupName = el("b", "", group.name || group.id);
        groupName.title = group.id;
        identity.append(groupName);
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
        declared.append(el("span", "s2-label", t("Group rate")), el("span", "", rate(group.rate)));
        if (!number(group.rate) && number(group.lastRate)) declared.append(historical(rate(group.lastRate), group.lastRateAt));
        identity.append(declared);
        const rates = el("div", "s2-cell s2-billing");
        const local = el("div", "s2-cell s2-local");
        let keyRow = 3;
        for (const source of sources) {
          const keyList = el("div", "s2-source");
          keyList.dataset.source = source.id;
          const line = el("div", "s2-source");
          line.dataset.source = source.id;
          const p = currentProvider(source.id);
          const applied = el("div", "s2-source");
          applied.dataset.source = source.id;
          const query = button("Query once", () => queryAccount(account, query, source), "s2-query-source");
          if (refreshIcon) query.replaceChildren(refreshIcon.cloneNode(true));
          query.setAttribute("aria-label", t("Query once"));
          query.title = t("Query once");
          query.disabled = !writable() || !p?.sub2api || p.sub2api.session !== account.id;
          for (const key of sourceKeys(source).filter((key) => groupOf(source, key) === group.id)) {
            const current = keyCurrent(source, key, account.id);
            const k = el("div", "s2-key");
            k.dataset.key = key.id;
            const display = p.keyList.find((x) => x.id === key.id);
            const keyLabel = el("div", "s2-key");
            keyLabel.dataset.key = key.id;
            keyLabel.append(el("span", "s2-label", t("Key")), el("span", "s2-value", keyName(display)));
            if (!keyList.childElementCount) keyLabel.append(query);
            keyLabel.title = p.name + " · " + keyName(display);
            if (!group.id) {
              const bound = keyBinding(p, key.id);
              if (bound?.groupId) keyLabel.append(el("span", "s2-muted",
                t("Previous group: {group}", { group: [bound.groupName, bound.groupId].filter(Boolean).join(" · ") })));
            }
            if (key.groupError) keyLabel.append(el("span", "s2-error", key.groupError));
            keyList.append(keyLabel);
            k.append(el("span", "s2-label", t("Key billing rate")), el("span", "s2-value", rate(current ? key.rate : undefined)));
            if ((!current || !number(key.rate)) && number(key.lastRate ?? key.rate)) {
              k.append(historical(rate(key.lastRate ?? key.rate), key.lastRateAt || key.lastSuccess));
            }
            k.title = time(key.lastSuccess);
            if (key.error) k.append(el("span", "s2-error", key.error));
            line.append(k);
            const localKey = el("div", "s2-key");
            localKey.dataset.key = key.id;
            localKey.append(el("span", "s2-label", t("Local rate")),
              el("span", "s2-value", rate(localRate(p, key))));
            for (const cell of [keyLabel, k, localKey]) cell.style.setProperty("--s2-row", keyRow);
            keyRow++;
            const sync = syncState(source, key, account.id);
            localKey.append(el("span", "s2-sync" + (sync === "Synced" ? " ready" : ""), t(sync)));
            if (key.syncError) localKey.append(el("span", "s2-error", key.syncError));
            applied.append(localKey);
          }
          rates.append(line);
          keys.append(keyList);
          local.append(applied);
        }
        if (!sources.length) {
          for (const [host, label, value] of [[keys, "Key", t("No linked key")],
            [rates, "Key billing rate", t("Rate not confirmed")], [local, "Local rate", "-"]]) {
            const cell = el("div", "s2-key s2-muted");
            cell.style.setProperty("--s2-row", keyRow);
            cell.append(el("span", "s2-label", t(label)), el("span", "s2-value", value));
            host.append(cell);
          }
          keyRow++;
        }
        const updated = el("div", "s2-group-details");
        updated.append(identity, groupMetrics(group));
        const detail = observation(group, "group");
        if (group.observation?.scope === "group" && group.monitorLastSuccess &&
          !String(group.monitorLastSuccess).startsWith("0001-") && Number.isFinite(Date.parse(group.monitorLastSuccess))) {
          if (detail) detail.append(el("div", "s2-muted", t("Last successful observation: {time}", { time: time(group.monitorLastSuccess) })));
        }
        if (detail) updated.append(detail);
        if (group.error) updated.append(el("span", "s2-error", group.error));
        const header = el("div", "s2-table-head");
        for (const label of ["Key", "Key billing rate", "Local rate"]) header.append(el("span", "", t(label)));
        row.append(updated, header, keys, rates, local);
        table.append(row);
      }
      section.append(table);
    }
    pane.append(section);
  }
  const history = el("details", "s2-history");
  history.open = historyOpen === true;
  history.append(el("summary", "", t("Recent Sub2API changes")));
  if (!events.length) history.append(el("p", "usage-note", t("No recorded changes")));
  for (const event of events) {
    const entry = el("div", "s2-change");
    entry.append(el("time", "s2-muted", time(event.at)), el("b", "", event.site));
    const items = event.items || [], groups = items.filter((item) => item.kind === "group_rate");
    for (const item of items) {
      if (item.kind === "key_rate" && groups.some((group) => sameRateChange(group, item))) continue;
      const line = el("div");
      const labels = item.kind === "group_renamed" ? [] : [item.groupName || item.groupId];
      if (item.kind === "key_rate") {
        const p = currentProvider(item.sourceId), key = p?.keyList?.find((key) => key.id === item.keyId);
        labels.push(p?.name || item.sourceId);
        if (item.keyId) labels.push(keyName(key || { id: item.keyId }));
      } else labels.push(item.sourceId);
      let values = item.kind === "group_added" || item.kind === "group_unavailable" ? [] : [item.before, item.after];
      if (item.kind === "group_added") values.push(rate(item.rate));
      if (item.kind === "group_rate" || item.kind === "key_rate") {
        values = [rate(changeRate(item.before)) + " → " + rate(changeRate(item.after))];
      }
      if (item.kind === "group_renamed") {
        values = [[item.before, item.after].filter(Boolean).join(" → ") || item.groupName || item.groupId];
      }
      if (item.kind === "group_rate") {
        const keys = items.filter((key) => key.kind === "key_rate" && sameRateChange(item, key));
        if (keys.length) values.push(t("{n} linked keys changed to the same rate", { n: keys.length }));
      }
      line.append(el("span", "", t(changeNames[item.kind] || "Sub2API change")),
        el("span", "", [...labels, ...values].filter(Boolean).join(" · ")));
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
    const snapshot = JSON.stringify(data?.monitor?.accounts?.flatMap((account) =>
      (account.sources || []).map((source) => [account.sessionId, source.id, source.revision, source.bindingVersion])));
    if (snapshot !== providerSnapshot && data?.monitor?.accounts?.some((account) => account.sources?.some((source) => {
      const provider = currentProvider(source.id);
      return source.revision && (!provider || provider.sub2api?.session === account.sessionId &&
        (source.revision !== provider.sub2apiRevision || source.bindingVersion !== provider.sub2api.version));
    }))) await reloadProviders();
    providerSnapshot = snapshot;
    if (opened || refresh || renderedLang !== intlLang() || !host.childElementCount || host.querySelector(".empty-state") || previous !== JSON.stringify(data)) paint();
  } catch (e) {
    if (!host.isConnected || host.closest("[hidden]") || pane !== host) return;
    if (!data) host.replaceChildren(el("p", "s2-error", e.message));
    else status(e.message, "err");
  }
}
export { renderUsage };
