// The real page, isolated panel fixtures: credentials never enter a provider
// save, 2FA is cancellable, and applying a rate uses the displayed version.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");
const { strings } = require("./strings.cjs");

const assets = path.resolve(__dirname, "../assets");
const { I18N } = strings();
const word = (lang, key) => lang === "en" ? key : I18N[lang]?.[key];
const stamp = "2026-10-16T01:00:00Z";
const relay = {
  id: "relay", name: "Relay", icon: "generic", chat: "https://panel.example.com/v1", responses: "", anthropic: "", catalog: "",
  models: [{ id: "model-a", name: "Model A", on: true }], agents: [], fallback: [], headers: {},
  key: { set: true, masked: "sk-…one" },
  keyList: [{ id: "key-one", name: "First", masked: "sk-…one", on: true }, { id: "key-two", name: "Second", masked: "sk-…two", on: true }],
  balanceURL: "https://panel.example.com/api/v1/custom-balance", balancePath: "$credits.left", balanceMethod: "POST",
  balanceToken: { takes: true, set: true }, priceRate: 1.2, sub2apiRevision: "revision-1", saveRevision: "save-1",
};
const account = { id: "session-one", base: "https://panel.example.com", email: "reader@example.com", subject: "42", state: "ready", version: 3, generation: 11, updatedAt: stamp };
const binding = { session: account.id, groupId: "a", groupName: "Premium", keyIds: ["key-one", "key-two"], version: "binding-7" };
const nextVersion = (value) => (value || "binding-new") + "-next";
const boundKeys = (p) => p.sub2api?.keys || (p.sub2api?.keyIds || []).map((id) =>
  ({ id, groupId: p.sub2api.groupId || "", groupName: p.sub2api.groupName || "" }));
const localRate = (p, keyId = "key-one") => boundKeys(p).find((k) => k.id === keyId)?.priceRate ||
  p.keyPriceRates?.[keyId] || p.priceRate || 1;
function observationHistory(o, observedAt = stamp) {
  const { source, scope, window, sampledAt, matchedName, match, approximate } = o;
  return Object.fromEntries(["availability", "probeLatencyMs", "ttftP50Ms", "cacheRate"]
    .filter((key) => typeof o[key] === "number")
    .map((key) => [key, { source, scope, window, sampledAt, matchedName, match, approximate, [key]: o[key], observedAt }]));
}

function fixture(bound = false) {
  const p = structuredClone(relay);
  if (bound) p.sub2api = structuredClone(binding);
  const group = {
    id: "a", name: "Premium", rate: .8, enabled: true, visible: true, lastSuccess: stamp, monitorLastSuccess: stamp,
    error: "Latest observation unavailable", lastObservationAt: stamp,
    lastObservation: { source: "v2-matrix", scope: "group", window: "90m", sampledAt: stamp, match: "id", approximate: false, availability: .98, ttftP50Ms: 45, cacheRate: .75 },
  };
  group.observationHistory = observationHistory(group.lastObservation);
  const source = { id: p.id, name: p.name, groupId: "a", groupName: "Premium", bindingVersion: binding.version, revision: p.sub2apiRevision,
    keys: p.keyList.map((k) => ({ id: k.id, groupId: "a", groupName: "Premium", rate: .8, lastSuccess: stamp })),
    activeKeyIds: p.keyList.map((k) => k.id), rate: .8, conflict: false };
  return {
    providers: [p], accounts: bound ? [structuredClone(account)] : [], posted: [], failNext: "", revision: 1, providerReads: 0, panelReads: 0,
    capability: { writable: true, reason: "", browser: true, browserReason: "" },
    configs: { [account.id]: { configured: true, enabled: true, groups: [{ id: "a", name: "Premium" }], revision: "settings-1", generation: account.generation } },
    monitor: {
      updatedAt: stamp, running: true, polling: false,
      accounts: [{ sessionId: account.id, site: account.base, generation: account.generation, settingsRevision: "settings-1", enabled: true, groupsKnown: true, groupsComplete: true, watchedGroups: [{ id: "a", name: "Premium" }],
        groups: [group, { id: "b", name: "Standard", visible: true, lastSuccess: stamp, lastRate: 1.25, lastRateAt: stamp, lastEnabled: false, lastEnabledAt: stamp },
          { id: "c", name: "Free", rate: 0, visible: true, lastSuccess: stamp }],
        sources: [source], lastSuccess: stamp,
        observation: { source: "v2-snapshot", scope: "global", window: "90m", sampledAt: stamp, match: "id", approximate: false, availability: .75 },
      }],
      changes: [{ id: "change-one", sessionId: account.id, site: account.base, at: stamp, items: [{ kind: "key_rate", groupId: "a", groupName: "Premium", sourceId: p.id, keyId: "key-one", before: "1", after: "0.8" }] }],
    },
  };
}

function server(f, lang) {
  const all = () => ({ providers: f.providers, presets: f.presets || [], excluded: [], gateway: { running: true, window: true } });
  return async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data, status = 200) => route.fulfill({ status, json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs=${JSON.stringify({ lang, theme: "light", web: true })};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") { f.providerReads++; return json(all()); }
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/sub2api") { f.panelReads++; return json({ accounts: f.accounts, monitor: f.monitor, configs: f.configs, capability: f.capability }); }
    if (url.pathname.startsWith("/api/sub2api/")) {
      const action = url.pathname.slice("/api/sub2api/".length), body = req.postDataJSON();
      f.posted.push({ action, body });
      if (!f.capability.writable) return json({ error: f.capability.reason }, 403);
      const p = f.providers.find((p) => p.id === body.provider);
      if (["bind", "bind-key", "apply-key-rate", "logout", "login-account", "2fa-account", "browser-account", "monitor", "preview", "refresh-account"].includes(action) &&
        body.expectedGeneration !== f.accounts.find((a) => a.id === (body.session || p?.sub2api?.session))?.generation) {
        return json({ error: "The panel account changed; read it again." }, 409);
      }
      const bump = () => { p.sub2apiRevision = "revision-" + ++f.revision; p.saveRevision = "save-" + f.revision; };
      const sampled = () => {
        const source = f.monitor.accounts.flatMap((a) => a.sources).find((s) => s.id === p.id);
        if (source && p.sub2api) Object.assign(source, {
          bindingVersion: p.sub2api.version, revision: p.sub2apiRevision,
          groupId: p.sub2api.groupId, groupName: p.sub2api.groupName,
        });
      };
      if (["login", "browser", "bind", "bind-key", "unbind", "apply-key-rate"].includes(action)) {
        if (f.failNext === action) {
          f.failNext = "";
          bump();
          if (p.sub2api) p.sub2api.version = nextVersion(p.sub2api.version);
          if (action === "apply-key-rate") p.priceRate = 1.4;
          sampled();
          return json({ error: "The provider changed; read it again." }, 409);
        }
        if (body.expectedRevision !== p.sub2apiRevision ||
          ["bind", "bind-key", "unbind"].includes(action) && body.expectedVersion !== (p.sub2api?.version || "") ||
          action === "apply-key-rate" && (body.bindingVersion !== p.sub2api?.version || body.expectedRate !== localRate(p, body.keyId))) {
          return json({ error: "The provider changed; read it again." }, 409);
        }
      }
      if (action === "login-account" && !f.loginReady) return json({ state: "two_factor", attempt: "account-attempt", sessionId: body.session });
      if (action === "2fa-account" || action === "login-account" || action === "browser-account") {
        const a = f.accounts.find((a) => a.id === body.session);
        a.generation++;
        a.state = "ready";
        return json({ state: "ready", sessionId: a.id, account: a });
      }
      if (action === "login" && !f.loginReady) return json({ state: "two_factor", attempt: "attempt-one" });
      if (action === "2fa" || action === "browser" || action === "login" && f.loginReady) {
        f.accounts = [structuredClone(account)];
        if (f.associationFailure) return json({ state: "ready", account,
          binding: { linked: false, error: "The balance request failed; existing settings were kept." } });
        p.sub2api = p.sub2api?.session === account.id
          ? structuredClone(p.sub2api) : { session: account.id, version: binding.version };
        f.providers[0].balanceToken.set = false;
        if (!p.balanceURL) p.balanceURL = account.base + "/api/v1/user/profile";
        if (!p.balancePath) p.balancePath = "$data.balance";
        bump();
        sampled();
        return json({ state: "ready", account, binding: { linked: true } });
      }
      if (action === "unbind") { delete p.sub2api; bump(); }
      if (action === "bind") { p.sub2api = {
        session: body.session, groupId: body.groupId, groupName: body.groupName, keyIds: body.keyIds, version: nextVersion(body.expectedVersion),
      }; bump(); sampled(); }
      if (action === "bind-key") {
        const keys = structuredClone(boundKeys(p)), index = keys.findIndex((k) => k.id === body.keyId);
        const key = { ...(keys[index] || {}), id: body.keyId, groupId: body.groupId, groupName: body.groupName };
        if (index < 0) keys.push(key); else keys[index] = key;
        p.sub2api = { session: body.session, keys, version: nextVersion(body.expectedVersion) };
        bump(); sampled();
      }
      if (action === "logout") f.accounts.find((a) => a.id === body.session).state = "logged_out";
      if (["monitor", "preview", "refresh-account"].includes(action) && body.settingsRevision !== f.configs[body.session].revision) {
        return json({ error: "The monitor settings changed; read them again." }, 409);
      }
      if (action === "monitor") {
        const revision = nextVersion(f.configs[body.session].revision);
        f.configs[body.session] = { configured: true, enabled: body.enabled, groups: body.groups, revision, generation: body.expectedGeneration };
        const monitored = f.monitor.accounts.find((a) => a.sessionId === body.session);
        if (monitored) monitored.settingsRevision = revision;
      }
      if (action === "preview") {
        const result = structuredClone(f.previewAccount || f.monitor.accounts.find((a) => a.sessionId === body.session));
        if (!body.provider) result.sources = [];
        return json(result);
      }
      if (action === "apply-key-rate") {
        p.keyPriceRates = { ...p.keyPriceRates, [body.keyId]: body.rate };
        p.sub2api.keys = structuredClone(boundKeys(p));
        p.sub2api.keys.find((key) => key.id === body.keyId).priceRate = body.rate;
        bump();
      }
      return json({ ok: true });
    }
    if (url.pathname.startsWith("/api/provider/")) {
      const action = url.pathname.slice("/api/provider/".length), body = req.postDataJSON();
      f.posted.push({ action: "provider/" + action, body });
      if (action === "balance") return json({ ok: true, amount: "$9.00" });
      if (action === "save" && body.saveRevision !== f.providers.find((p) => p.id === body.from)?.saveRevision) {
        return json({ error: "The provider changed; reopen it before saving." }, 409);
      }
      if (action === "delete") f.providers = f.providers.filter((p) => p.id !== body.id);
      if (action === "off" || action === "on") f.providers.find((p) => p.id === body.id).off = action === "off";
      return json(all());
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}
async function open(browser, f, lang, width, view = "providers", allGroups = true) {
  const context = await browser.newContext({ viewport: { width, height: 860 }, reducedMotion: "reduce" });
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/*", server(f, lang));
  await page.goto("http://magpie.test/?view=" + view + (view === "usage" ? "&tab=sub2api" : ""));
  if (view === "usage" && allGroups) await page.getByRole("button", { name: word(lang, "All panel groups"), exact: true }).click();
  return { page, context, errors };
}
const posted = (f, action) => f.posted.filter((p) => p.action === action);
async function waitPost(page, action, click) {
  try {
    const [response, actionResult] = await Promise.allSettled([page.waitForResponse((r) => new URL(r.url()).pathname === "/api/sub2api/" + action), click()]);
    if (actionResult.status === "rejected") throw actionResult.reason;
    if (response.status === "rejected") throw response.reason;
  } catch (error) {
    if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, "sub2api-failure.png"), fullPage: true });
    throw error;
  }
}
async function fits(page, locator) {
  assert(await locator.evaluate((box) => box.scrollWidth <= box.clientWidth + 1), "the feature fits its container");
  assert(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), "the page has no sideways scroll");
}
async function wheelTo(page, locator) {
  for (let attempt = 0; attempt < 16; attempt++) {
    await locator.waitFor({ state: "visible" });
    const box = await locator.boundingBox();
    if (!box) continue;
    const view = await locator.evaluate((node) => {
      const box = (node.closest(".ebody") || document.querySelector("#view-usage"))?.getBoundingClientRect();
      return box && { x: box.x, y: box.y, width: box.width, height: box.height };
    });
    assert(view, "the scrollable view is visible");
    const header = await page.locator("header.top").boundingBox(), footer = await page.locator("footer.foot").boundingBox();
    const top = Math.max(view.y, header ? header.y + header.height : 0);
    const bottom = Math.min(page.viewportSize().height, view.y + view.height, footer?.y ?? Infinity);
    if (box.y >= top + 2 && box.y + box.height <= bottom - 2) return;
    const node = await locator.elementHandle();
    try {
      await page.mouse.move(view.x + view.width / 2, (top + bottom) / 2);
      await page.mouse.wheel(0, box.y < top + 2 ? Math.min(-120, box.y - top - 100) : Math.max(120, box.y + box.height - bottom + 100));
      await page.waitForFunction(({ node, top }) => !node.isConnected || Math.abs(node.getBoundingClientRect().top - top) > 1,
        { node, top: box.y }, { timeout: 1500 });
    } finally { await node.dispose(); }
  }
  assert.fail("the target did not enter the viewport after real wheel scrolling");
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(engine + ": group overview defaults to linked Keys and keeps filters separate from monitoring", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      for (const width of [1410, 420]) {
        await t.test(`${lang} ${width}px`, async (st) => {
          const f = fixture(true), p = f.providers[0], mon = f.monitor.accounts[0];
          p.name = "哈基米biu"; p.keyList = [p.keyList[0]]; p.priceRate = 1;
          p.keyList[0].name = "sk-6...52fa";
          p.sub2api = { session: account.id, version: binding.version, keys: [
            { id: "key-one", groupId: "105", groupName: "不降智的gpt" },
          ] };
          f.accounts[0].email = "示例帐号"; f.accounts[0].base = "https://o10.top";
          p.chat = "https://o10.top/v1";
          mon.site = f.accounts[0].base;
          mon.groups = [{ id: "105", name: "不降智的gpt", rate: .15, visible: true, lastSuccess: stamp },
            { id: "unlinked", name: "Standard", rate: 1, visible: true, lastSuccess: stamp }];
          mon.sources[0].keys = [{ id: "key-one", groupId: "105", rate: .15, lastSuccess: stamp }];
          mon.sources[0].activeKeyIds = ["key-one"];
          delete mon.observation;
          const { page, context, errors } = await open(browser, f, lang, width, "usage", false);
          st.after(() => context.close());
          if (width === 1410) await page.setViewportSize({ width, height: 1116 });
          const tr = (key) => word(lang, key), pane = page.locator("#sub2apiPane");
          await pane.locator('[data-group="105"]').waitFor();
          const linked = pane.getByRole("button", { name: tr("Linked keys"), exact: true });
          const all = pane.getByRole("button", { name: tr("All panel groups"), exact: true });
          assert.equal(await linked.getAttribute("aria-pressed"), "true");
          assert.equal(await pane.locator(".s2-group").count(), 1);
          assert.match(await pane.locator(".s2-site").textContent(), /https:\/\/o10.top · 哈基米biu/);
          assert.doesNotMatch(await pane.locator(".s2-table").textContent(), /哈基米biu/,
            "source names appear at the panel address, never on each Key");
          assert.match(await pane.locator(".s2-local .s2-value").textContent(), /^1×$/);
          assert.equal(await pane.locator(".s2-apply").textContent(),
            tr("Set locally to {rate}").replace("{rate}", (.15).toLocaleString(lang) + "×"));
          assert.equal(await pane.locator(".s2-apply svg").count(), 1);
          assert.equal(await pane.locator(".s2-differences").textContent(), tr("{n} rate differences").replace("{n}", 1));
          await page.evaluate(() => { usageEvery = 0; localStorage.setItem("magpie.navLayout", "top"); navPick = "top"; layoutNav(); });
          await page.locator("#nav .on").evaluate((node) => node.scrollIntoView({ inline: "nearest", block: "nearest" }));
          await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
          await fits(page, pane);
          if (process.env.ARTIFACT_DIR && ["en", "zh"].includes(lang)) {
            await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-overview-${engine}-${lang}-${width}.png`), fullPage: true });
          }
          await all.click();
          assert.equal(await pane.locator(".s2-group").count(), 2);
          assert.equal(await all.getAttribute("aria-pressed"), "true");
          await linked.click();
          assert.equal(await pane.locator(".s2-group").count(), 1);
          assert.equal(posted(f, "monitor").length, 0, "display filters never change account monitoring");
          assert.equal(f.configs[account.id].enabled, true);
          const more = pane.getByRole("button", { name: tr("Account actions"), exact: true });
          await wheelTo(page, more);
          await more.click();
          assert.equal(await more.getAttribute("aria-expanded"), "true");
          await page.getByRole("menuitemradio", { name: tr("Sign out"), exact: true }).click();
          const confirm = page.getByRole("alertdialog");
          await confirm.getByRole("button", { name: tr("Cancel"), exact: true }).click();
          assert.equal(posted(f, "logout").length, 0, "the actions menu retains sign-out confirmation");
          p.sub2api.keys[0].groupId = "";
          await page.evaluate(() => window.Sub2API.renderUsage(document.querySelector("#sub2apiPane"), false, true));
          await pane.getByText(tr("No groups with linked keys"), { exact: true }).waitFor();
          assert.equal(await pane.locator(".s2-group").count(), 0);
          await all.click();
          assert.equal(await pane.locator('.s2-group[data-group=""] .s2-keys [data-key="key-one"]').count(), 1,
            "All groups keeps an unassigned Key reachable");
          await fits(page, pane);
          assert.deepEqual(errors, []);
        });
      }
    }
  });
  test(engine + ": one source has independent Key groups and applied rates", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(true), p = f.providers[0], source = f.monitor.accounts[0].sources[0];
    p.sub2api = { session: account.id, version: binding.version, keys: [
      { id: "key-one", groupId: "a", groupName: "Premium", priceRate: .6 },
      { id: "key-two", groupId: "b", groupName: "Standard" },
    ] };
    delete source.groupId; delete source.groupName; delete source.rate;
    source.conflict = true;
    Object.assign(source.keys[0], { groupId: "a", groupName: "Premium" });
    Object.assign(source.keys[1], { groupId: "b", groupName: "Standard", rate: 1.5 });
    const { page, context, errors } = await open(browser, f, "en", 420, "usage");
    t.after(() => context.close());
    const pane = page.locator("#sub2apiPane"), a = pane.locator('[data-group="a"]'), b = pane.locator('[data-group="b"]');
    await a.waitFor();
    assert.equal(await a.locator('.s2-billing [data-key="key-one"]').count(), 1);
    assert.equal(await a.locator('[data-key="key-two"]').count(), 0, "the second Key belongs to its own group");
    assert.match(await b.locator('.s2-billing [data-key="key-two"]').textContent(), /1.5×/);
    const local = b.locator('.s2-local [data-key="key-two"]');
    assert.match(await local.textContent(), /1.2×/, "an unset Key rate inherits the source fallback");
    if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-key-groups-${engine}-420.png`), fullPage: true });
    const apply = local.getByRole("button", { name: "Apply rate", exact: true });
    await wheelTo(page, apply);
    await apply.click();
    await waitPost(page, "apply-key-rate", () => page.getByRole("alertdialog").getByRole("button", { name: "Apply rate", exact: true }).click());
    assert.deepEqual(posted(f, "apply-key-rate")[0].body, { provider: "relay", keyId: "key-two", rate: 1.5,
      expectedRate: 1.2, bindingVersion: binding.version, expectedRevision: "revision-1", expectedGeneration: account.generation });
    assert.equal(localRate(p, "key-one"), .6);
    assert.equal(localRate(p, "key-two"), 1.5);
    assert.equal(p.priceRate, relay.priceRate);
    await page.locator('#nav [data-view="providers"]').click();
    await page.locator(".row.provider", { hasText: "Relay" }).click();
    const first = page.locator('.s2-editor [data-key="key-one"]').getByRole("button", { name: "Panel group", exact: true });
    await wheelTo(page, first);
    await first.click();
    await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Standard/ }).click());
    assert.equal(boundKeys(p).find((k) => k.id === "key-one").groupId, "b");
    assert.equal(boundKeys(p).find((k) => k.id === "key-two").groupId, "b");
    await wheelTo(page, first);
    await first.click();
    await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: "No group selected", exact: true }).click());
    assert.deepEqual(boundKeys(p), [
      { id: "key-one", groupId: "", groupName: "", priceRate: .6 },
      { id: "key-two", groupId: "b", groupName: "Standard", priceRate: 1.5 },
    ], "clearing one Key's group preserves both local rates and the other Key's group");
    await fits(page, page.locator(".s2-editor"));
    assert.deepEqual(errors, []);
  });
  test(engine + ": each Key stays aligned with its billing and local rate when another Key has an error", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const width of [1100, 420]) {
      await t.test(width + "px", async (st) => {
        const f = fixture(true);
        f.monitor.accounts[0].sources[0].keys[0].error =
          "This Key's billing request failed and its long diagnostic must wrap without moving another Key's values into its row.";
        const { page, context, errors } = await open(browser, f, "en", width, "usage");
        st.after(() => context.close());
        const row = page.locator('#sub2apiPane [data-group="a"]');
        await row.waitFor();
        for (const id of ["key-one", "key-two"]) {
          const box = (cell) => row.locator(`.${cell} [data-key="${id}"]`).boundingBox();
          const [key, billing, local] = await Promise.all([box("s2-keys"), box("s2-billing"), box("s2-local")]);
          assert(Math.abs(billing.y - local.y) <= 1, "the same Key's billing and local rates share a row");
          if (width > 900) assert(Math.abs(key.y - billing.y) <= 1, "desktop Key names share that row too");
          else assert(key.y < billing.y, "a narrow row names the Key immediately above its rates");
        }
        const first = await row.locator('.s2-billing [data-key="key-one"]').boundingBox();
        const second = await row.locator('.s2-billing [data-key="key-two"]').boundingBox();
        assert(second.y >= first.y + first.height, "a wrapped error cannot overlap the next Key");
        await fits(page, page.locator("#sub2apiPane"));
        if (process.env.ARTIFACT_DIR) await page.screenshot({
          path: path.join(process.env.ARTIFACT_DIR, `sub2api-key-alignment-${engine}-${width}.png`), fullPage: true,
        });
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": Usage group menus reflect each Key binding", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const width of [1100, 420]) {
      await t.test(width + "px: selected Keys match the current group", async (st) => {
        const f = fixture(true);
        f.providers[0].sub2api = { session: account.id, version: binding.version, keys: [
          { id: "key-one", groupId: "a", groupName: "Premium", priceRate: .6 },
          { id: "key-two", groupId: "a", groupName: "Premium", priceRate: 1.5 },
        ] };
        const { page, context, errors } = await open(browser, f, "en", width, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        const link = (id) => pane.locator(`[data-group="${id}"]`).getByRole("button", { name: "Link key", exact: true });
        const openGroup = async (id) => { await wheelTo(page, link(id)); await link(id).click(); };
        const waitGroup = (id, group) => page.waitForFunction(({ id, group }) =>
          providers.providers[0].sub2api?.keys?.find((key) => key.id === id)?.groupId === group, { id, group });
        await openGroup("a");
        const items = page.getByRole("menu").locator(".pm-item");
        assert.equal(await items.filter({ hasText: "Relay · First" }).getAttribute("aria-checked"), "true",
          "a Key already bound to this group is checked in its link menu");
        assert.equal(await items.filter({ hasText: "Relay · Second" }).getAttribute("aria-checked"), "true",
          "multiple Keys in the same group are checked together");
        if (process.env.ARTIFACT_DIR) {
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-group-ticks-${engine}-${width}.png`), fullPage: true });
        }
        await page.keyboard.press("Escape");
        assert.equal(posted(f, "bind-key").length, 0, "closing a group menu without a change does not write");
        await openGroup("b");
        for (const name of ["First", "Second"]) {
          assert.equal(await items.filter({ hasText: "Relay · " + name }).getAttribute("aria-checked"), "false",
            "a Key belonging to another group is not checked");
        }
        let entered, release, hold = true;
        const requested = new Promise((resolve) => entered = resolve), gate = new Promise((resolve) => release = resolve);
        st.after(release);
        await page.route("**/api/sub2api/bind-key", async (route) => {
          if (hold) { hold = false; entered(); await gate; }
          await route.fallback();
        });
        const moving = waitPost(page, "bind-key", () => page.getByRole("menuitemcheckbox", { name: /Relay · First/ }).click());
        await requested;
        assert.equal(await page.getByRole("menu").count(), 0, "the menu closes as soon as one Key changes");
        assert(await link("b").isDisabled(), "a pending Key move cannot reopen the old group menu");
        release();
        await moving;
        await waitGroup("key-one", "b");
        assert.deepEqual(boundKeys(f.providers[0]).map((key) => [key.id, key.groupId]), [["key-one", "b"], ["key-two", "a"]],
          "moving a Key preserves its sibling's group");
        await openGroup("b");
        assert.equal(await items.filter({ hasText: "Relay · First" }).getAttribute("aria-checked"), "true");
        assert.equal(await items.filter({ hasText: "Relay · Second" }).getAttribute("aria-checked"), "false");
        await waitPost(page, "bind-key", () => page.getByRole("menuitemcheckbox", { name: /Relay · First/ }).click());
        await waitGroup("key-one", "");
        assert.equal(posted(f, "bind-key").at(-1).body.groupId, "");
        assert.equal(posted(f, "bind-key").at(-1).body.groupName, "");
        assert.deepEqual(boundKeys(f.providers[0]).map((key) => [key.id, key.groupId]), [["key-one", ""], ["key-two", "a"]],
          "unchecking a Key clears only its own group");
        assert.deepEqual(boundKeys(f.providers[0]).map((key) => key.priceRate), [.6, 1.5],
          "group moves and removals preserve both Key price overrides");
        await page.locator('#nav [data-view="providers"]').click();
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const editorPick = page.locator('.s2-editor [data-key="key-one"]').getByRole("button", { name: "Panel group", exact: true });
        await wheelTo(page, editorPick);
        await editorPick.click();
        await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Standard/ }).click());
        await waitGroup("key-one", "b");
        await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
        await page.locator('#nav [data-view="usage"]').click();
        await openGroup("b");
        assert.equal(await items.filter({ hasText: "Relay · First" }).getAttribute("aria-checked"), "true",
          "the Usage menu reflects the group selected in the source editor");
        assert.equal(await items.filter({ hasText: "Relay · Second" }).getAttribute("aria-checked"), "false");
        await page.keyboard.press("Escape");
        await openGroup("a");
        assert.equal(await items.filter({ hasText: "Relay · First" }).getAttribute("aria-checked"), "false");
        assert.equal(await items.filter({ hasText: "Relay · Second" }).getAttribute("aria-checked"), "true");
        assert.equal(posted(f, "bind-key").length, 3, "each explicit group change writes only the chosen Key once");
        await fits(page, pane);
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": an account without sources can query groups and control monitoring", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(true);
    f.providers = [];
    f.previewAccount = { ...structuredClone(f.monitor.accounts[0]), settingsRevision: "", sources: [] };
    f.monitor.accounts = [];
    f.configs[account.id] = { configured: false, enabled: false, groups: [], revision: "", generation: account.generation };
    const { page, context, errors } = await open(browser, f, "en", 420, "usage");
    t.after(() => context.close());
    const pane = page.locator("#sub2apiPane");
    const watch = pane.getByRole("switch", { name: "Monitor this account", exact: true });
    await watch.waitFor();
    assert(await watch.isEnabled(), "an account's monitor switch does not require a source or Key");
    const query = pane.locator(".s2-account-head").getByRole("button", { name: "Query once", exact: true });
    await waitPost(page, "preview", () => query.click());
    await pane.locator('[data-group="b"]').waitFor();
    const group = pane.locator('[data-group="a"]');
    if (process.env.ARTIFACT_DIR) {
      await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-unlinked-${engine}-420.png`), fullPage: true });
    }
    const boxes = await group.evaluate((row) => [".s2-declared", ".s2-billing .s2-key", ".s2-local .s2-key"]
      .map((selector) => row.querySelector(selector).getBoundingClientRect().top));
    assert(Math.max(...boxes) - Math.min(...boxes) < 2, "an unlinked group's rates remain on the same row");
    assert.equal(f.configs[account.id].enabled, false, "a one-time group query does not enable monitoring");
    assert.equal(posted(f, "monitor").length, 0);
    assert.equal(await pane.locator(".s2-group").getByRole("checkbox").count(), 0);
    await waitPost(page, "monitor", () => watch.check());
    await page.waitForFunction(() => document.querySelector(".s2-account-head input[type=checkbox]")?.checked);
    assert.deepEqual(posted(f, "monitor")[0].body, { session: account.id, enabled: true, groups: [],
      settingsRevision: "", expectedGeneration: account.generation });
    assert.equal(f.configs[account.id].enabled, true);
    await waitPost(page, "monitor", () => watch.uncheck());
    assert.equal(f.configs[account.id].enabled, false);
    assert.deepEqual(f.configs[account.id].groups, []);
    assert.deepEqual(errors, []);
  });
  test(engine + ": Usage group links only offer Keys from the account API origin", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const source = (id, balanceURL, chat = relay.chat, extra = {}) => {
      const p = { ...structuredClone(relay), id, name: id, balanceURL, chat, ...extra };
      delete p.sub2api;
      return p;
    };
    await t.test("full API origins select individual Keys independently of balance URLs", async (st) => {
      const f = fixture(true);
      f.monitor.accounts[0].sources = [];
      f.providers = [
        source("Same balance", "https://panel.example.com/custom-balance", "https://api.other.test/v1"),
        source("Same chat", ""),
        source("Default port", "", "https://PANEL.EXAMPLE.COM:443/v1"),
        source("Same responses", "", "", { responses: "https://panel.example.com/v1/responses" }),
        source("Other site", "", "https://other.test/v1"),
        source("Other subdomain", "", "https://api.panel.example.com/v1"),
        source("Suffix lookalike", "", "https://panel.example.com.other.test/v1"),
        source("Other port", "", "https://panel.example.com:8443/v1"),
        source("HTTP downgrade", "", "http://panel.example.com/v1"),
        source("Invalid URL", "", "not-a-url"),
        source("URL credentials", "", "https://user:secret@panel.example.com/v1"),
        source("External balance", "https://other.test/api/balance", relay.chat),
        source("First API is external", "", "https://other.test/v1", { responses: relay.chat }),
      ];
      const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      const link = page.locator('#sub2apiPane [data-group="a"]').getByRole("button", { name: "Link key", exact: true });
      await link.click();
      const menu = page.getByRole("menu");
      assert.deepEqual(await menu.getByRole("menuitemcheckbox").locator(".pm-name").allTextContents(),
        ["Same chat", "Default port", "Same responses", "External balance"].flatMap((name) => [name + " · First", name + " · Second"]),
        "a group only lists Keys whose API endpoint belongs to this origin");
      await waitPost(page, "bind-key", () => menu.getByRole("menuitemcheckbox", { name: "Same chat · First", exact: true }).click());
      const body = posted(f, "bind-key")[0].body;
      assert.equal(body.provider, "Same chat");
      assert.equal(body.session, account.id);
      assert.equal(body.groupId, "a");
      assert.equal(body.keyId, "key-one", "the explicit selection links only this Key");
      assert.deepEqual(boundKeys(f.providers.find((p) => p.id === "Same chat")).map((k) => k.id), ["key-one"]);
      assert.deepEqual(errors, []);
    });
    await t.test("no matching origin leaves no provider to bind", async (st) => {
      const f = fixture(true);
      f.monitor.accounts[0].sources = [];
      f.providers = [source("Only another site", account.base, "https://other.test/v1")];
      const { page, context, errors } = await open(browser, f, "en", 420, "usage");
      st.after(() => context.close());
      const link = page.locator('#sub2apiPane [data-group="a"]').getByRole("button", { name: "Link key", exact: true });
      await wheelTo(page, link);
      await link.click();
      assert.equal(await page.getByRole("menu").getByRole("menuitemcheckbox").count(), 0,
        "an empty same-origin candidate list never offers another site's source");
      await page.keyboard.press("Escape");
      assert.equal(posted(f, "bind-key").length, 0);
      assert.deepEqual(errors, []);
    });
    await t.test("the Chinese label identifies the key billing multiplier", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "zh", 420, "usage");
      st.after(() => context.close());
      const label = page.locator('#sub2apiPane [data-group="a"] .s2-billing .s2-label').first();
      await label.waitFor();
      assert.equal(await label.textContent(), "Key 账单倍率");
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": source editor accounts match the panel and saved balance origins", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const candidates = () => [
      structuredClone(account),
      { ...account, id: "session-two", email: "second@example.com" },
      { ...account, id: "session-default-port", email: "canonical@example.com", base: "https://PANEL.EXAMPLE.COM:443/" },
      { ...account, id: "session-api", email: "api@example.com", base: "https://api.other.example" },
      { ...account, id: "session-subdomain", email: "subdomain@example.com", base: "https://sub.panel.example.com" },
      { ...account, id: "session-http", email: "http@example.com", base: "http://panel.example.com" },
      { ...account, id: "session-port", email: "port@example.com", base: "https://panel.example.com:444" },
      { ...account, id: "session-userinfo", email: "userinfo@example.com", base: "https://user:secret@panel.example.com" },
      { ...account, id: "session-out", email: "out@example.com", state: "logged_out" },
    ];
    for (const width of [1100, 420]) {
      await t.test(width + "px: only same-site signed-in accounts are offered", async (st) => {
        const f = fixture();
        f.accounts = candidates();
        f.providers[0].chat = "https://api.other.example/v1";
        const { page, context, errors } = await open(browser, f, "en", width);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        const pick = box.getByRole("button", { name: "Signed-in accounts", exact: true });
        await wheelTo(page, pick);
        await pick.click();
        const choices = page.getByRole("menuitemradio");
        assert.equal(await choices.count(), 3, "the balance URL takes precedence over the API URL and excludes other origins and signed-out accounts");
        for (const email of [account.email, "second@example.com", "canonical@example.com"]) {
          assert.equal(await choices.filter({ hasText: email }).count(), 1, "same-site account " + email + " remains available");
        }
        await fits(page, box);
        if (process.env.ARTIFACT_DIR) {
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-account-filter-${engine}-${width}.png`), fullPage: true });
        }
        await choices.filter({ hasText: "second@example.com" }).click();
        const use = box.getByRole("button", { name: "Link account", exact: true });
        await waitPost(page, "bind", () => use.click());
        assert.equal(posted(f, "bind")[0].body.session, "session-two");
        assert.deepEqual(errors, []);
      });
      await t.test(width + "px: editing the address invalidates account choices without resetting credentials", async (st) => {
        const f = fixture();
        f.accounts = candidates();
        const { page, context, errors } = await open(browser, f, "en", width);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        const pick = box.getByRole("button", { name: "Signed-in accounts", exact: true });
        await wheelTo(page, pick);
        await pick.click();
        await page.getByRole("menuitemradio").filter({ hasText: account.email }).click();
        const base = box.getByLabel("Panel address", { exact: true });
        const email = box.getByLabel("Panel email", { exact: true });
        const password = box.getByLabel("Panel password", { exact: true });
        const use = box.getByRole("button", { name: "Link account", exact: true });
        assert(await use.isEnabled());
        await email.fill("typed@example.com");
        await password.fill("typed-password");
        const inputNode = await password.elementHandle(), oldUse = await use.elementHandle();
        st.after(() => inputNode.dispose());
        st.after(() => oldUse.dispose());
        await wheelTo(page, pick);
        await pick.click();
        await page.getByRole("menuitemradio").filter({ hasText: account.email }).waitFor();
        await base.fill("https://api.other.example");
        assert.equal(await page.getByRole("menuitemradio").count(), 0, "changing the address closes its obsolete menu");
        assert(await pick.isDisabled(), "neither the old panel account nor an account from another balance origin remains selectable");
        assert(await use.isDisabled(), "the old account selection is cleared immediately");
        await oldUse.evaluate((button) => button.onclick());
        assert.equal(posted(f, "bind").length, 0, "an obsolete linking control cannot bind the previously selected account");
        assert(await inputNode.evaluate((input) => input.isConnected && input.value === "typed-password"),
          "the password input stays mounted while account choices refresh");
        assert.equal(await email.inputValue(), "typed@example.com");
        await base.fill("https://PANEL.EXAMPLE.COM:443");
        assert(await pick.isEnabled(), "the same origin restores its eligible accounts");
        assert(await use.isDisabled(), "returning to an origin does not revive a cleared account selection");
        await pick.click();
        assert.equal(await page.getByRole("menuitemradio").count(), 3);
        await base.fill("https://user:secret@panel.example.com");
        assert.equal(await page.getByRole("menuitemradio").count(), 0);
        assert(await pick.isDisabled(), "an address containing userinfo cannot offer panel accounts");
        assert.equal(await password.inputValue(), "typed-password");
        assert.equal(await email.inputValue(), "typed@example.com");
        assert.equal(posted(f, "bind").length, 0);
        await fits(page, box);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("without a balance URL the first saved API origin selects the accounts", async (st) => {
      const f = fixture();
      f.accounts = candidates();
      Object.assign(f.providers[0], { balanceURL: "", chat: "", responses: "https://panel.example.com/v1",
        anthropic: "https://api.other.example/v1" });
      const { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      const pick = box.getByRole("button", { name: "Signed-in accounts", exact: true });
      await wheelTo(page, pick);
      await pick.click();
      assert.equal(await page.getByRole("menuitemradio").count(), 3);
      assert.equal(await page.getByRole("menuitemradio").filter({ hasText: "api@example.com" }).count(), 0);
      assert.deepEqual(errors, []);
    });
    await t.test("a saved balance URL containing userinfo cannot offer accounts", async (st) => {
      const f = fixture();
      f.accounts = candidates();
      f.providers[0].balanceURL = "https://user:secret@panel.example.com/api/v1/custom-balance";
      const { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      const pick = box.getByRole("button", { name: "Signed-in accounts", exact: true });
      await pick.waitFor();
      assert(await pick.isDisabled(), "deriving the displayed panel address must not remove userinfo from the saved source's eligibility check");
      assert(await box.getByRole("button", { name: "Link account", exact: true }).isDisabled());
      assert.equal(posted(f, "bind").length, 0);
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": source editor group previews stay local and preserve account bindings", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    await t.test("an unconfigured account can query and bind groups without enabling monitoring", async (st) => {
      const f = fixture(true);
      f.previewAccount = structuredClone(f.monitor.accounts[0]);
      f.previewAccount.settingsRevision = "";
      f.monitor.accounts = [];
      f.configs[account.id] = { configured: false, enabled: false, groups: [], revision: "", generation: account.generation };
      Object.assign(f.providers[0].sub2api, { groupId: "", groupName: "", keyIds: [] });
      const settings = structuredClone(f.configs), snapshot = structuredClone(f.monitor);
      const { page, context, errors } = await open(browser, f, "en", 420);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor"), pick = box.getByRole("button", { name: "Panel group", exact: true }).first();
      await pick.waitFor();
      const query = box.getByRole("button", { name: "Query once", exact: true });
      assert.equal(await query.count(), 1, "the source editor offers a group query before monitoring has a snapshot");
      assert.equal(posted(f, "preview").length, 0, "opening the source does not query the panel");
      await wheelTo(page, query);
      await waitPost(page, "preview", () => query.click());
      assert.deepEqual(posted(f, "preview")[0].body, {
        session: account.id, expectedGeneration: account.generation, settingsRevision: "",
      }, "group discovery queries the panel account without source billing");
      await wheelTo(page, pick);
      await pick.click();
      await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Premium/ }).click());
      await pick.filter({ hasText: "Premium" }).waitFor();
      assert.equal(posted(f, "bind-key").at(-1).body.groupId, "a");
      assert.equal(posted(f, "bind-key").at(-1).body.keyId, "key-one", "only the explicitly selected Key joins the group");
      assert.deepEqual(f.configs, settings, "a one-time query does not configure or enable monitoring");
      assert.deepEqual(f.monitor, snapshot, "a one-time query does not change the background snapshot or history");
      assert.equal(posted(f, "monitor").length, 0);
      await fits(page, box);
      assert.deepEqual(errors, []);
    });
    for (const failure of ["resource", "request"]) {
      await t.test(failure + " group errors remain visible and retryable", async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 420);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.getByRole("button", { name: "Panel group", exact: true }).first().waitFor();
        const query = box.getByRole("button", { name: "Query once", exact: true });
        assert.equal(await query.count(), 1, "the source editor offers a retryable group query");
        await page.route("**/api/sub2api/preview", (route) => route.fulfill(failure === "request"
          ? { status: 503, json: { error: "The group query failed." } }
          : { json: { ...structuredClone(f.monitor.accounts[0]), groups: [], groupsError: "The group query failed." } }));
        await wheelTo(page, query);
        await waitPost(page, "preview", () => query.click());
        await box.getByText(failure === "request" ? "The group query failed." : "Groups: The group query failed.", { exact: true }).waitFor();
        assert(await query.isEnabled(), "a failed query releases the retry button");
        assert.equal(posted(f, "monitor").length, 0);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("linking the current account cannot clear its group and confirmed keys", async (st) => {
      const f = fixture(true), before = structuredClone(f.providers[0].sub2api);
      const { page, context, errors } = await open(browser, f, "en", 420);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const use = page.locator(".s2-editor").getByRole("button", { name: "Link account", exact: true });
      await use.waitFor();
      assert(await use.isDisabled(), "the already linked account cannot be relinked with empty group and keys");
      await use.evaluate((button) => button.onclick());
      assert.equal(posted(f, "bind").length, 0, "the click handler also guards a duplicate link");
      assert.deepEqual(f.providers[0].sub2api, before);
      assert.deepEqual(errors, []);
    });
    for (const invalidation of ["close", "generation", "settings"]) {
      await t.test(invalidation + " discards a pending editor group query", async (st) => {
        const f = fixture(true);
        f.previewAccount = structuredClone(f.monitor.accounts[0]);
        f.monitor.accounts = [];
        const { page, context, errors } = await open(browser, f, "en", 420);
        st.after(() => context.close());
        const row = page.locator(".row.provider", { hasText: "Relay" });
        await row.click();
        const box = page.locator(".s2-editor"), pick = box.getByRole("button", { name: "Panel group", exact: true }).first();
        await pick.waitFor();
        const query = box.getByRole("button", { name: "Query once", exact: true });
        assert.equal(await query.count(), 1, "the source editor offers cancellable group discovery");
        let release, delivered;
        const gate = new Promise((resolve) => release = resolve), completed = new Promise((resolve) => delivered = resolve);
        st.after(release);
        await page.route("**/api/sub2api/preview", async (route) => {
          await gate;
          await route.fulfill({ json: { ...f.previewAccount, sources: [] } });
          delivered();
        });
        const requested = page.waitForRequest("**/api/sub2api/preview");
        await wheelTo(page, query);
        await query.click();
        await requested;
        if (invalidation === "close") {
          await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
          await page.locator(".editor").waitFor({ state: "hidden" });
          await row.click();
          await pick.waitFor();
        } else {
          if (invalidation === "generation") f.accounts[0].generation++;
          else f.configs[account.id].revision = "settings-new";
          await page.evaluate(async () => { await (await import("/sub2api/common.js")).read(); });
        }
        release();
        await completed;
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        await wheelTo(page, pick);
        await pick.click();
        assert.equal(await page.getByRole("menuitemradio", { name: /Standard/ }).count(), 0,
          "an invalidated query cannot add its group choices to the editor");
        assert.equal(f.monitor.accounts.length, 0);
        assert.equal(posted(f, "monitor").length, 0);
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": late editor content stays inside the scrolling body", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(), { page, context, errors } = await open(browser, f, "en", 420);
    t.after(() => context.close());
    await page.locator(".row.provider", { hasText: "Relay" }).click();
    const box = page.locator(".s2-editor");
    await box.locator("summary").click();
    await box.getByLabel("Panel password", { exact: true }).waitFor();
    assert(await box.evaluate((node) => node.closest(".ebody")?.parentElement === node.closest(".editor")),
      "late panel fields must belong to the editor's scrolling body");
    assert.deepEqual(errors, []);
  });
  test(engine + ": existing preset account tokens keep replacement, removal and cancellation", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const action of ["replace", "remove", "cancel"]) {
      await t.test(action, async (st) => {
        const f = fixture();
        f.providers[0].preset = "aihubmix";
        f.providers[0].balanceURL = "";
        f.presets = [{ id: "aihubmix", name: "AiHubMix", chat: relay.chat, models: relay.models }];
        const { page, context, errors } = await open(browser, f, "en", 420);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const token = page.getByPlaceholder("saved · paste a new one to replace it", { exact: true });
        await token.waitFor();
        assert.equal(await page.locator(".s2-editor").count(), 0, "a preset's existing token is not a panel login");
        assert.equal(f.panelReads, 0, "editing a saved token needs no panel request");
        assert.match(await token.locator("../..").textContent(), /AiHubMix/);
        if (action === "cancel") {
          await token.press("Escape");
          await page.locator(".editor").waitFor({ state: "hidden" });
          assert.equal(f.posted.length, 0);
        } else {
          if (action === "replace") await token.fill("replacement-account-token");
          else await token.locator("..").getByRole("button", { name: "Remove", exact: true }).click();
          const response = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/provider/save");
          await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
          await response;
          const body = posted(f, "provider/save").at(-1).body;
          assert.equal(body.balanceToken, action === "replace" ? "replacement-account-token" : undefined);
          assert.equal(body.clearBalanceToken, action === "remove" ? true : undefined);
        }
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": Sub2API panel lifecycle and monitoring", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    await t.test("read-only capabilities hide credential inputs and prevent writes", async (st) => {
      const f = fixture(true);
      f.capability = { writable: false, reason: "Sub2API changes require the Windows desktop app." };
      const { page, context, errors } = await open(browser, f, "en", 420);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.getByText(f.capability.reason, { exact: true }).waitFor();
      assert.equal(await box.locator("input[type=password]").count(), 0);
      assert.equal(await box.getByRole("button", { name: "Sign in", exact: true }).count(), 0);
      await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
      await page.evaluate(async () => { usageTab = "sub2api"; await show("usage"); });
      const pane = page.locator("#sub2apiPane");
      await pane.getByText(f.capability.reason, { exact: true }).waitFor();
      assert(await pane.getByRole("switch", { name: "Monitor this account" }).isDisabled());
      assert(await pane.getByRole("button", { name: "Account actions", exact: true }).isDisabled());
      assert(await pane.locator('[data-group="a"] .s2-apply').first().isDisabled());
      await page.locator("#usageReload").click();
      assert.equal(f.posted.length, 0, "a local snapshot refresh never writes in read-only mode");
      assert.deepEqual(errors, []);
    });
    await t.test("an unavailable browser host stays disabled without opening a login", async (st) => {
      const f = fixture();
      f.capability.browser = false;
      f.capability.browserReason = "Browser verification is unavailable in this build.";
      const { page, context, errors } = await open(browser, f, "en", 420);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      assert(await box.getByRole("button", { name: "Sign in with browser verification", exact: true }).isDisabled());
      await box.getByText(f.capability.browserReason, { exact: true }).waitFor();
      assert.equal(f.posted.length, 0);
      assert.deepEqual(errors, []);
    });
    for (const mode of ["provider", "account"]) {
      await t.test(`explicit browser verification signs in the ${mode} without sending credentials`, async (st) => {
        const f = fixture(mode === "account");
        if (mode === "account") { f.providers = []; f.accounts[0].state = "challenge"; }
        const { page, context, errors } = await open(browser, f, "en", 420, mode === "account" ? "usage" : "providers");
        st.after(() => context.close());
        let box;
        if (mode === "provider") {
          await page.locator(".row.provider", { hasText: "Relay" }).click();
          box = page.locator(".s2-editor");
          await box.locator("summary").click();
          await box.getByLabel("Panel password", { exact: true }).fill("discarded-secret");
          await box.getByRole("checkbox", { name: "Remember this password on this computer" }).check();
          assert(await box.evaluate((node) => !!(node.compareDocumentPosition(node.closest(".editor").querySelector(":scope > .bar")) & Node.DOCUMENT_POSITION_FOLLOWING)),
            "the panel is inserted before the editor's save bar");
          if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `browser-${engine}-420.png`), fullPage: true });
        } else {
          box = page.locator("#sub2apiPane");
          await box.getByRole("button", { name: "Sign in again", exact: true }).click();
        }
        assert.equal(f.posted.length, 0, "the verification window is never opened automatically");
        const action = mode === "account" ? "browser-account" : "browser";
        await waitPost(page, action, () => box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click());
        assert.deepEqual(posted(f, action)[0].body, mode === "account"
          ? { session: account.id, expectedGeneration: account.generation }
          : { provider: "relay", expectedRevision: "revision-1", base: account.base });
        if (mode === "account") await box.locator(".s2-account-status .s2-state.ready").waitFor();
        else await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
        assert.equal(await box.locator("input[type=password]").count(), 0);
        assert.equal(posted(f, "login").length + posted(f, "login-account").length + posted(f, "bind").length, 0);
        assert.deepEqual(errors, []);
      });
      await t.test(`leaving the ${mode} cancels browser verification before an attempt is returned`, async (st) => {
        const f = fixture(mode === "account");
        if (mode === "account") { f.providers = []; f.accounts[0].state = "challenge"; }
        const { page, context, errors } = await open(browser, f, "en", 420, mode === "account" ? "usage" : "providers");
        st.after(() => context.close());
        let box;
        if (mode === "provider") {
          await page.locator(".row.provider", { hasText: "Relay" }).click();
          box = page.locator(".s2-editor");
          await box.locator("summary").click();
        } else {
          box = page.locator("#sub2apiPane");
          await box.getByRole("button", { name: "Sign in again", exact: true }).click();
        }
        const action = mode === "account" ? "browser-account" : "browser";
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        await page.route(`**/api/sub2api/${action}`, async (route) => {
          await gate;
          await route.fulfill({ json: { state: "ready", account } });
        });
        const requested = page.waitForRequest(`**/api/sub2api/${action}`);
        await box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click();
        const pending = await requested;
        await box.getByText("Complete sign-in in the verification window.", { exact: true }).waitFor();
        assert.equal(await box.locator("input[type=password]").count(), 0);
        const aborted = page.waitForEvent("requestfailed", { predicate: (r) => r === pending });
        if (mode === "account") await page.locator('#nav [data-view="providers"]').click();
        else await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
        await aborted;
        release();
        assert.equal(posted(f, "cancel").length, 0, "request cancellation works before the core returns its attempt");
        assert.equal(f.accounts.length, mode === "account" ? 1 : 0);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("switching from 2FA to browser verification cancels the old attempt and remains cancellable", async (st) => {
      const f = fixture(), { page, context, errors } = await open(browser, f, "en", 420);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      await box.getByLabel("Panel email", { exact: true }).fill(account.email);
      await box.getByLabel("Panel password", { exact: true }).fill("discarded-secret");
      await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
      await box.getByLabel("Six-digit verification code").fill("123456");
      let release;
      const gate = new Promise((resolve) => release = resolve);
      st.after(release);
      await page.route("**/api/sub2api/browser", async (route) => {
        await gate;
        await route.fulfill({ json: { state: "ready", account } });
      });
      const requested = page.waitForRequest("**/api/sub2api/browser");
      await box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click();
      const pending = await requested;
      assert.deepEqual(posted(f, "cancel")[0].body, { attempt: "attempt-one" });
      assert.equal(await box.getByLabel("Six-digit verification code").count(), 0);
      const aborted = page.waitForEvent("requestfailed", { predicate: (r) => r === pending });
      await box.getByRole("button", { name: "Cancel", exact: true }).click();
      await aborted;
      release();
      await box.getByLabel("Panel password", { exact: true }).waitFor();
      assert.equal(posted(f, "2fa").length, 0);
      assert.equal(posted(f, "cancel").length, 1, "the new request is cancelled directly before it returns an attempt");
      assert.deepEqual(errors, []);
    });
    for (const method of ["password", "2fa"]) {
      await t.test(`${method} signs an account in without a linked provider`, async (st) => {
        const f = fixture(true);
        f.providers = [];
        f.accounts[0].state = "login_required";
        f.loginReady = method === "password";
        const { page, context, errors } = await open(browser, f, "en", 420, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        await pane.getByRole("button", { name: "Sign in again", exact: true }).click();
        const form = pane.locator(".s2-editor-body");
        await form.getByLabel("Panel password", { exact: true }).fill("account-secret");
        await waitPost(page, "login-account", () => form.getByRole("button", { name: "Sign in", exact: true }).click());
        assert.equal(posted(f, "login-account")[0].body.expectedGeneration, account.generation);
        assert.equal(posted(f, "login-account")[0].body.base, undefined, "the server restores the private network context");
        if (method === "2fa") {
          await form.getByLabel("Six-digit verification code").fill("123456");
          await waitPost(page, "2fa-account", () => form.getByRole("button", { name: "Verify code", exact: true }).click());
          assert.equal(posted(f, "2fa-account")[0].body.expectedGeneration, account.generation);
        }
        await pane.locator(".s2-account-status .s2-state.ready").waitFor();
        assert.equal(posted(f, "bind").length, 0);
        assert.equal(posted(f, "login").length, 0);
        assert.equal(await pane.locator("input[type=password]").count(), 0);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("leaving Usage cancels the account's pending verification", async (st) => {
      const f = fixture(true);
      f.accounts[0].state = "login_required";
      const { page, context, errors } = await open(browser, f, "en", 420, "usage");
      st.after(() => context.close());
      const pane = page.locator("#sub2apiPane");
      await pane.getByRole("button", { name: "Sign in again", exact: true }).click();
      const form = pane.locator(".s2-editor-body");
      await form.getByLabel("Panel password", { exact: true }).fill("account-secret");
      await waitPost(page, "login-account", () => form.getByRole("button", { name: "Sign in", exact: true }).click());
      await form.getByLabel("Six-digit verification code").waitFor();
      await waitPost(page, "cancel", () => page.locator('#nav [data-view="providers"]').click());
      assert.deepEqual(posted(f, "cancel")[0].body, { attempt: "account-attempt" });
      assert.equal(posted(f, "2fa-account").length, 0);
      assert.deepEqual(errors, []);
    });
    await t.test("a linked provider signs in at the account base beside a custom balance URL", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 420);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.getByRole("button", { name: "Sign in again", exact: true }).click();
      assert.equal(await box.getByLabel("Panel address", { exact: true }).inputValue(), account.base);
      await box.getByLabel("Panel password", { exact: true }).fill("account-secret");
      await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
      assert.equal(posted(f, "login")[0].body.base, account.base);
      assert.equal(f.providers[0].balanceURL, relay.balanceURL);
      assert.deepEqual(errors, []);
    });
    await t.test("one-time billing works with monitoring off and stays local until explicit rate application", async (st) => {
      const f = fixture(true);
      f.configs[account.id].enabled = false;
      const settings = structuredClone(f.configs), snapshot = structuredClone(f.monitor);
      const { page, context, errors } = await open(browser, f, "en", 420, "usage");
      st.after(() => context.close());
      const pane = page.locator("#sub2apiPane");
      const row = pane.locator('[data-group="a"] .s2-local .s2-source');
      const query = pane.locator('[data-group="a"] .s2-keys').getByRole("button", { name: "Query once", exact: true });
      await wheelTo(page, query);
      await waitPost(page, "preview", () => query.click());
      assert.deepEqual(posted(f, "preview")[0].body, { session: account.id, expectedGeneration: account.generation,
        settingsRevision: "settings-1", provider: "relay", expectedRevision: "revision-1", bindingVersion: binding.version });
      await pane.getByText("One-time query", { exact: true }).waitFor();
      assert.deepEqual(f.configs, settings);
      assert.deepEqual(f.monitor, snapshot);
      assert.equal(posted(f, "monitor").length, 0);
      const apply = row.getByRole("button", { name: "Apply rate", exact: true }).first();
      assert(await apply.isEnabled());
      await wheelTo(page, apply);
      await apply.click();
      const confirm = page.getByRole("alertdialog");
      await waitPost(page, "apply-key-rate", () => confirm.getByRole("button", { name: "Apply rate", exact: true }).first().click());
      assert.equal(localRate(f.providers[0]), .8);
      assert.equal(f.providers[0].priceRate, relay.priceRate, "a Key rate preserves the source fallback");
      assert.equal(localRate(f.providers[0], "key-two"), relay.priceRate, "the other Key is unchanged");
      assert.equal(f.configs[account.id].enabled, false);
      assert.deepEqual(errors, []);
    });
    await t.test("an unconfigured account can query groups then source billing without a prior snapshot", async (st) => {
      const f = fixture(true);
      f.previewAccount = structuredClone(f.monitor.accounts[0]);
      f.monitor.accounts = [];
      f.configs[account.id] = { configured: false, enabled: false, groups: [], revision: "", generation: account.generation };
      f.previewAccount.settingsRevision = "";
      const { page, context, errors } = await open(browser, f, "en", 420, "usage");
      st.after(() => context.close());
      const pane = page.locator("#sub2apiPane"), row = pane.locator('[data-group="a"] .s2-local .s2-source');
      await waitPost(page, "preview", () => pane.locator(".s2-account-head").getByRole("button", { name: "Query once", exact: true }).click());
      assert.equal(posted(f, "preview")[0].body.provider, undefined);
      assert.equal(posted(f, "preview")[0].body.settingsRevision, "");
      const query = pane.locator('[data-group="a"] .s2-keys').getByRole("button", { name: "Query once", exact: true });
      await wheelTo(page, query);
      await waitPost(page, "preview", () => query.click());
      assert.equal(posted(f, "preview").at(-1).body.provider, "relay");
      await page.waitForFunction(() => [...document.querySelectorAll('[data-group="a"] .s2-local .s2-source button')]
        .some((b) => b.getAttribute("aria-label") === "Apply rate" && !b.disabled));
      assert(await row.getByRole("button", { name: "Apply rate", exact: true }).first().isEnabled());
      assert.equal(f.monitor.accounts.length, 0);
      assert.equal(f.configs[account.id].configured, false);
      assert.equal(posted(f, "monitor").length, 0);
      assert.deepEqual(errors, []);
    });
    for (const method of ["password", "2fa", "browser"]) {
      await t.test(`${method} keeps a saved account when association fails`, async (st) => {
        const f = fixture();
        f.loginReady = method === "password";
        f.associationFailure = true;
        const { page, context, errors } = await open(browser, f, "en", 420);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        if (method === "browser") {
          await waitPost(page, "browser", () => box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click());
        } else {
          await box.getByLabel("Panel email", { exact: true }).fill(account.email);
          await box.getByLabel("Panel password", { exact: true }).fill("single-use-secret");
          await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
        }
        if (method === "2fa") {
          await box.getByLabel("Six-digit verification code").fill("123456");
          await waitPost(page, "2fa", () => box.getByRole("button", { name: "Verify code", exact: true }).click());
        }
        await box.locator(".s2-message").filter({ hasText: "Signed in; account linking failed:" }).waitFor();
        assert.equal(f.accounts[0].id, account.id);
        assert.equal(f.providers[0].sub2api, undefined);
        assert.equal(f.providers[0].balanceToken.set, true);
        assert.equal(posted(f, "bind").length, 0, "the client never repeats the server's association");
        const use = box.getByRole("button", { name: "Link account", exact: true });
        assert(await use.isEnabled(), "the saved account is selected for explicit recovery");
        await waitPost(page, "bind", () => use.click());
        assert.equal(posted(f, "bind")[0].body.expectedGeneration, account.generation);
        assert.equal(posted(f, method === "browser" ? "browser" : "login").length, 1, "association recovery does not repeat sign-in");
        assert.deepEqual(errors, []);
      });
    }
    await t.test("panels load on demand and closing cancels a late sign-in attempt", async (st) => {
      const f = fixture(), { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      const loaded = [];
      page.on("request", (request) => loaded.push(new URL(request.url()).pathname));
      const row = page.locator(".row.provider", { hasText: "Relay" });
      await row.waitFor();
      assert.equal(await page.evaluate(() => performance.getEntriesByType("resource")
        .some((entry) => /\/sub2api\/(?:editor|usage|common)\.js/.test(entry.name))), false);
      let releaseModule;
      const moduleGate = new Promise((resolve) => releaseModule = resolve);
      await page.route("**/sub2api/editor.js", async (route) => { await moduleGate; await route.fallback(); });
      const moduleRequested = page.waitForRequest("**/sub2api/editor.js");
      await row.click();
      await moduleRequested;
      const closedEditor = await page.locator(".editor").elementHandle();
      await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
      await page.locator(".editor").waitFor({ state: "hidden" });
      const moduleLoaded = page.waitForResponse("**/sub2api/common.js");
      releaseModule();
      await moduleLoaded;
      assert.equal(await page.locator(".s2-editor").count(), 0, "a closed editor is not mounted after the import");
      await row.click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      assert.equal(await closedEditor.evaluate((node) => node.querySelector(".bal-fix")), null, "late balance fields stay out of a cancelled form");
      await closedEditor.dispose();
      await box.getByLabel("Panel email", { exact: true }).fill(account.email);
      await box.getByLabel("Panel password", { exact: true }).fill("late-secret");
      assert.equal(loaded.includes("/sub2api/usage.js"), false, "editing does not load Usage");
      let releaseLogin;
      const loginGate = new Promise((resolve) => releaseLogin = resolve);
      await page.route("**/api/sub2api/login", async (route) => { await loginGate; await route.fallback(); });
      const loginRequested = page.waitForRequest("**/api/sub2api/login");
      await box.getByRole("button", { name: "Sign in", exact: true }).click();
      await loginRequested;
      await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
      await page.locator(".editor").waitFor({ state: "hidden" });
      const pending = await loginRequested;
      releaseLogin();
      assert(pending.failure(), "closing cancels the password request before a late response can create an attempt");
      assert.equal(posted(f, "cancel").length, 0);
      assert.equal(await page.locator(".s2-editor").count(), 0);
      assert.equal(posted(f, "2fa").length, 0);
      assert.deepEqual(errors, []);
    });
    for (const lang of ["en", "zh", "zh-TW", "ja", "de"]) {
      for (const width of [1100, 420]) {
        await t.test(`${lang} ${width}px: password, 2FA, cancel, binding and old balance`, async (st) => {
          const f = fixture(), { page, context, errors } = await open(browser, f, lang, width);
          st.after(() => context.close());
          const tr = (key) => word(lang, key);
          await page.locator(".row.provider", { hasText: "Relay" }).click();
          const box = page.locator(".s2-editor");
          await box.locator("summary").click();
          await box.getByRole("button", { name: tr("Sign in"), exact: true }).waitFor();
          assert(await box.getByRole("button", { name: tr("Sign in with browser verification"), exact: true }).isEnabled());
          assert.equal(await box.getByLabel(tr("Panel address"), { exact: true }).inputValue(), account.base);
          const password = box.getByLabel(tr("Panel password"), { exact: true });
          await box.getByLabel(tr("Panel email"), { exact: true }).fill(account.email);
          await password.fill("one-use-secret");
          assert.equal(await box.getByRole("checkbox", { name: tr("Remember this password on this computer") }).isChecked(), false);
          assert.equal(f.posted.length, 0, "typing writes nothing");
          await waitPost(page, "login", () => box.getByRole("button", { name: tr("Sign in"), exact: true }).click());
          const code = box.getByLabel(tr("Six-digit verification code"));
          await code.waitFor();
          assert.equal(posted(f, "login")[0].body.password, "one-use-secret");
          assert.equal(posted(f, "login")[0].body.remember, false);
          assert.equal(posted(f, "login")[0].body.expectedRevision, "revision-1");
          assert.equal(await box.locator("input[type=password]").count(), 0, "the password is cleared before 2FA");
          await waitPost(page, "cancel", () => box.getByRole("button", { name: tr("Cancel"), exact: true }).click());
          assert.equal(posted(f, "cancel")[0].body.attempt, "attempt-one");
          await box.getByLabel(tr("Panel password"), { exact: true }).fill("second-secret");
          await waitPost(page, "login", () => box.getByRole("button", { name: tr("Sign in"), exact: true }).click());
          await code.fill("123456");
          await waitPost(page, "2fa", () => box.getByRole("button", { name: tr("Verify code"), exact: true }).click());
          await box.getByRole("button", { name: tr("Unlink provider"), exact: true }).waitFor();
          assert.deepEqual(posted(f, "2fa")[0].body, { provider: "relay", attempt: "attempt-one", code: "123456" });
          assert.equal(posted(f, "provider/save").length, 0, "sign-in does not save an old provider draft");
          assert.equal(await page.locator("body").textContent().then((s) => s.includes("second-secret")), false);
          const pick = box.getByRole("button", { name: tr("Panel group"), exact: true }).first();
          await wheelTo(page, pick);
          await pick.click();
          await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Standard/ }).click());
          assert.equal(posted(f, "bind-key").at(-1).body.expectedVersion, binding.version);
          assert.equal(posted(f, "bind-key").at(-1).body.expectedRevision, "revision-2");
          assert.equal(posted(f, "bind-key").at(-1).body.expectedGeneration, account.generation);
          assert.equal(posted(f, "bind-key").at(-1).body.groupId, "b");
          await fits(page, box);
          await page.locator(".editor details.more").filter({ has: page.locator(".bal-url") }).locator(":scope > summary").click();
          assert.equal(await page.locator(".bal-url").inputValue(), relay.balanceURL);
          assert.equal(await page.locator(".bal-path").inputValue(), relay.balancePath);
          assert.equal(await page.locator(".bal-method .opt.on").textContent(), "POST");
          const checked = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/provider/balance");
          await page.locator(".editor").getByRole("button", { name: tr("Check balance"), exact: true }).click();
          await checked;
          const body = posted(f, "provider/balance").at(-1).body;
          assert.equal(body.balanceMethod, "POST");
          assert.equal(body.balancePath, "$credits.left");
          assert.equal(body.balanceToken, undefined, "a session query never copies a panel token into the form");
          await waitPost(page, "unbind", () => box.getByRole("button", { name: tr("Unlink provider"), exact: true }).click());
          assert.equal(posted(f, "unbind").at(-1).body.expectedVersion, nextVersion(binding.version));
          assert.equal(posted(f, "unbind").at(-1).body.expectedRevision, "revision-3");
          assert.equal(posted(f, "logout").length, 0, "unlinking does not sign out a shared account");
          await box.getByLabel(tr("Panel password"), { exact: true }).fill("cancel-secret");
          await waitPost(page, "login", () => box.getByRole("button", { name: tr("Sign in"), exact: true }).click());
          await code.waitFor();
          await waitPost(page, "cancel", () => page.locator(".editor .bar").getByRole("button", { name: tr("Cancel"), exact: true }).click());
          await page.locator(".editor").waitFor({ state: "hidden" });
          assert.deepEqual(errors, []);
        });
        await t.test(`${lang} ${width}px: groups, rates, account monitoring, refresh and history`, async (st) => {
          const f = fixture(true), p = f.providers[0], mon = f.monitor.accounts[0];
          const other = (id, groupId, rates) => {
            const keyList = rates.map((_, i) => ({ id: "other-" + i, name: "Key " + i, on: true }));
            const provider = { ...structuredClone(relay), id, name: id, keyList, sub2api: { ...binding,
              groupId, keyIds: keyList.map((k) => k.id), version: "binding-4" } };
            f.providers.push(provider);
            mon.sources.push({ id, name: id, groupId, groupName: groupId, bindingVersion: "binding-4", revision: provider.sub2apiRevision, activeKeyIds: provider.keyList.map((k) => k.id), conflict: new Set(rates).size > 1,
              ...(new Set(rates).size === 1 ? { rate: rates[0] } : {}), keys: rates.map((rate, i) => ({ id: "other-" + i, rate, lastSuccess: stamp })) });
          };
          other("Conflicting", "b", [.7, .9]); other("Zero", "c", [0]); other("Unassigned", "", [1.1]);
          const { page, context, errors } = await open(browser, f, lang, width, "usage");
          st.after(() => context.close());
          const tr = (key) => word(lang, key), pane = page.locator("#sub2apiPane");
          await pane.locator('.s2-group[data-group="a"]').waitFor();
          const a = pane.locator('.s2-group[data-group="a"]');
          const b = pane.locator('.s2-group[data-group="b"]');
          const c = pane.locator('.s2-group[data-group="c"]');
          assert.equal(await a.locator(".s2-billing .s2-label").first().textContent(), tr("Key rate"));
          assert.match(await b.textContent(), new RegExp(tr("Rate not confirmed")));
          assert.doesNotMatch(await b.textContent(), new RegExp(tr("Key rate conflict")), "independent Key rates are not a conflict");
          assert.match(await c.textContent(), /0×/);
          assert(await c.locator(".s2-apply").first().isDisabled());
          assert(await b.getByRole("button", { name: tr("Apply rate"), exact: true }).first().isEnabled());
          await pane.locator('.s2-group[data-group=""]').waitFor();
          assert(await pane.locator('.s2-group[data-group=""] .s2-apply').first().isDisabled());
          await wheelTo(page, a.locator(".s2-observation > summary"));
          await a.locator(".s2-observation > summary").click();
          for (const summary of await a.locator(".s2-metric-history > summary").all()) {
            await wheelTo(page, summary);
            await summary.click();
          }
          assert.match(await a.locator(".s2-observation").textContent(), /98%/);
          assert.match(await a.locator(".s2-observation").textContent(), /75%/);
          assert.match(await a.textContent(), /Latest observation unavailable/);
          assert.equal(await b.locator(".s2-observation").count(), 0, "the global snapshot is not copied to an unknown group");
          assert.equal(await pane.locator(".s2-account-details > .s2-observation").count(), 1);
          assert.equal(await pane.locator(".s2-group").getByRole("checkbox").count(), 0,
            "all groups follow the account switch without individual watch controls");
          const watchAccount = pane.getByRole("switch", { name: tr("Monitor this account") });
          await wheelTo(page, watchAccount);
          await waitPost(page, "monitor", () => watchAccount.uncheck());
          assert.equal(posted(f, "monitor").at(-1).body.enabled, false);
          assert.deepEqual(posted(f, "monitor").at(-1).body.groups, [{ id: "a", name: "Premium" }],
            "the account switch preserves legacy group references without editing them");
          const apply = a.getByRole("button", { name: tr("Apply rate"), exact: true }).first();
          assert(await apply.isEnabled(), "an observation failure does not discard successful key billing");
          await wheelTo(page, apply);
          await apply.click();
          const confirm = page.getByRole("alertdialog");
          await confirm.waitFor();
          assert.match(await confirm.textContent(), new RegExp(tr("Historical estimates may be recalculated at this rate. Prices set for individual models stay as set; recorded costs stay unchanged.").replace(/[.*+?^${}()|[\]\\]/g, "\\$&")));
          await confirm.getByRole("button", { name: tr("Cancel"), exact: true }).click();
          assert.equal(posted(f, "apply-key-rate").length, 0, "cancel does not apply a rate");
          await apply.click();
          await waitPost(page, "apply-key-rate", () => confirm.getByRole("button", { name: tr("Apply rate"), exact: true }).first().click());
          assert.deepEqual(posted(f, "apply-key-rate")[0].body, { provider: "relay", keyId: "key-one", rate: .8, expectedRate: 1.2, bindingVersion: binding.version, expectedRevision: "revision-1", expectedGeneration: account.generation });
          assert.equal(localRate(p), .8);
          assert.equal(p.priceRate, relay.priceRate);
          await page.waitForFunction(() => document.querySelector('[data-group="a"] .s2-local button')?.disabled);
          const onlyChanged = pane.getByRole("checkbox", { name: tr("Only changed groups") });
          await wheelTo(page, onlyChanged);
          await onlyChanged.check();
          assert.equal(await pane.locator(".s2-group").count(), 1);
          const history = pane.locator(".s2-history > summary");
          await wheelTo(page, history);
          await history.click();
          assert.match(await pane.locator(".s2-history").textContent(), new RegExp(tr("Key rate changed")));
          const reload = page.locator("#usageReload");
          await wheelTo(page, reload);
          await waitPost(page, "refresh", () => reload.click());
          assert.equal(posted(f, "monitor").length, 1, "refreshing the view does not change background monitoring");
          assert.equal(await page.locator("select").count(), 0);
          await fits(page, pane);
          if (process.env.ARTIFACT_DIR) {
            await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
            await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-${engine}-${lang}-${width}.png`), fullPage: true });
          }
          assert.deepEqual(errors, []);
        });
      }
    }
    await t.test("conflicting editor actions reread versions without replaying credentials or changes", async (st) => {
      const f = fixture(), { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      await box.getByLabel("Panel email", { exact: true }).fill(account.email);
      await box.getByLabel("Panel password", { exact: true }).fill("first-secret");
      f.failNext = "login";
      let reads = f.providerReads;
      await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
      await box.locator(".s2-message.bad").waitFor();
      assert(f.providerReads > reads);
      assert.equal(posted(f, "login").length, 1, "a conflict never replays the password");
      assert.equal(await box.getByLabel("Panel password", { exact: true }).inputValue(), "");
      assert.equal(await box.getByLabel("Panel email", { exact: true }).inputValue(), account.email);
      await box.getByLabel("Panel password", { exact: true }).fill("second-secret");
      await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
      assert.equal(posted(f, "login").at(-1).body.expectedRevision, "revision-2");
      await box.getByLabel("Six-digit verification code").fill("123456");
      await waitPost(page, "2fa", () => box.getByRole("button", { name: "Verify code", exact: true }).click());
      const pick = box.getByRole("button", { name: "Panel group", exact: true }).first();
      await pick.waitFor();
      f.failNext = "bind-key";
      reads = f.providerReads;
      await wheelTo(page, pick);
      await pick.click();
      await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Standard/ }).click());
      await box.locator(".s2-message.bad").waitFor();
      assert(f.providerReads > reads);
      assert.equal(posted(f, "bind-key").length, 1);
      await wheelTo(page, pick);
      await pick.click();
      await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Standard/ }).click());
      assert.equal(posted(f, "bind-key").at(-1).body.expectedRevision, "revision-4");
      assert.equal(posted(f, "bind-key").at(-1).body.expectedVersion, nextVersion(binding.version));
      await pick.filter({ hasText: "Standard" }).waitFor();
      const secondKey = box.locator('[data-key="key-two"]').getByRole("button", { name: "Panel group", exact: true });
      await wheelTo(page, secondKey);
      await secondKey.click();
      await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Premium/ }).click());
      assert.equal(posted(f, "bind-key").at(-1).body.expectedRevision, "revision-5");
      assert.equal(posted(f, "bind-key").at(-1).body.expectedVersion, nextVersion(nextVersion(binding.version)));
      assert.equal(posted(f, "bind-key").at(-1).body.keyId, "key-two");
      await page.waitForFunction(() => providers.providers[0].sub2api?.version === "binding-7-next-next-next");
      f.failNext = "unbind";
      const unlink = box.getByRole("button", { name: "Unlink provider", exact: true });
      reads = f.providerReads;
      await wheelTo(page, unlink);
      await waitPost(page, "unbind", () => unlink.click());
      await box.locator(".s2-message.bad").waitFor();
      assert(f.providerReads > reads);
      assert.equal(posted(f, "unbind").length, 1);
      assert.equal(posted(f, "unbind")[0].body.expectedVersion, nextVersion(nextVersion(nextVersion(binding.version))));
      await wheelTo(page, unlink);
      await waitPost(page, "unbind", () => unlink.click());
      assert.equal(posted(f, "unbind").at(-1).body.expectedRevision, "revision-7");
      assert.equal(posted(f, "unbind").at(-1).body.expectedVersion, nextVersion(nextVersion(nextVersion(nextVersion(binding.version)))));
      await unlink.waitFor({ state: "hidden" });
      const signedIn = box.getByRole("button", { name: "Signed-in accounts", exact: true });
      await wheelTo(page, signedIn);
      await signedIn.click();
      await page.getByRole("menuitemradio", { name: new RegExp(account.email) }).click();
      await waitPost(page, "bind", () => box.getByRole("button", { name: "Link account", exact: true }).click());
      assert.equal(posted(f, "bind").at(-1).body.expectedRevision, "revision-8");
      assert.equal(posted(f, "bind").at(-1).body.expectedVersion, "");
      assert.equal(posted(f, "provider/save").length, 0);
      assert.deepEqual(errors, []);
    });
    await t.test("rate and group conflicts reread before the next explicit action", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      const pane = page.locator("#sub2apiPane"), a = pane.locator('[data-group="a"]');
      const apply = a.getByRole("button", { name: "Apply rate", exact: true }).first();
      await apply.waitFor();
      let reads = f.providerReads, snapshots = f.panelReads;
      f.failNext = "apply-key-rate";
      await apply.click();
      const confirm = page.getByRole("alertdialog");
      await waitPost(page, "apply-key-rate", () => confirm.getByRole("button", { name: "Apply rate", exact: true }).first().click());
      await page.locator("#status").filter({ hasText: "The provider changed" }).waitFor();
      assert(f.providerReads > reads);
      assert(f.panelReads > snapshots);
      assert.equal(posted(f, "apply-key-rate").length, 1, "rereading never reapplies the rate");
      assert.equal(f.providers[0].priceRate, 1.4, "the concurrent price survives");
      await apply.click();
      await waitPost(page, "apply-key-rate", () => confirm.getByRole("button", { name: "Apply rate", exact: true }).first().click());
      assert.deepEqual(posted(f, "apply-key-rate").at(-1).body, {
        provider: "relay", keyId: "key-one", rate: .8, expectedRate: 1.4, bindingVersion: nextVersion(binding.version), expectedRevision: "revision-2", expectedGeneration: account.generation,
      });
      await page.waitForFunction(() => providers.providers[0].sub2api.keys?.[0]?.priceRate === .8);
      const link = pane.locator('[data-group="b"]').getByRole("button", { name: "Link key", exact: true });
      f.failNext = "bind-key";
      reads = f.providerReads;
      await link.click();
      await waitPost(page, "bind-key", () => page.getByRole("menuitemcheckbox", { name: /Relay · First/ }).click());
      await page.waitForFunction(() => providers.providers[0].sub2api?.version === "binding-7-next-next");
      assert(f.providerReads > reads);
      assert.equal(posted(f, "bind-key").length, 1);
      await link.click();
      await waitPost(page, "bind-key", () => page.getByRole("menuitemcheckbox", { name: /Relay · First/ }).click());
      assert.equal(posted(f, "bind-key").at(-1).body.expectedRevision, "revision-4");
      assert.equal(posted(f, "bind-key").at(-1).body.expectedVersion, nextVersion(nextVersion(binding.version)));
      assert.deepEqual(errors, []);
    });
    for (const method of ["password", "2fa", "browser"]) {
      await t.test(`${method} binding refreshes a clean editor before its next provider save`, async (st) => {
        const f = fixture();
        f.loginReady = method === "password";
        f.providers[0].balanceURL = f.providers[0].balancePath = f.providers[0].balanceMethod = "";
        const { page, context, errors } = await open(browser, f, "en", 1100);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        const panel = await box.elementHandle();
        st.after(() => panel.dispose());
        if (method === "browser") {
          await waitPost(page, "browser", () => box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click());
        } else {
          await box.getByLabel("Panel email", { exact: true }).fill(account.email);
          await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
          await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
          if (method === "2fa") {
            await box.getByLabel("Six-digit verification code").fill("123456");
            await waitPost(page, "2fa", () => box.getByRole("button", { name: "Verify code", exact: true }).click());
          }
        }
        await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
        assert(await panel.evaluate((node) => node.isConnected), "the panel survives the provider form refresh");
        assert.deepEqual(await page.evaluate(() => ({ revision: draft.saveRevision, dirty: providerDirty() })),
          { revision: "save-2", dirty: false });
        const pick = box.getByRole("button", { name: "Panel group", exact: true }).first();
        await wheelTo(page, pick);
        const scrollTop = await pick.evaluate((node) => node.closest(".ebody").scrollTop);
        await pick.click();
        await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Standard/ }).click());
        await pick.filter({ hasText: "Standard" }).waitFor();
        await page.locator(".editor .mchips-fold").waitFor({ state: "hidden" });
        assert.equal(await pick.evaluate((node) => node.closest(".ebody").scrollTop), scrollTop,
          "binding keeps the editor's scroll position through the delayed mount and model fold measurement");
        assert.equal(await page.evaluate(() => draft.saveRevision), "save-3");
        await page.locator(".editor details.more").filter({ has: page.locator(".bal-url") }).locator(":scope > summary").click();
        assert.equal(await page.locator(".bal-url").inputValue(), account.base + "/api/v1/user/profile");
        assert.equal(await page.locator(".bal-path").inputValue(), "$data.balance");
        assert.equal(await page.getByPlaceholder("saved · paste a new one to replace it", { exact: true }).count(), 0,
          "the old account token is no longer offered for removal");
        const response = page.waitForResponse("**/api/provider/save");
        await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
        assert.equal((await response).status(), 200);
        await page.locator(".editor").waitFor({ state: "hidden" });
        const body = posted(f, "provider/save").at(-1).body;
        assert.equal(body.saveRevision, "save-3");
        assert.equal(body.balanceURL, account.base + "/api/v1/user/profile");
        assert.equal(body.balancePath, "$data.balance");
        assert.equal(body.clearBalanceToken, undefined);
        for (const key of ["sub2api", "password", "refreshToken", "accessToken"]) assert.equal(body[key], undefined);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("a provider refresh preserves pending verification and its typed code", async (st) => {
      const f = fixture(), { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      await box.getByLabel("Panel email", { exact: true }).fill(account.email);
      await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
      await page.evaluate(async () => { await (await import("/sub2api/common.js")).reloadProviders(); });
      assert.equal(await box.getByLabel("Panel password", { exact: true }).inputValue(), "one-use-secret");
      assert.equal(posted(f, "login").length, 0, "a provider refresh never submits the password");
      await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
      const code = box.getByLabel("Six-digit verification code");
      await code.fill("123456");
      f.providers[0].saveRevision = "save-refresh";
      await page.evaluate(async () => { await (await import("/sub2api/common.js")).reloadProviders(); });
      assert.equal(await code.inputValue(), "123456");
      assert.equal(posted(f, "cancel").length, 0);
      await waitPost(page, "2fa", () => box.getByRole("button", { name: "Verify code", exact: true }).click());
      await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
      assert.equal(posted(f, "2fa").length, 1);
      assert.equal(posted(f, "2fa")[0].body.attempt, "attempt-one");
      assert.deepEqual(errors, []);
    });
    await t.test("a late editor remount still shows the completed binding once", async (st) => {
      const f = fixture();
      f.loginReady = true;
      const { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.locator("summary").click();
      await box.getByLabel("Panel email", { exact: true }).fill(account.email);
      await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
      await page.evaluate(() => {
        const mount = window.Sub2API.mountEditor;
        const gate = new Promise((resolve) => window.releaseEditorMount = resolve);
        window.Sub2API.mountEditor = async (...args) => { await gate; return mount(...args); };
      });
      await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
      await page.locator("#status").filter({ hasText: "Signed in" }).waitFor();
      assert.equal(await box.count(), 0, "the provider form has refreshed before its delayed panel mount");
      await page.evaluate(() => window.releaseEditorMount());
      await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
      assert.equal(await box.getByRole("button", { name: "Sign in", exact: true }).count(), 0);
      assert.equal(posted(f, "login").length, 1);
      assert.deepEqual(errors, []);
    });
    for (const outcome of ["ready", "failure"]) {
      await t.test(`a pending sign-in survives a delayed remount with ${outcome}`, async (st) => {
        const f = fixture();
        f.loginReady = true;
        const { page, context, errors } = await open(browser, f, "en", 1100);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        const panel = await box.elementHandle();
        st.after(() => panel.dispose());
        await box.getByLabel("Panel email", { exact: true }).fill(account.email);
        await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
        let releaseLogin;
        const loginGate = new Promise((resolve) => releaseLogin = resolve);
        st.after(releaseLogin);
        await page.evaluate(() => {
          const mount = window.Sub2API.mountEditor;
          const gate = new Promise((resolve) => window.releaseEditorMount = resolve);
          window.Sub2API.mountEditor = async (...args) => { await gate; return mount(...args); };
        });
        await page.route("**/api/sub2api/login", async (route) => {
          await loginGate;
          if (outcome === "failure") {
            f.posted.push({ action: "login", body: route.request().postDataJSON() });
            await route.fulfill({ status: 503, json: { error: "Panel temporarily unavailable" } });
          } else await route.fallback();
        });
        const requested = page.waitForRequest("**/api/sub2api/login");
        await box.getByRole("button", { name: "Sign in", exact: true }).click();
        await requested;
        f.providers[0].saveRevision = "save-refresh";
        await page.evaluate(async () => { await (await import("/sub2api/common.js")).reloadProviders(); });
        await box.waitFor({ state: "detached" });
        const response = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/sub2api/login");
        releaseLogin();
        assert.equal((await response).status(), outcome === "ready" ? 200 : 503);
        if (outcome === "ready") await page.locator("#status").filter({ hasText: "Signed in" }).waitFor();
        else await page.waitForFunction((node) => node.querySelector(".s2-message")?.textContent === "Panel temporarily unavailable" &&
          [...node.querySelectorAll("button")].some((b) => b.textContent === "Sign in" && !b.disabled), panel);
        await page.evaluate(() => window.releaseEditorMount());
        if (outcome === "ready") {
          await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
          assert.equal(await box.getByRole("button", { name: "Sign in", exact: true }).count(), 0);
        } else {
          await box.locator(".s2-message").filter({ hasText: "Panel temporarily unavailable" }).waitFor();
          assert(await box.getByRole("button", { name: "Sign in", exact: true }).isEnabled());
        }
        assert.equal(posted(f, "login").length, 1);
        assert.deepEqual(errors, []);
      });
    }
    for (const when of ["login", "providers"]) {
      await t.test(`edits during a delayed ${when} response keep their draft and old save revision`, async (st) => {
        const f = fixture();
        f.loginReady = true;
        const { page, context, errors } = await open(browser, f, "en", 1100);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        await box.getByLabel("Panel email", { exact: true }).fill(account.email);
        await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        const routePath = when === "login" ? "**/api/sub2api/login" : "**/api/providers";
        await page.route(routePath, async (route) => { await gate; await route.fallback(); });
        const requested = page.waitForRequest(routePath);
        await box.getByRole("button", { name: "Sign in", exact: true }).click();
        await requested;
        await page.getByPlaceholder("e.g. My Relay", { exact: true }).fill("Unsaved relay");
        release();
        await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
        assert.deepEqual(await page.evaluate(() => ({ revision: draft.saveRevision, name: draft.name, dirty: providerDirty() })),
          { revision: "save-1", name: "Unsaved relay", dirty: true });
        const response = page.waitForResponse("**/api/provider/save");
        await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
        assert.equal((await response).status(), 409, "a dirty draft cannot overwrite the newly bound provider");
        await page.waitForFunction(() => draft?.saveRevisionConflict === true);
        assert.equal(posted(f, "provider/save").at(-1).body.name, "Unsaved relay");
        assert.equal(posted(f, "provider/save").at(-1).body.saveRevision, "save-1");
        assert(await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).isDisabled());
        assert.deepEqual(errors, []);
      });
    }
    await t.test("ordinary provider saves carry the version originally opened", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const response = page.waitForResponse((r) => new URL(r.url()).pathname === "/api/provider/save");
      await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
      await response;
      const body = posted(f, "provider/save").at(-1).body;
      assert.equal(body.saveRevision, "save-1");
      for (const key of ["sub2api", "password", "refreshToken", "accessToken"]) assert.equal(body[key], undefined);
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": closing forms cancels pending authentication before a late result", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const cases = [
      ["save", "browser", false], ["save", "password", false], ["cancel", "password", false],
      ["delete", "browser", false], ["gone", "browser", false], ["leave", "password", false],
      ["cancel", "2fa", false], ["cancel", "password", true], ["leave", "2fa", true],
    ];
    for (const [close, method, recovery] of cases) {
      await t.test(`${close} ends ${method} ${recovery ? "account recovery" : "provider sign-in"}`, async (st) => {
        const f = fixture(recovery);
        f.loginReady = method === "password" && close !== "save";
        if (recovery) { f.providers = []; f.accounts[0].state = "login_required"; }
        const { page, context, errors } = await open(browser, f, "en", 1100, recovery ? "usage" : "providers");
        st.after(() => context.close());
        let box;
        if (recovery) {
          box = page.locator("#sub2apiPane .s2-editor-body");
          await page.locator("#sub2apiPane").getByRole("button", { name: "Sign in again", exact: true }).click();
        } else {
          await page.locator(".row.provider", { hasText: "Relay" }).click();
          box = page.locator(".s2-editor");
          await box.locator("summary").click();
        }
        if (method !== "browser") {
          if (!recovery) await box.getByLabel("Panel email", { exact: true }).fill(account.email);
          await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
          if (method === "2fa") {
            await waitPost(page, recovery ? "login-account" : "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
            await box.getByLabel("Six-digit verification code").fill("123456");
          }
        }
        const action = method === "browser" ? "browser" : method === "2fa" ? "2fa" : "login";
        const endpoint = action + (recovery ? "-account" : "");
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        await page.route(`**/api/sub2api/${endpoint}`, async (route) => {
          await gate;
          if (route.request().failure()) return route.abort().catch(() => {});
          await route.fallback();
        });
        if (close === "gone") await page.route("**/api/provider/delete", async (route) => {
          f.providers = [];
          await route.fulfill({ status: 400, json: { error: "The provider was removed but its agent could not be updated" } });
        });
        const requested = page.waitForRequest(`**/api/sub2api/${endpoint}`);
        await box.getByRole("button", { name: method === "browser" ? "Sign in with browser verification" : method === "2fa" ? "Verify code" : "Sign in", exact: true }).click();
        const pending = await requested;
        if (["delete", "gone"].includes(close)) {
          await page.locator(".editor .bar").getByRole("button", { name: "Remove", exact: true }).click();
          await page.getByRole("alertdialog").getByRole("button", { name: "Remove", exact: true }).click();
        } else if (close === "leave") {
          if (recovery) await page.locator('#nav [data-view="providers"]').click();
          else await page.evaluate(() => show("usage"));
        } else if (recovery) await box.getByRole("button", { name: "Cancel", exact: true }).click();
        else await page.locator(".editor .bar").getByRole("button", { name: close === "save" ? "Save" : "Cancel", exact: true }).click();
        if (recovery) await box.waitFor({ state: "hidden" });
        else await page.locator(".editor").waitFor({ state: "hidden" });
        assert(pending.failure(), "ending the form aborts the in-flight authentication request");
        release();
        assert.equal(posted(f, endpoint).length, 0, "the late fixture result never creates or updates a session");
        assert.equal(f.accounts.length, recovery ? 1 : 0);
        if (recovery) assert.equal(f.accounts[0].generation, account.generation);
        if (f.providers[0]) assert.equal(f.providers[0].sub2api, undefined);
        if (method === "2fa") assert.equal(posted(f, "cancel").at(-1).body.attempt, recovery ? "account-attempt" : "attempt-one");
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": authentication mode switches abort only the replaced request", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const method of ["password", "2fa"]) {
      await t.test(method + " can switch to browser verification", async (st) => {
        const f = fixture(), { page, context, errors } = await open(browser, f, "en", 1100);
        st.after(() => context.close());
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const box = page.locator(".s2-editor");
        await box.locator("summary").click();
        await box.getByLabel("Panel email", { exact: true }).fill(account.email);
        await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
        if (method === "2fa") {
          await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
          await box.getByLabel("Six-digit verification code").fill("123456");
        }
        const endpoint = method === "password" ? "login" : "2fa";
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        await page.route(`**/api/sub2api/${endpoint}`, async (route) => {
          await gate;
          if (route.request().failure()) return route.abort().catch(() => {});
          await route.fallback();
        });
        const requested = page.waitForRequest(`**/api/sub2api/${endpoint}`);
        await box.getByRole("button", { name: method === "password" ? "Sign in" : "Verify code", exact: true }).click();
        const pending = await requested;
        await waitPost(page, "browser", () => box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click());
        await box.getByRole("button", { name: "Unlink provider", exact: true }).waitFor();
        assert(pending.failure(), "the previous password or 2FA request is aborted before browser sign-in");
        release();
        assert.equal(posted(f, "browser").length, 1);
        assert.equal(posted(f, endpoint).length, 0);
        assert.equal(f.accounts.length, 1);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("a new sign-in remains cancellable when the old request finishes aborting", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100);
      st.after(() => context.close());
      await page.locator(".row.provider", { hasText: "Relay" }).click();
      const box = page.locator(".s2-editor");
      await box.getByRole("button", { name: "Sign in again", exact: true }).click();
      await page.evaluate(() => {
        const fetch = window.fetch;
        const gate = new Promise((resolve) => window.releaseOldLogin = resolve);
        let first = true;
        window.fetch = async (...args) => {
          const hold = first && args[0] === "/api/sub2api/login";
          if (hold) first = false;
          try { return await fetch(...args); }
          catch (error) {
            if (hold) await gate;
            throw error;
          }
        };
      });
      let release;
      const gate = new Promise((resolve) => release = resolve);
      st.after(release);
      await page.route("**/api/sub2api/login", async (route) => {
        await gate;
        if (route.request().failure()) return route.abort().catch(() => {});
        await route.fallback();
      });
      const begin = async () => {
        await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
        const requested = page.waitForRequest("**/api/sub2api/login");
        await box.getByRole("button", { name: "Sign in", exact: true }).click();
        return requested;
      };
      const submit = await box.getByRole("button", { name: "Sign in", exact: true }).elementHandle();
      st.after(() => submit.dispose());
      const first = await begin();
      await box.getByRole("button", { name: "Sign in again", exact: true }).click();
      assert(first.failure(), "starting again aborts the first request");
      const second = await begin();
      await page.evaluate(() => window.releaseOldLogin());
      await page.waitForFunction((button) => !button.disabled, submit);
      await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
      await page.locator(".editor").waitFor({ state: "hidden" });
      assert(second.failure(), "the first request's finally cannot remove the second request's cancellation");
      release();
      assert.equal(posted(f, "login").length, 0);
      assert.equal(f.accounts[0].generation, account.generation);
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": browser verification keeps ownership until the provider refresh finishes", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(), { page, context, errors } = await open(browser, f, "en", 1100);
    t.after(() => context.close());
    await page.locator(".row.provider", { hasText: "Relay" }).click();
    const box = page.locator(".s2-editor");
    await box.locator("summary").click();
    let release;
    const gate = new Promise((resolve) => release = resolve);
    t.after(release);
    await page.route("**/api/providers", async (route) => { await gate; await route.fallback(); });
    const refreshing = page.waitForRequest("**/api/providers");
    await waitPost(page, "browser", () => box.getByRole("button", { name: "Sign in with browser verification", exact: true }).click());
    await refreshing;
    assert.equal(await box.getByRole("button", { name: "Sign in", exact: true }).count(), 0,
      "another password login cannot start while the browser result is still refreshing the provider");
    await box.getByText("Complete sign-in in the verification window.", { exact: true }).waitFor();
    await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
    await page.locator(".editor").waitFor({ state: "hidden" });
    const refreshed = page.waitForResponse("**/api/providers");
    release();
    await refreshed;
    assert.equal(await page.evaluate(async () => (await import("/sub2api/common.js")).editors.size), 0);
    assert.doesNotMatch(await page.locator("#status").textContent(), /Signed in/);
    assert.deepEqual(errors, []);
  });
  test(engine + ": provider operations keep the editor that replaced their draft", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const cases = [
      ["save", "cancel", "second"], ["save", "cancel", "relay"], ["save", "api", "second"],
      ["conflict", "api", "relay"], ["gone", "cancel", "second"], ["kept", "api", "second"],
    ];
    for (const [outcome, wait, next] of cases) {
      await t.test(`${outcome} waits for ${wait} while ${next} is opened again`, async (st) => {
        const f = fixture();
        f.providers.push({ ...structuredClone(f.providers[0]), id: "second", name: "Second Relay" });
        const { page, context, errors } = await open(browser, f, "en", 1100);
        st.after(() => context.close());
        await page.locator('.row.provider[data-id="relay"]').click();
        const box = page.locator(".s2-editor");
        if (wait === "cancel") {
          await box.locator("summary").click();
          await box.getByLabel("Panel email", { exact: true }).fill(account.email);
          await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
          await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
          await box.getByLabel("Six-digit verification code").waitFor();
        }
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        const action = ["gone", "kept"].includes(outcome) ? "delete" : "save";
        const error = "The old provider operation failed.";
        if (outcome !== "save") await page.route(`**/api/provider/${action}`, async (route) => {
          if (wait === "api") await gate;
          if (outcome === "gone") f.providers = f.providers.filter((p) => p.id !== "relay");
          await route.fulfill({ status: outcome === "conflict" ? 409 : 400, json: { error } });
        });
        const endpoint = wait === "cancel" ? "**/api/sub2api/cancel" : `**/api/provider/${action}`;
        if (wait === "cancel" || outcome === "save") {
          await page.route(endpoint, async (route) => { await gate; await route.fallback(); });
        }
        const pending = page.waitForRequest(endpoint);
        await page.locator(".editor .bar").getByRole("button", { name: action === "save" ? "Save" : "Remove", exact: true }).click();
        if (action === "delete") await page.getByRole("alertdialog").getByRole("button", { name: "Remove", exact: true }).click();
        await pending;
        await page.locator(".editor .bar").getByRole("button", { name: "Cancel", exact: true }).click();
        await page.locator(".editor").waitFor({ state: "hidden" });
        await page.locator(`.row.provider[data-id="${next}"]`).click();
        await page.getByPlaceholder("e.g. My Relay", { exact: true }).fill("New draft must survive");
        const before = await page.evaluate(() => ({ editing, name: draft?.name, dirty: providerDirty(),
          conflict: !!draft?.saveRevisionConflict }));
        assert.deepEqual(before, { editing: next, name: "New draft must survive", dirty: true, conflict: false });
        release();
        await page.getByText(outcome === "save" ? "Relay saved" : error, { exact: true }).waitFor();
        assert.deepEqual(await page.evaluate(() => ({ editing, name: draft?.name, dirty: providerDirty(),
          conflict: !!draft?.saveRevisionConflict })), before, "an old operation cannot change a later draft, including one for the same provider");
        assert.equal(await page.locator(".editor .editor-error").count(), 0, "old errors stay out of the new editor");
        assert(await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).isEnabled());
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": account recovery callbacks keep the form that replaced them", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const outcome of ["ready", "conflict", "cancel"]) {
      await t.test(outcome + " cannot dismiss a later recovery form", async (st) => {
        const f = fixture(true);
        f.providers = [];
        f.accounts[0].state = "login_required";
        f.loginReady = outcome !== "cancel";
        const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        await pane.getByRole("button", { name: "Sign in again", exact: true }).click();
        if (outcome === "conflict") await page.route("**/api/sub2api/login-account", (route) =>
          route.fulfill({ status: 409, json: { error: "The old account changed." } }));
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        let first = true;
        const endpoint = outcome === "cancel" ? "**/api/sub2api/cancel" : "**/api/providers";
        await page.route(endpoint, async (route) => {
          if (first) { first = false; await gate; }
          await route.fallback();
        });
        const pending = page.waitForRequest(endpoint);
        await pane.getByLabel("Panel password", { exact: true }).fill("first-one-use-secret");
        await waitPost(page, "login-account", () => pane.getByRole("button", { name: "Sign in", exact: true }).click());
        if (outcome === "cancel") {
          await pane.getByLabel("Six-digit verification code").waitFor();
          await pane.getByRole("button", { name: "Cancel", exact: true }).click();
        }
        await pending;
        await page.locator('#nav [data-view="providers"]').click();
        await page.locator("#view-providers").waitFor({ state: "visible" });
        await page.locator('#nav [data-view="usage"]').click();
        if (outcome === "ready") {
          await page.waitForFunction(async () => (await import("/sub2api/common.js")).accountOf("session-one")?.state === "ready");
          await pane.locator(".s2-account-status .s2-state.ready").waitFor();
          await pane.getByRole("button", { name: "Account actions", exact: true }).click();
          await page.getByRole("menuitemradio", { name: "Sign out", exact: true }).click();
          await waitPost(page, "logout", () => page.getByRole("alertdialog").getByRole("button", { name: "Sign out", exact: true }).click());
        }
        await pane.getByRole("button", { name: "Sign in again", exact: true }).click();
        await pane.getByLabel("Panel password", { exact: true }).fill("new-one-use-secret");
        const response = page.waitForResponse(endpoint);
        const reread = outcome === "cancel" ? Promise.resolve() : page.waitForResponse("**/api/sub2api");
        release();
        await Promise.all([response, reread]);
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        assert.equal(await pane.locator(".s2-editor-body").count(), 1, "the later recovery form stays open");
        assert.equal(await pane.getByLabel("Panel password", { exact: true }).inputValue(), "new-one-use-secret");
        assert.doesNotMatch(await page.locator("#status").textContent(), /Signed in|The old account changed/);
        assert.equal(f.accounts[0].state, outcome === "ready" ? "logged_out" : "login_required");
        assert.deepEqual(errors, []);
      });
    }
    await t.test("a conflict still closes its own stale form and reports the error", async (st) => {
      const f = fixture(true);
      f.providers = [];
      f.accounts[0].state = "login_required";
      const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      const pane = page.locator("#sub2apiPane");
      await pane.getByRole("button", { name: "Sign in again", exact: true }).click();
      await page.route("**/api/sub2api/login-account", (route) =>
        route.fulfill({ status: 409, json: { error: "This account changed; start again." } }));
      await pane.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
      await waitPost(page, "login-account", () => pane.getByRole("button", { name: "Sign in", exact: true }).click());
      await page.locator("#status").getByText("This account changed; start again.", { exact: true }).waitFor();
      assert.equal(await pane.locator(".s2-editor-body").count(), 0);
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": single queries keep only the current request and page", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const outcome of ["left", "newer", "error-left", "error-newer"]) {
      await t.test(outcome, async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        const keyRate = pane.locator('[data-group="a"] .s2-billing .s2-value').first();
        const sourceQuery = pane.locator('[data-group="a"] .s2-keys')
          .getByRole("button", { name: "Query once", exact: true });
        await keyRate.getByText("0.8×", { exact: true }).waitFor();
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        let first = true;
        const failed = outcome.startsWith("error");
        await page.route("**/api/sub2api/preview", async (route) => {
          if (!first) return route.fallback();
          first = false;
          const captured = structuredClone(f.monitor.accounts[0]);
          if (!route.request().postDataJSON().provider) captured.sources = [];
          await gate;
          await route.fulfill(failed ? { status: 409, json: { error: "The obsolete query failed." } } : { json: captured });
        });
        const pending = page.waitForRequest("**/api/sub2api/preview");
        const left = outcome.endsWith("left");
        await (left ? sourceQuery : pane.locator(".s2-account-head").getByRole("button", { name: "Query once", exact: true })).click();
        await pending;
        const monitored = f.monitor.accounts[0];
        monitored.groups[0].rate = monitored.sources[0].rate = 1.6;
        monitored.sources[0].keys.forEach((key) => { key.rate = 1.6; });
        if (left) {
          await page.locator('#nav [data-view="providers"]').click();
          await page.locator("#view-usage").waitFor({ state: "hidden" });
          await page.locator('#nav [data-view="usage"]').click();
          await keyRate.getByText("1.6×", { exact: true }).waitFor();
        } else {
          await waitPost(page, "preview", () => sourceQuery.click());
          await keyRate.getByText("1.6×", { exact: true }).waitFor();
        }
        const reads = f.providerReads;
        const response = page.waitForResponse("**/api/sub2api/preview");
        release();
        await response;
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        assert.equal(await keyRate.textContent(), "1.6×", "a late query cannot replace newer monitoring or preview");
        assert.doesNotMatch(await page.locator("#status").textContent(), /obsolete query/);
        assert.equal(f.providerReads, reads, "an obsolete conflict does not start recovery");
        assert.equal(await pane.getByText("One-time query", { exact: true }).count(), left ? 0 : 1);
        await page.locator("#usageReload").click();
        await page.evaluate(() => refreshUsage(false));
        assert.equal(await keyRate.textContent(), "1.6×", "a refresh retains the current view");
        assert(await sourceQuery.isEnabled());
        assert.deepEqual(errors, []);
      });
    }
    await t.test("a current query reports its error and releases its button", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      const query = page.locator("#sub2apiPane .s2-account-head").getByRole("button", { name: "Query once", exact: true });
      await page.route("**/api/sub2api/preview", (route) =>
        route.fulfill({ status: 400, json: { error: "The current query failed." } }));
      await query.click();
      await page.locator("#status").getByText("The current query failed.", { exact: true }).waitFor();
      assert(await query.isEnabled());
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": newer successful monitoring samples replace only older matching previews", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const scenario of ["newer", "group-success", "billing-success", "partial-key",
      "older", "same", "failed-attempt", "off", "snapshot-off", "other-generation", "other-settings"]) {
      await t.test(scenario, async (st) => {
        const f = fixture(true);
        const previewAt = "2026-10-16T01:01:00Z", later = "2026-10-16T01:02:00Z";
        f.previewAccount = structuredClone(f.monitor.accounts[0]);
        f.previewAccount.lastSuccess = previewAt;
        if (scenario === "off") f.configs[account.id].enabled = false;
        const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane"), row = pane.locator('[data-group="a"]');
        const keyRate = row.locator(".s2-billing .s2-value").first();
        await keyRate.getByText("0.8×", { exact: true }).waitFor();
        await page.locator("#usageEvery").click();
        await page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
        await waitPost(page, "preview", () => row.locator(".s2-keys")
          .getByRole("button", { name: "Query once", exact: true }).click());
        await pane.getByText("One-time query", { exact: true }).waitFor();
        await page.evaluate(() => refreshUsage(false));
        assert.equal(await pane.getByText("One-time query", { exact: true }).count(), 1,
          "an unchanged GET and the older baseline do not expire a newer preview");

        const monitored = f.monitor.accounts[0];
        monitored.lastAttempt = later;
        monitored.groups[0].rate = monitored.sources[0].rate = 1.6;
        monitored.sources[0].keys.forEach((key) => { key.rate = 1.6; });
        monitored.lastSuccess = scenario === "same" ? previewAt
          : ["older", "failed-attempt", "group-success", "billing-success", "partial-key"].includes(scenario) ? stamp : later;
        if (scenario === "failed-attempt") monitored.error = "All monitored resources are temporarily unavailable.";
        if (scenario === "group-success") {
          monitored.groupsLastSuccess = later;
          monitored.billingError = "Key billing is temporarily unavailable.";
        }
        if (scenario === "billing-success") {
          monitored.billingLastSuccess = later;
          monitored.groupsError = "Groups are temporarily unavailable.";
        }
        if (scenario === "partial-key") {
          monitored.groupsError = "Groups are temporarily unavailable.";
          monitored.billingError = "The second key is temporarily unavailable.";
          delete monitored.sources[0].rate;
          monitored.sources[0].keys[0].lastSuccess = later;
          delete monitored.sources[0].keys[1].rate;
          monitored.sources[0].keys[1].error = monitored.billingError;
        }
        if (scenario === "snapshot-off") monitored.enabled = false;
        if (scenario === "other-generation") monitored.generation--;
        if (scenario === "other-settings") monitored.settingsRevision = "older-settings";
        await page.evaluate(() => refreshUsage(false));
        const accepted = await page.evaluate(async () => (await import("/sub2api/common.js")).monitorOf("session-one"));
        assert.equal(accepted.sources[0].keys[0].rate, 1.6, "the normal refresh accepts the changed monitor snapshot");
        const replaced = ["newer", "group-success", "billing-success", "partial-key"].includes(scenario);
        assert.equal(await keyRate.textContent(), replaced ? "1.6×" : "0.8×",
          "only a strictly newer successful sample from the active matching monitor replaces its preview");
        if (scenario === "partial-key") assert.equal(await row.locator('.s2-billing [data-key="key-one"] .s2-value').textContent(), "1.6×",
          "a successful key is visible even when its sibling and group read fail");
        assert.equal(await pane.getByText("One-time query", { exact: true }).count(), replaced ? 0 : 1);
        assert.equal(posted(f, "preview").length, 1, "refreshing never performs another panel query");
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": account ready distinguishes its own generation from an external change", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const method of ["password", "browser"]) for (const changed of ["same", "logout", "new-login"]) {
      await t.test(method + " with " + changed + " generation", async (st) => {
        const f = fixture(true);
        f.accounts[0].state = "login_required";
        f.loginReady = true;
        const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        await pane.getByRole("button", { name: "Sign in again", exact: true }).click();
        const body = pane.locator(".s2-editor-body");
        await body.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
        const original = await body.elementHandle();
        st.after(() => original.dispose());
        let release;
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        let first = true;
        await page.route("**/api/providers", async (route) => {
          if (first) { first = false; await gate; }
          await route.fallback();
        });
        const pending = page.waitForRequest("**/api/providers");
        await body.getByRole("button", { name: method === "password" ? "Sign in" : "Sign in with browser verification", exact: true }).click();
        await pending;
        assert.equal(f.accounts[0].generation, account.generation + 1);
        const external = changed === "logout" || changed === "new-login";
        if (external) {
          f.accounts[0].generation++;
          f.accounts[0].state = changed === "logout" ? "logged_out" : "ready";
        } else {
          await pane.getByRole("checkbox", { name: "Only changed groups" }).check();
          assert.equal(await original.evaluate((node) => node.isConnected), true, "repainting the older DTO preserves the accepted login");
        }
        // The timer uses this same refresh while the sign-in's final provider read is held.
        await page.evaluate(() => refreshUsage(false));
        assert.equal(await page.evaluate(async () =>
          (await import("/sub2api/common.js")).accountOf("session-one")?.generation), f.accounts[0].generation);
        if (!external) {
          assert.equal(await original.evaluate((node) => node.isConnected), true, "this login's generation keeps its pending form");
        } else {
          await page.waitForFunction((node) => !node.isConnected, original);
          await pane.getByLabel("Panel password", { exact: true }).fill("replacement-form-secret");
        }
        const response = page.waitForResponse("**/api/providers");
        const reread = page.waitForResponse("**/api/sub2api");
        release();
        await Promise.all([response, reread]);
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        if (!external) {
          await pane.locator(".s2-editor-body").waitFor({ state: "hidden" });
          assert.equal(await pane.getByRole("button", { name: "Sign in", exact: true }).count(), 0);
          await pane.locator(".s2-account-status").getByText("Signed in", { exact: true }).waitFor();
        } else {
          assert.equal(await pane.locator(".s2-editor-body").count(), 1);
          assert.equal(await pane.getByLabel("Panel password", { exact: true }).inputValue(), "replacement-form-secret");
          assert.doesNotMatch(await page.locator("#status").textContent(), /Signed in/);
        }
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": monitor toggles recover conflicts before the next explicit write", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const outcome of ["conflict", "failure"]) {
      await t.test(outcome, async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        const watch = pane.getByRole("switch", { name: "Monitor this account", exact: true });
        const control = watch;
        await watch.waitFor();
        await page.locator("#usageEvery").click();
        await page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
        if (outcome === "conflict") {
          const other = await open(browser, f, "en", 1100, "usage");
          st.after(() => other.context.close());
          const otherPane = other.page.locator("#sub2apiPane");
          await otherPane.locator(".s2-account-head").waitFor();
          await other.page.locator("#usageEvery").click();
          await other.page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
          const otherControl = otherPane.getByRole("switch", { name: "Monitor this account", exact: true });
          await waitPost(other.page, "monitor", () => otherControl.click());
          await other.page.waitForFunction(async () =>
            (await import("/sub2api/common.js")).configOf("session-one").revision === "settings-1-next");
          assert.deepEqual(other.errors, []);
        } else {
          let failed = false;
          await page.route("**/api/sub2api/monitor", (route) => {
            if (failed) return route.fallback();
            failed = true;
            f.posted.push({ action: "monitor", body: route.request().postDataJSON() });
            return route.fulfill({ status: 503, json: { error: "Monitoring is temporarily unavailable." } });
          });
        }
        const expected = structuredClone(f.configs[account.id]), writes = posted(f, "monitor").length, reads = f.panelReads;
        await waitPost(page, "monitor", () => control.click());
        const error = outcome === "conflict" ? "The monitor settings changed; read them again." : "Monitoring is temporarily unavailable.";
        await page.locator("#status").getByText(error, { exact: true }).waitFor();
        assert.deepEqual(f.configs[account.id], expected, "an unsuccessful toggle keeps the saved configuration");
        assert.equal(posted(f, "monitor").length, writes + 1, "recovery never replays the rejected write");
        assert.equal(posted(f, "monitor").at(-1).body.settingsRevision, "settings-1");
        assert.equal(await control.isEnabled(), true, "the failed control remains available for an explicit retry");
        assert.deepEqual(await page.evaluate(async () => (await import("/sub2api/common.js")).configOf("session-one")), expected,
          "conflicts recover the authoritative settings before the next write");
        assert.equal(await watch.isChecked(), expected.enabled);
        if (outcome === "conflict") assert(f.panelReads > reads, "a conflict rereads the shared account settings");
        else assert.equal(f.panelReads, reads, "an ordinary error only restores the control");
        await waitPost(page, "monitor", () => control.click());
        await page.waitForFunction((revision) =>
          import("/sub2api/common.js").then((m) => m.configOf("session-one").revision === revision), nextVersion(expected.revision));
        assert.equal(posted(f, "monitor").length, writes + 2);
        assert.equal(posted(f, "monitor").at(-1).body.settingsRevision, expected.revision);
        assert.equal(f.configs[account.id].enabled, !expected.enabled);
        assert.deepEqual(f.configs[account.id].groups, expected.groups, "the retry preserves legacy group references");
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": saved monitor toggles survive a failed refresh without replaying writes", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    await t.test("account", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      const pane = page.locator("#sub2apiPane");
      const watch = pane.getByRole("switch", { name: "Monitor this account", exact: true });
      const control = watch;
      await watch.waitFor();
      await page.locator("#usageEvery").click();
      await page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
      const initial = structuredClone(f.configs[account.id]);
      const refreshError = "The saved monitor state could not be refreshed.";
      let failed = false;
      await page.route("**/api/sub2api", (route) => {
        if (failed) return route.fallback();
        failed = true;
        return route.fulfill({ status: 503, json: { error: refreshError } });
      });

      await waitPost(page, "monitor", () => control.click());
      await page.locator("#status").getByText(refreshError, { exact: true }).waitFor();
      await page.waitForFunction(() => !document.querySelector("#sub2apiPane .s2-account-head input[type=checkbox]")?.disabled);
      const saved = structuredClone(f.configs[account.id]);
      assert.equal(saved.revision, "settings-1-next");
      assert.equal(saved.enabled, false);
      assert.deepEqual(saved.groups, initial.groups);
      assert.equal(await control.isChecked(), false,
        "a committed setting remains selected even when its refresh fails");
      assert.equal(posted(f, "monitor").length, 1, "a failed refresh never replays the committed write");
      const filter = pane.getByRole("checkbox", { name: "Only changed groups", exact: true });
      await filter.click();
      await filter.click();
      assert.equal(await control.isChecked(), false,
        "filter redraws keep the confirmed setting rather than the pre-write value");
      assert.deepEqual(await page.evaluate(async () => (await import("/sub2api/common.js")).configOf("session-one")),
        { ...saved, revision: initial.revision }, "only the confirmed fields change; a failed refresh cannot invent a revision");

      await waitPost(page, "monitor", () => control.click());
      await page.locator("#status").getByText("The monitor settings changed; read them again.", { exact: true }).waitFor();
      assert.equal(posted(f, "monitor").length, 2, "the stale revision is rejected without replaying the explicit write");
      assert.equal(posted(f, "monitor").at(-1).body.settingsRevision, initial.revision);
      assert.deepEqual(f.configs[account.id], saved, "the rejected write cannot replace the committed settings");
      assert.equal(await control.isChecked(), false);
      assert.deepEqual(await page.evaluate(async () => (await import("/sub2api/common.js")).configOf("session-one")), saved);

      await waitPost(page, "monitor", () => control.click());
      await page.waitForFunction((revision) =>
        import("/sub2api/common.js").then((m) => m.configOf("session-one").revision === revision), nextVersion(saved.revision));
      assert.equal(posted(f, "monitor").length, 3);
      assert.equal(posted(f, "monitor").at(-1).body.settingsRevision, saved.revision);
      assert.equal(await control.isChecked(), true);
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": delayed monitor writes cannot overwrite newer accepted settings or account generations", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const newer of ["settings", "generation"]) {
      await t.test(newer, async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        const watch = pane.getByRole("switch", { name: "Monitor this account", exact: true });
        const control = watch;
        await watch.waitFor();
        await page.locator("#usageEvery").click();
        await page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
        const initial = structuredClone(f.configs[account.id]);
        let enter, release;
        const committed = new Promise((resolve) => enter = resolve);
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        await page.route("**/api/sub2api/monitor", async (route) => {
          const body = route.request().postDataJSON();
          f.posted.push({ action: "monitor", body });
          f.configs[account.id] = { configured: true, enabled: body.enabled, groups: body.groups,
            revision: nextVersion(body.settingsRevision), generation: body.expectedGeneration };
          enter();
          await gate;
          return route.fulfill({ json: { ok: true } });
        });
        await control.click();
        await committed;
        if (newer === "generation") f.accounts[0].generation++;
        f.configs[account.id] = { ...initial, revision: newer === "settings" ? "newer-settings" : initial.revision,
          generation: f.accounts[0].generation };
        const authoritative = structuredClone(f.configs[account.id]);
        await page.evaluate(async () => (await import("/sub2api/common.js")).read());
        const refreshError = "The delayed settings refresh failed.";
        await page.route("**/api/sub2api", (route) =>
          route.fulfill({ status: 503, json: { error: refreshError } }));
        release();
        await page.locator("#status").getByText(refreshError, { exact: true }).waitFor();
        const filter = pane.getByRole("checkbox", { name: "Only changed groups", exact: true });
        await filter.click();
        await filter.click();
        assert.deepEqual(await page.evaluate(async () => (await import("/sub2api/common.js")).configOf("session-one")), authoritative,
          "an older completed POST cannot replace settings or identity accepted by a newer read");
        assert.equal(await watch.isChecked(), authoritative.enabled);
        assert.equal(posted(f, "monitor").length, 1, "a failed refresh never replays the delayed write");
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": stale monitor controls cannot borrow a revision accepted during refresh", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    await t.test("account", async (st) => {
      const f = fixture(true);
      const a = await open(browser, f, "en", 1100, "usage");
      const b = await open(browser, f, "en", 1100, "usage");
      st.after(() => a.context.close());
      st.after(() => b.context.close());
      const control = (client) => client.page.locator("#sub2apiPane")
        .getByRole("switch", { name: "Monitor this account", exact: true });
      for (const client of [a, b]) {
        await client.page.locator("#sub2apiPane .s2-account-head").waitFor();
        await client.page.locator("#usageEvery").click();
        await client.page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
        // A timer read may have started while the other window was opening.
        await client.page.evaluate(() => usageFlight);
      }

      const otherControl = control(b);
      await waitPost(b.page, "monitor", () => otherControl.click());
      await b.page.waitForFunction(async () =>
        (await import("/sub2api/common.js")).configOf("session-one").revision === "settings-1-next");
      const authoritative = structuredClone(f.configs[account.id]);

      let enterRefresh, releaseRefresh, firstProviderRead = true;
      const requested = new Promise((resolve) => enterRefresh = resolve);
      const gate = new Promise((resolve) => releaseRefresh = resolve);
      st.after(releaseRefresh);
      await a.page.route("**/api/providers", async (route) => {
        if (!firstProviderRead) return route.fallback();
        firstProviderRead = false;
        enterRefresh();
        await gate;
        return route.fallback();
      });
      await a.page.locator("#usageReload").click();
      await requested;
      await a.page.waitForFunction(async () =>
        (await import("/sub2api/common.js")).configOf("session-one").revision === "settings-1-next");
      assert.equal(await control(a).isChecked(), true);

      const writes = posted(f, "monitor").length;
      const reads = f.panelReads;
      await waitPost(a.page, "monitor", () => control(a).click());
      await a.page.locator("#status").getByText("The monitor settings changed; read them again.", { exact: true }).waitFor();
      assert.equal(posted(f, "monitor").length, writes + 1, "a stale write is never replayed");
      assert.equal(posted(f, "monitor").at(-1).body.settingsRevision, "settings-1",
        "the old controls submit the revision from their own rendering");
      assert.deepEqual(f.configs[account.id], authoritative, "the stale write cannot replace current settings");
      assert(f.panelReads > reads, "the conflict reloads authoritative settings");
      assert.equal(await control(a).isChecked(), authoritative.enabled);

      releaseRefresh();
      await a.page.waitForFunction(async () =>
        (await import("/sub2api/common.js")).configOf("session-one").revision === "settings-1-next");
      assert.deepEqual(a.errors, []);
      assert.deepEqual(b.errors, []);
    });
  });
  test(engine + ": account writes replace earlier reads without stale results or failures", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const outcome of ["success-first", "failure", "success-last", "conflict", "concurrent-post"]) {
      await t.test(outcome, async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        const watch = pane.getByRole("switch", { name: "Monitor this account", exact: true });
        await watch.waitFor();
        await page.locator("#usageEvery").click();
        await page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
        const captured = structuredClone({ accounts: f.accounts, monitor: f.monitor, configs: f.configs, capability: f.capability });
        let entered, release, releaseWritten, first = true, holdWritten = outcome === "concurrent-post";
        const requested = new Promise((resolve) => entered = resolve);
        const gate = new Promise((resolve) => release = resolve);
        const writtenGate = new Promise((resolve) => releaseWritten = resolve);
        st.after(release);
        st.after(releaseWritten);
        await page.route("**/api/sub2api", async (route) => {
          if (!first) {
            if (!holdWritten) return route.fallback();
            holdWritten = false;
            const written = structuredClone({ accounts: f.accounts, monitor: f.monitor, configs: f.configs, capability: f.capability });
            await writtenGate;
            return route.fulfill({ json: written });
          }
          first = false;
          entered();
          await gate;
          await route.fulfill(outcome === "failure"
            ? { status: 503, json: { error: "The earlier read failed." } } : { json: captured });
        });
        const refreshing = page.evaluate(() => refreshUsage(false));
        await requested;
        if (outcome === "conflict") {
          f.configs[account.id].revision = "external-settings";
          const query = pane.locator(".s2-account-head").getByRole("button", { name: "Query once", exact: true });
          await waitPost(page, "preview", () => query.click());
          await page.waitForFunction(async () =>
            (await import("/sub2api/common.js")).configOf("session-one").revision === "external-settings");
        } else {
          const written = outcome === "concurrent-post" && page.waitForRequest("**/api/sub2api");
          await waitPost(page, "monitor", () => watch.uncheck());
          if (written) {
            await written;
            await pane.getByRole("checkbox", { name: "Only changed groups", exact: true }).check();
            await waitPost(page, "monitor", () => watch.click());
            await page.locator("#status").getByText("The monitor settings changed; read them again.", { exact: true }).waitFor();
            releaseWritten();
          }
          if (outcome === "success-first") release();
          await page.waitForFunction(() => {
            const watch = document.querySelector(".s2-account-head input[type=checkbox]");
            return watch && !watch.disabled && !watch.checked;
          });
          assert.equal(f.configs[account.id].enabled, false);
          if (outcome === "success-last") {
            await waitPost(page, "monitor", () => watch.check());
            await page.waitForFunction(async () =>
              (await import("/sub2api/common.js")).configOf("session-one").revision === "settings-1-next-next");
            assert(await watch.isChecked());
          }
        }
        release();
        await refreshing;
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        const current = await page.evaluate(async () => (await import("/sub2api/common.js")).configOf("session-one"));
        assert.deepEqual(current, f.configs[account.id], "an earlier read cannot overwrite the state recovered after a write or conflict");
        assert.doesNotMatch(await page.locator("#status").textContent(), /The earlier read failed/, "an obsolete failure cannot replace the successful write's feedback");
        if (outcome !== "conflict") {
          const enabled = outcome === "success-last";
          assert.equal(await watch.isChecked(), enabled);
          await waitPost(page, "monitor", () => watch.setChecked(!enabled));
          await page.waitForFunction((enabled) => {
            const watch = document.querySelector(".s2-account-head input[type=checkbox]");
            return watch && !watch.disabled && watch.checked === enabled;
          }, !enabled);
          assert.equal(f.configs[account.id].enabled, !enabled, "the next write uses the refreshed settings revision");
        }
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": provider snapshots keep accepted writes across readers and late responses", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const outcome of ["success", "failure", "loadProviders", "providerAction", "conflict"]) {
      await t.test(outcome, async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const pane = page.locator("#sub2apiPane");
        const applied = pane.locator('[data-group="a"] .s2-local .s2-source');
        const apply = applied.getByRole("button", { name: "Apply rate", exact: true }).first();
        await apply.waitFor();
        await page.locator("#usageEvery").click();
        await page.getByRole("menuitemradio", { name: "Off", exact: true }).click();
        const old = structuredClone({ providers: f.providers, presets: [], excluded: [], gateway: { running: true, window: true } });
        let entered, release, first = true;
        const requested = new Promise((resolve) => entered = resolve);
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        await page.route("**/api/providers", async (route) => {
          if (!first) return route.fallback();
          first = false;
          entered();
          await gate;
          await route.fulfill(outcome === "failure"
            ? { status: 503, json: { error: "The older provider read failed." } } : { json: old });
        });
        const refreshing = outcome === "loadProviders" ? page.evaluate(() => loadProviders())
          : page.evaluate(() => refreshUsage(true));
        await requested;
        const expected = outcome === "conflict" ? 1.4 : .8;
        if (outcome === "providerAction") {
          await page.evaluate(() => providerAction("off", { id: "relay" }, "Provider off"));
          assert.equal(f.providers[0].off, true);
        } else {
          if (outcome === "conflict") f.failNext = "apply-key-rate";
          await apply.click();
          await waitPost(page, "apply-key-rate", () => page.getByRole("alertdialog").getByRole("button", { name: "Apply rate", exact: true }).first().click());
          await page.waitForFunction((value) => (providers.providers[0].sub2api.keys?.[0]?.priceRate || providers.providers[0].priceRate) === value, expected);
          await applied.locator('[data-key="key-one"] > .s2-value').filter({ hasText: expected + "×" }).waitFor();
        }
        release();
        await refreshing;
        await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
        assert.deepEqual(await page.evaluate(() => providers.providers[0]), f.providers[0],
          "an older provider GET cannot replace a newer accepted source");
        if (outcome !== "providerAction") {
          assert.equal(await applied.locator('[data-key="key-one"] > .s2-value').textContent(), expected + "×");
          assert.equal(posted(f, "apply-key-rate").length, 1, "reads and conflicts never replay a price write");
        }
        assert.doesNotMatch(await page.locator("#status").textContent(), /The older provider read failed/);
        assert.deepEqual(errors, []);
      });
    }
    await t.test("crossed provider write responses follow the current list without replay", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      await page.locator(".s2-account-head").waitFor();
      await page.evaluate(() => { usageEvery = 0; });
      let entered, release;
      const requested = new Promise((resolve) => entered = resolve);
      const gate = new Promise((resolve) => release = resolve);
      st.after(release);
      await page.route("**/api/provider/off", async (route) => {
        f.posted.push({ action: "provider/off", body: route.request().postDataJSON() });
        f.providers[0].off = true;
        const old = structuredClone({ providers: f.providers, presets: [], excluded: [], gateway: { running: true, window: true } });
        entered();
        await gate;
        await route.fulfill({ json: old });
      });
      const earlier = page.evaluate(() => providerAction("off", { id: "relay" }, "Provider off"));
      await requested;
      await page.evaluate(() => providerAction("on", { id: "relay" }, "Provider on"));
      release();
      await earlier;
      assert.equal(f.providers[0].off, false);
      assert.equal(await page.evaluate(() => providers.providers[0].off), false,
        "a delayed earlier POST response cannot restore its older provider snapshot");
      assert.equal(posted(f, "provider/off").length, 1);
      assert.equal(posted(f, "provider/on").length, 1);
      assert.deepEqual(errors, []);
    });
    await t.test("a current provider read failure is reported once and keeps known state", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      await page.locator(".s2-account-head").waitFor();
      await page.evaluate(() => { usageEvery = 0; });
      let reads = 0;
      await page.route("**/api/providers", (route) => {
        reads++;
        return route.fulfill({ status: 503, json: { error: "The current provider read failed." } });
      });
      const result = await page.evaluate(async () => {
        try { await (await import("/sub2api/common.js")).reloadProviders(); }
        catch (e) { return { error: e.message, priceRate: providers.providers[0].priceRate }; }
      });
      assert.deepEqual(result, { error: "The current provider read failed.", priceRate: 1.2 });
      assert.equal(reads, 1, "a genuine current failure is not retried forever");
      assert.deepEqual(errors, []);
    });
    await t.test("waiting for models supersedes an ordinary pending list", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      await page.locator(".s2-account-head").waitFor();
      await page.evaluate(() => { usageEvery = 0; });
      const old = structuredClone({ providers: f.providers, presets: [], excluded: [], gateway: { running: true, window: true } });
      let entered, release, first = true, waited = 0;
      const requested = new Promise((resolve) => entered = resolve);
      const gate = new Promise((resolve) => release = resolve);
      st.after(release);
      await page.route("**/api/providers*", async (route) => {
        if (new URL(route.request().url()).searchParams.has("wait")) { waited++; return route.fallback(); }
        if (!first) return route.fallback();
        first = false;
        entered();
        await gate;
        await route.fulfill({ json: old });
      });
      const ordinary = page.evaluate(() => api("providers"));
      await requested;
      f.providers[0].models.push({ id: "arrived-model", name: "Arrived", on: true });
      const complete = await page.evaluate(() => api("providers?wait=1"));
      assert.equal(waited, 1, "a model-waiting read must reach its own endpoint");
      release();
      assert.deepEqual(await ordinary, complete, "an older ordinary response follows the completed model list");
      assert.equal(complete.providers[0].models.at(-1).id, "arrived-model");
      assert.deepEqual(errors, []);
    });
    await t.test("provider reads keep their own response headers and cancellation", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      await page.locator(".s2-account-head").waitFor();
      await page.evaluate(() => { usageEvery = 0; });
      let entered, release, calls = 0;
      const requested = new Promise((resolve) => entered = resolve);
      const gate = new Promise((resolve) => release = resolve);
      st.after(release);
      await page.route("**/api/providers*", async (route) => {
        if (++calls === 3) entered();
        const marker = new URL(route.request().url()).searchParams.get("own") || "shared";
        await gate;
        await route.fulfill({ headers: { "X-Own-Read": marker },
          json: { providers: f.providers, presets: [], excluded: [], gateway: { running: true, window: true } } });
      });
      await page.evaluate(() => {
        window.providerReadAbort = new AbortController();
        window.providerReadResults = [
          api("providers"),
          api("providers?own=headers", undefined, (headers) => { window.providerReadHeader = headers.get("X-Own-Read"); }),
          api("providers?own=cancel", undefined, undefined, window.providerReadAbort.signal).then(
            () => "unexpected success", (e) => e.name),
        ];
      });
      await requested;
      assert.equal(await page.evaluate(async () => {
        window.providerReadAbort.abort();
        return window.providerReadResults[2];
      }), "AbortError");
      release();
      await page.evaluate(() => Promise.all(window.providerReadResults));
      assert.equal(calls, 3, "callback and cancellable reads do not share another request");
      assert.equal(await page.evaluate(() => window.providerReadHeader), "headers");
      assert.deepEqual(errors, []);
    });
    await t.test("a model-waiting read keeps waiting after a source write invalidates it", async (st) => {
      const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
      st.after(() => context.close());
      await page.locator(".s2-account-head").waitFor();
      await page.evaluate(() => { usageEvery = 0; });
      const old = structuredClone({ providers: f.providers, presets: [], excluded: [], gateway: { running: true, window: true } });
      let entered, release, waited = 0;
      const requested = new Promise((resolve) => entered = resolve);
      const gate = new Promise((resolve) => release = resolve);
      st.after(release);
      await page.route("**/api/providers?wait=1", async (route) => {
        if (++waited === 1) {
          entered();
          await gate;
          return route.fulfill({ json: old });
        }
        f.providers[0].models.push({ id: "arrived-model", name: "Arrived", on: true });
        return route.fallback();
      });
      const waiting = page.evaluate(() => api("providers?wait=1"));
      await requested;
      await page.evaluate(() => providerAction("off", { id: "relay" }, "Provider off"));
      release();
      const result = await waiting;
      assert.equal(waited, 2, "invalidating a waiting read must not downgrade it to a non-waiting read");
      assert.equal(result.providers[0].models.at(-1).id, "arrived-model");
      assert.equal(result.providers[0].off, true);
      assert.deepEqual(errors, []);
    });
  });
  test(engine + ": provider write snapshots follow reads without reusing a pre-commit list", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const commit of ["before read", "after read"]) {
      await t.test(commit, async (st) => {
        const f = fixture(true), first = await open(browser, f, "en", 1100, "usage");
        st.after(() => first.context.close());
        await first.page.locator(".s2-account-head").waitFor();
        await first.page.evaluate(() => { usageEvery = 0; });
        let entered, release;
        const requested = new Promise((resolve) => entered = resolve);
        const gate = new Promise((resolve) => release = resolve);
        st.after(release);
        const all = () => structuredClone({ providers: f.providers, presets: [], excluded: [], gateway: { running: true, window: true } });
        await first.page.route("**/api/provider/off", async (route) => {
          f.posted.push({ action: "provider/off", body: route.request().postDataJSON() });
          let old;
          if (commit === "before read") { f.providers[0].off = true; old = all(); }
          entered();
          await gate;
          if (commit === "after read") { f.providers[0].off = true; old = all(); }
          await route.fulfill({ json: old });
        });
        const earlier = first.page.evaluate(() => providerAction("off", { id: "relay" }, "Provider off"));
        await requested;
        if (commit === "before read") {
          const second = await open(browser, f, "en", 1100, "usage");
          st.after(() => second.context.close());
          await second.page.locator(".s2-account-head").waitFor();
          await second.page.evaluate(() => { usageEvery = 0; });
          const apply = second.page.locator('[data-group="a"] .s2-local .s2-source')
            .getByRole("button", { name: "Apply rate", exact: true }).first();
          await apply.click();
          await waitPost(second.page, "apply-key-rate", () => second.page.getByRole("alertdialog")
            .getByRole("button", { name: "Apply rate", exact: true }).first().click());
          await second.page.waitForFunction(() => providers.providers[0].sub2api.keys?.[0]?.priceRate === .8);
          assert.deepEqual(second.errors, []);
        }
        await first.page.evaluate(() => loadProviders());
        const afterRead = await first.page.evaluate(() => structuredClone(providers.providers[0]));
        assert.equal(localRate(afterRead), commit === "before read" ? .8 : 1.2);
        assert.equal(!!afterRead.off, commit === "before read");
        const reads = f.providerReads;
        release();
        await earlier;
        assert.deepEqual(await first.page.evaluate(() => providers.providers[0]), f.providers[0],
          "a delayed write response cannot replace a newer read or reuse a read from before its commit");
        assert(f.providerReads > reads, "the uncertain write response must obtain a fresh provider list");
        assert.equal(posted(f, "provider/off").length, 1, "refreshing the list never replays the write");
        assert.equal(posted(f, "apply-key-rate").length, commit === "before read" ? 1 : 0);
        assert.deepEqual(first.errors, []);
      });
    }
  });
  test(engine + ": crossed Key imports retain counts while following the latest provider list", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
    t.after(() => context.close());
    await page.locator(".s2-account-head").waitFor();
    await page.evaluate(() => { usageEvery = 0; });
    let entered, release;
    const requested = new Promise((resolve) => entered = resolve);
    const gate = new Promise((resolve) => release = resolve);
    t.after(release);
    await page.route("**/api/keys/import", async (route) => {
      const old = structuredClone({ providers: f.providers, presets: [], excluded: [],
        gateway: { running: true, window: true }, added: 2, had: 1 });
      entered();
      await gate;
      await route.fulfill({ json: old });
    });
    const imported = page.evaluate(() => api("keys/import", { id: "relay", key: "fictional-import" }));
    await requested;
    await page.evaluate(() => providerAction("off", { id: "relay" }, "Provider off"));
    release();
    const result = await imported;
    assert.equal(result.providers[0].off, true);
    assert.deepEqual({ added: result.added, had: result.had }, { added: 2, had: 1 },
      "a refreshed provider list must preserve the completed import's own result counts");
    assert.deepEqual(errors, []);
  });
  test(engine + ": linking a Usage Key preserves the other Key's independent group", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const mode of ["unbound", "bound-empty", "bound-confirmed"]) {
      await t.test(mode, async (st) => {
        const f = fixture(true);
        const expected = mode === "bound-confirmed" ? ["key-one", "key-two"] : ["key-one"];
        if (mode === "unbound") delete f.providers[0].sub2api;
        if (mode === "bound-empty") f.providers[0].sub2api.keyIds = [];
        if (mode === "bound-confirmed") Object.assign(f.providers[0].sub2api, { groupId: "b", groupName: "Standard" });
        f.monitor.accounts[0].sources = [];
        const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const link = page.locator('#sub2apiPane [data-group="a"]').getByRole("button", { name: "Link key", exact: true });
        await link.click();
        await waitPost(page, "bind-key", () => page.getByRole("menuitemcheckbox", { name: /Relay · First/ }).click());
        await page.waitForFunction(() => providers.providers[0].sub2api?.keys?.[0]?.groupId === "a");
        assert.equal(posted(f, "bind-key").length, 1);
        const body = posted(f, "bind-key")[0].body;
        assert.equal(body.session, account.id);
        assert.equal(body.groupId, "a");
        assert.equal(body.keyId, "key-one");
        assert.deepEqual(boundKeys(f.providers[0]).map((k) => k.id), expected, "only the explicitly selected Key is added");
        if (mode === "bound-confirmed") assert.equal(boundKeys(f.providers[0]).find((key) => key.id === "key-two").groupId, "b");
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": a first login keeps a single Key's group an explicit reachable choice", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture();
    f.loginReady = true;
    f.providers[0].keyList.splice(1);
    const { page, context, errors } = await open(browser, f, "en", 1100);
    t.after(() => context.close());
    await page.locator(".row.provider", { hasText: "Relay" }).click();
    const box = page.locator(".s2-editor");
    await box.locator("summary").click();
    await box.getByLabel("Panel email", { exact: true }).fill(account.email);
    await box.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
    await waitPost(page, "login", () => box.getByRole("button", { name: "Sign in", exact: true }).click());
    const pick = box.getByRole("button", { name: "Panel group", exact: true }).first();
    await wheelTo(page, pick);
    assert.deepEqual(f.providers[0].sub2api.keyIds || [], [], "sign-in alone does not confirm Key membership");
    await pick.click();
    await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: /Premium/ }).click());
    await pick.filter({ hasText: "Premium" }).waitFor();
    assert.equal(posted(f, "bind-key").at(-1).body.keyId, "key-one");
    assert.deepEqual(boundKeys(f.providers[0]), [{ id: "key-one", groupId: "a", groupName: "Premium" }]);
    await pick.click();
    await waitPost(page, "bind-key", () => page.getByRole("menuitemradio", { name: "No group selected", exact: true }).click());
    assert.equal(boundKeys(f.providers[0])[0].groupId, "", "clearing the selection unlinks this Key");
    assert.deepEqual(errors, []);
  });
  test(engine + ": filtered account sign-ins finish and restore verification on remount", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const method of ["password", "browser", "two-factor", "password-error", "two-factor-error"]) {
      await t.test(method, async (st) => {
        const twoFactor = method.startsWith("two-factor"), failed = method.endsWith("-error");
        const f = fixture(true);
        f.accounts[0].state = "login_required";
        const second = { ...structuredClone(account), id: "session-two", base: "https://second.example.com", email: "second@example.com" };
        f.accounts.push(second);
        f.configs[second.id] = structuredClone(f.configs[account.id]);
        f.monitor.accounts.push({ ...structuredClone(f.monitor.accounts[0]), sessionId: second.id, site: second.base, sources: [] });
        const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        await page.evaluate(() => { usageEvery = 0; });
        const pane = page.locator("#sub2apiPane");
        const section = pane.locator(".s2-account").filter({ hasText: account.email });
        const choose = async (name) => {
          await pane.getByRole("button", { name: "All Sub2API sites", exact: true }).click();
          await page.getByRole("menuitemradio", { name, exact: true }).click();
        };
        await section.getByRole("button", { name: "Sign in again", exact: true }).click();
        const finishHidden = async (action, resultState, click) => {
          let entered, release, delivered;
          const requested = new Promise((resolve) => entered = resolve);
          const gate = new Promise((resolve) => release = resolve);
          const completed = new Promise((resolve) => delivered = resolve);
          st.after(release);
          await page.route("**/api/sub2api/" + action, async (route) => {
            const input = route.request().postDataJSON();
            assert.equal(input.expectedGeneration, account.generation);
            f.posted.push({ action, body: input });
            let result = { state: "two_factor", attempt: "hidden-attempt", sessionId: account.id };
            if (resultState === "ready") {
              f.accounts[0].state = "ready";
              f.accounts[0].generation++;
              result = { state: "ready", sessionId: account.id, account: structuredClone(f.accounts[0]) };
            }
            entered();
            await gate;
            await route.fulfill(resultState === "error"
              ? { status: 503, json: { error: "Retryable sign-in error." } } : { json: result });
            delivered();
          });
          await click();
          await requested;
          await choose(second.base);
          assert.equal(await section.count(), 0);
          release();
          await completed;
          if (resultState === "error") await page.locator("#status").getByText("Retryable sign-in error.", { exact: true }).waitFor();
          await page.evaluate(() => new Promise((resolve) => requestAnimationFrame(() => requestAnimationFrame(resolve))));
          await choose("All Sub2API sites");
        };
        await section.getByLabel("Panel password", { exact: true }).fill("one-use-secret");
        await finishHidden(method === "browser" ? "browser-account" : "login-account",
          twoFactor ? "two_factor" : failed ? "error" : "ready", () => section.getByRole("button", {
            name: method === "browser" ? "Sign in with browser verification" : "Sign in", exact: true,
          }).click());
        if (failed && !twoFactor) {
          const retry = section.getByRole("button", { name: "Sign in", exact: true });
          assert(await retry.isEnabled(), "a failed hidden password request releases the owned form's button");
          assert.equal(await section.getByLabel("Panel password", { exact: true }).inputValue(), "");
          await section.getByLabel("Panel password", { exact: true }).fill("retry-secret");
          await finishHidden("login-account", "ready", () => retry.click());
        }
        if (twoFactor) {
          const code = section.getByLabel("Six-digit verification code", { exact: true });
          await code.waitFor();
          assert.equal(posted(f, "cancel").length, 0, "filtering keeps the owned verification attempt");
          await code.fill("123456");
          if (failed) {
            await finishHidden("2fa-account", "error", () => section.getByRole("button", { name: "Verify code", exact: true }).click());
            assert(await section.getByRole("button", { name: "Verify code", exact: true }).isEnabled(),
              "a failed hidden verification request releases the owned form's button");
            assert.equal(await code.inputValue(), "");
            await code.fill("654321");
          }
          await finishHidden("2fa-account", "ready", () => section.getByRole("button", { name: "Verify code", exact: true }).click());
          assert.equal(posted(f, "2fa-account")[0].body.attempt, "hidden-attempt");
        }
        await section.locator(".s2-editor-body").waitFor({ state: "hidden" });
        await section.locator(".s2-account-status").getByText("Signed in", { exact: true }).waitFor();
        assert.equal(await section.getByRole("button", { name: "Sign in", exact: true }).count(), 0);
        assert.equal(await section.getByText("Complete sign-in in the verification window.", { exact: true }).count(), 0);
        assert.equal(await section.locator('input[type="password"], input[autocomplete="one-time-code"]').count(), 0);
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": rate guards reject incomplete and stale snapshots", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(true), mon = f.monitor.accounts[0], original = mon.sources[0], expected = [];
    const setRate = (s, rate) => { s.rate = rate; for (const k of s.keys) k.rate = rate; };
    const cases = [
      ["Upper boundary", (_, s) => setRate(s, 1000), true],
      ["Lower boundary", (_, s) => setRate(s, .001), true],
      ["Above range", (_, s) => setRate(s, 1000.001)],
      ["Too precise", (_, s) => setRate(s, .80001)],
      ["Zero rate", (_, s) => setRate(s, 0)],
      ["Negative rate", (_, s) => setRate(s, -.1)],
      ["No group", (p, s) => { p.sub2api.groupId = s.groupId = ""; }],
      ["Missing version", (_, s) => { delete s.bindingVersion; }],
      ["Missing revision", (_, s) => { delete s.revision; }],
      ["Different session", (p) => { p.sub2api.session = "another-session"; }],
      ["Different group", (p) => { p.sub2api.groupId = "b"; }],
      ["Stale version", (p) => { p.sub2api.version = nextVersion(p.sub2api.version); }],
      ["Stale revision", (p) => { p.sub2apiRevision = "new-revision"; }],
      ["Unconfirmed other key", (p) => { p.keyList.push({ id: "new-key", on: true }); }, true],
      ["Missing other sample", (_, s) => { s.keys.pop(); }, true],
      ["Failed key", (_, s) => { s.keys[0].error = "Billing unavailable"; }],
      ["Failed other key", (_, s) => { s.keys[1].error = "Billing unavailable"; }, true],
      ["Historical key", (_, s) => { s.keys[0].lastRate = s.keys[0].rate; s.keys[0].lastRateAt = stamp; delete s.keys[0].rate; }],
      ["Independent key rate", (_, s) => { s.keys[0].rate = .9; }, true],
      ["Already applied", (p) => { p.priceRate = .8; }],
      ["No enabled keys", (p, s) => { p.keyList.forEach((k) => { k.on = false; }); s.keys = []; }],
      ["Disabled bound key", (p, s) => { p.keyList[1].on = false; s.keys.pop(); s.activeKeyIds.pop(); }, true],
      ["Duplicate identity", (p, s) => { p.keyList.push(structuredClone(p.keyList[0])); p.sub2api.keyIds.push("key-one"); s.keys.push(structuredClone(s.keys[0])); }, true],
    ];
    for (const [name, mutate, allowed = false] of cases) {
      const p = structuredClone(f.providers[0]), s = structuredClone(original);
      p.id = p.name = s.id = s.name = name;
      mutate(p, s);
      f.providers.push(p); mon.sources.push(s); expected.push([name, allowed]);
    }
    mon.observationError = "Observation unavailable";
    const { page, context, errors } = await open(browser, f, "en", 1100, "usage");
    t.after(() => context.close());
    const pane = page.locator("#sub2apiPane");
    await pane.locator(".s2-group").first().waitFor();
    for (const [name, allowed] of expected) {
      const row = pane.locator(`.s2-local .s2-source[data-source="${name}"]`);
      if (name === "Different session") {
        assert.equal(await row.count(), 0, "another account's Key does not appear under this account");
        continue;
      }
      assert.equal(await row.locator(".s2-apply").first().isEnabled(), allowed, name);
    }
    const stale = pane.locator('.s2-billing .s2-source[data-source="Missing version"]');
    assert.match(await stale.textContent(), /Rate not confirmed/);
    assert.match(await stale.locator(".s2-historical").first().textContent(), /Last confirmed: 0.8×/);
    assert.match(await pane.textContent(), /0.80001×/, "an unrepresentable rate remains unrounded");
    assert.equal(posted(f, "apply-key-rate").length, 0);
    assert.equal(posted(f, "refresh").length, 0, "opening the page reads the local snapshot only");
    assert.deepEqual(errors, []);
  });
  test(engine + ": confirmation freezes the displayed rate and provider context", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const change of ["price", "binding", "keys", "rate", "generation"]) {
      await t.test(change, async (st) => {
        const f = fixture(true), { page, context, errors } = await open(browser, f, "en", 1100, "usage");
        st.after(() => context.close());
        const apply = page.locator('[data-group="a"] .s2-local').getByRole("button", { name: "Apply rate", exact: true }).first();
        await apply.click();
        const confirm = page.getByRole("alertdialog");
        await confirm.waitFor();
        const p = f.providers[0], source = f.monitor.accounts[0].sources[0];
        if (change === "price") p.priceRate = 1.4;
        else if (change === "binding") p.sub2api.version = nextVersion(p.sub2api.version);
        else if (change === "keys") p.keyList[0].on = false;
        else if (change === "generation") f.accounts[0].generation++;
        else { source.rate = .9; source.keys.forEach((k) => { k.rate = .9; }); }
        // Inject the same background read the Usage lifecycle performs while the dialog is open.
        await page.evaluate(async () => {
          await window.Sub2API.renderUsage(document.querySelector("#sub2apiPane"), false, true);
        });
        const reads = f.providerReads;
        await confirm.getByRole("button", { name: "Apply rate", exact: true }).first().click();
        await page.locator("#status").filter({ hasText: "The provider or monitored rate changed" }).waitFor();
        assert(f.providerReads > reads, "a stale confirmation rereads before another explicit action");
        assert.equal(posted(f, "apply-key-rate").length, 0, change + " never applies the old confirmation");
        assert.equal(p.priceRate, change === "price" ? 1.4 : 1.2);
        assert.deepEqual(errors, []);
      });
    }
  });
  test(engine + ": failed fields retain dated history and independent resource errors", async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    const f = fixture(true), mon = f.monitor.accounts[0], group = mon.groups[0], source = mon.sources[0];
    f.monitor.running = false;
    Object.assign(mon, { groupsError: "Groups unavailable", billingError: "Billing unavailable", observationError: "Observation unavailable",
      lastAttempt: stamp, nextPoll: "2026-10-16T01:05:00Z", lastObservation: mon.observation, lastObservationAt: stamp });
    mon.observationHistory = observationHistory(mon.observation);
    delete mon.observation;
    Object.assign(group, { lastRate: group.rate, lastRateAt: stamp, lastEnabled: group.enabled, lastEnabledAt: stamp, lastVisible: group.visible, lastVisibleAt: stamp });
    delete group.rate; delete group.enabled; delete group.visible; delete source.rate;
    for (const key of source.keys) {
      Object.assign(key, { lastRate: key.rate, lastRateAt: stamp, error: "Billing unavailable" });
      delete key.rate;
    }
    const { page, context, errors } = await open(browser, f, "en", 420, "usage");
    t.after(() => context.close());
    const pane = page.locator("#sub2apiPane"), a = pane.locator('[data-group="a"]');
    await a.waitFor();
    assert.match(await a.textContent(), /Rate not confirmed/);
    assert.match(await a.locator(".s2-declared .s2-historical").textContent(), /Last confirmed: 0.8×/);
    assert.match(await a.locator(".s2-key .s2-historical").first().textContent(), /Last confirmed: 0.8×/);
    assert.match(await a.locator(".s2-group-name").textContent(), /Last confirmed: Panel enabled/);
    assert.match(await a.locator(".s2-group-name").textContent(), /Last confirmed: Group became visible/);
    assert(await a.locator(".s2-apply").first().isDisabled());
    assert.equal(await pane.locator(".s2-account-details > .s2-observation").count(), 1);
    const details = a.locator(".s2-observation");
    await wheelTo(page, details.locator(":scope > summary"));
    await details.locator(":scope > summary").click();
    const availability = details.locator('[data-metric="availability"]');
    await wheelTo(page, availability.locator("summary"));
    await availability.locator("summary").click();
    assert.match(await availability.textContent(), /Last confirmed: 98%/);
    assert.match(await availability.textContent(), /v2-matrix/);
    assert.match(await pane.locator(".s2-account-status").textContent(), /Background monitoring is stopped/);
    assert.match(await pane.locator(".s2-account-status").textContent(), /Last attempt:/);
    assert.match(await pane.locator(".s2-account-status").textContent(), /Next check:/);
    for (const text of ["Groups: Groups unavailable", "Key billing: Billing unavailable", "Monitor: Observation unavailable"]) {
      assert.match(await pane.textContent(), new RegExp(text));
    }
    await fits(page, pane);
    if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-history-${engine}.png`), fullPage: true });
    group.observation = { source: "v2-snapshot", scope: "group", window: "30m", sampledAt: "2026-10-16T01:10:00Z", match: "id", availability: .99 };
    Object.assign(group.observationHistory.ttftP50Ms, { source: "v1-snapshot", observedAt: "2026-10-15T01:00:00Z" });
    await page.evaluate(async () => {
      await window.Sub2API.renderUsage(document.querySelector("#sub2apiPane"), false, true);
    });
    await wheelTo(page, details.locator(":scope > summary"));
    await details.locator(":scope > summary").click();
    assert.match(await details.textContent(), /99%/);
    assert.equal(await details.locator('[data-metric="availability"]').count(), 0, "the current metric never falls back to its old value");
    const oldTTFT = details.locator('[data-metric="ttftP50Ms"]');
    await wheelTo(page, oldTTFT.locator("summary"));
    await oldTTFT.locator("summary").click();
    assert.match(await oldTTFT.textContent(), /Last confirmed: 45 ms/);
    assert.match(await oldTTFT.textContent(), /v1-snapshot/);
    assert.match(await oldTTFT.textContent(), /10\/15\/2026/, "each old metric retains its own observation time");
    await fits(page, pane);
    if (process.env.ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `sub2api-partial-history-${engine}.png`), fullPage: true });
    assert.deepEqual(errors, []);
  });
}
