const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const words = {
  en: { copy: "Save local recovery copies", save: "Save", apply: "Apply", clear: "Clear recovery copies", cancel: "Cancel" },
  zh: { copy: "保存本地恢复副本", save: "保存", apply: "应用", clear: "清理恢复副本", cancel: "取消" },
  "zh-TW": { copy: "儲存本機復原副本", save: "儲存", apply: "應用", clear: "清除復原副本", cancel: "取消" },
  ja: { copy: "ローカル復元コピーを保存", save: "保存", apply: "適用", clear: "復元コピーを削除", cancel: "キャンセル" },
  de: { copy: "Lokale Wiederherstellungskopien speichern", save: "Speichern", apply: "Anwenden", clear: "Wiederherstellungskopien löschen", cancel: "Abbrechen" },
};

async function open(t, engine, lang, width) {
  const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch(
    process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : { channel: "chromium" }));
  t.after(() => browser.close());
  const page = await browser.newPage({ viewport: { width, height: 840 }, reducedMotion: "reduce" });
  page.setDefaultTimeout(7000);
  const errors = [];
  page.on("pageerror", (e) => errors.push(e.message));
  let providers = { providers: [{
    id: "relay", name: "Relay", icon: "generic", chat: "", responses: "https://relay.test/v1",
    anthropic: "", catalog: "", models: [{ id: "m", name: "M", on: true }], agents: [],
    fallback: [], headers: {}, key: { set: true, masked: "sk-1234" }, keyList: [],
    balanceToken: { takes: false, set: false }, nativeCompaction: true, carriesCompaction: true,
  }], presets: [], excluded: [], gateway: { running: true, window: true } };
  const copyState = { config: { providers: [], maxBytes: 512 * 1048576 }, bytes: 0, reservedBytes: 0, records: [], pending: 0, choices: [] };
  const posts = [];
  let failSave = false, failLocalSave = false;
  await page.route("**/*", async (route) => {
    const req = route.request(), url = new URL(req.url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window={};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json(providers);
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/compaction-recovery") return json(copyState);
    if (url.pathname.startsWith("/api/compaction-recovery/")) {
      const action = url.pathname.split("/").at(-1), body = req.postDataJSON();
      posts.push({ action, body });
      if (failLocalSave && action === "configure" && body.provider) {
        return route.fulfill({ status: 400, json: { error: "local setting could not be saved" } });
      }
      if (action === "configure") {
        if (body.provider) {
          copyState.config.providers = copyState.config.providers.filter((id) => id !== body.provider);
          if (body.enabled) copyState.config.providers.push(body.provider);
        }
        if (body.maxBytes) copyState.config.maxBytes = body.maxBytes;
      }
      if (action === "clear") copyState.records = [];
      if (action === "delete") copyState.records = copyState.records.filter((r) => r.id !== body.id);
      if (action === "allow") copyState.choices = [];
      return json({ ok: true });
    }
    if (url.pathname === "/api/provider/save") {
      const body = req.postDataJSON();
      posts.push({ action: "save-provider", body });
      if (failSave) return route.fulfill({ status: 400, json: { error: "provider save failed" } });
      if (body.from && body.from !== body.id && (copyState.config.providers.includes(body.from) || copyState.records.some((r) => r.provider === body.from))) {
        return route.fulfill({ status: 400, json: { error: `cannot change provider ID "${body.from}" while it has local recovery enabled or saved copies; first disable local recovery and delete its copies, or keep the ID and change only the display name` } });
      }
      const prior = providers.providers.find((p) => p.id === (body.from || body.id)) || providers.providers[0];
      let id = body.id;
      if (body.new) for (let n = 2; providers.providers.some((p) => p.id === id); n++) id = body.id + "-" + n;
      const saved = { ...prior, ...body, id, models: prior.models, key: prior.key };
      providers = { ...providers, savedID: id, providers: [...providers.providers.filter((p) => p.id !== (body.new ? id : body.from || id)), saved] };
      return json(providers);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    return route.fulfill({ body: await fs.readFile(file), contentType });
  });
  await page.goto("http://magpie.test/?view=providers");
  await page.locator(".row.provider", { hasText: "Relay" }).click();
  const adv = page.locator(".editor details.advanced");
  if (!await adv.evaluate((d) => d.open)) await adv.locator(":scope > summary").click();
  await page.locator(".recovery-copy input").waitFor();
  await page.waitForFunction(() => !document.querySelector(".recovery-copy input")?.disabled);
  return { page, posts, errors, copyState, fail: () => { failSave = true; }, failLocal: (on = true) => { failLocalSave = on; } };
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of Object.keys(words)) {
    for (const width of [1100, 420]) {
      test(`${engine} ${lang} ${width}: recovery copies save locally after provider, capacity and confirmed clear`, async (t) => {
        const { page, posts, errors } = await open(t, engine, lang, width);
        const w = words[lang], box = page.locator(".recovery-copy input");
        assert.equal(await box.isChecked(), false, "existing providers stay opted out");
        await box.check();
        assert.equal(posts.length, 0, "the draft does not save early");
        await page.locator(".editor .bar").getByRole("button", { name: w.save, exact: true }).click();
        await page.waitForFunction(() => !document.querySelector(".editor"));
        assert.deepEqual(posts.slice(0, 2).map((p) => p.action), ["save-provider", "configure"]);
        assert.equal(posts[0].body.localRecoveryEnabled, undefined, "local setting never enters provider sync");
        assert.deepEqual(posts[1].body, { provider: "relay", enabled: true });
        await page.locator(".row.provider", { hasText: "Relay" }).click();
        const adv = page.locator(".editor details.advanced");
        if (!await adv.evaluate((d) => d.open)) await adv.locator(":scope > summary").click();
        await page.waitForFunction(() => document.querySelector(".recovery-copy input")?.checked);
        await page.locator(".recovery-limit").fill("768");
        await page.locator(".recovery-tools").getByRole("button", { name: w.apply, exact: true }).click();
        await page.waitForFunction(() => document.querySelector(".recovery-limit").value === "768" && !document.querySelector(".recovery-limit").disabled);
        assert.ok(posts.some((p) => p.body.maxBytes === 768 * 1048576));
        await page.locator(".recovery-tools").getByRole("button", { name: w.clear, exact: true }).click();
        await page.locator(".action-confirm").getByRole("button", { name: w.cancel, exact: true }).click();
        assert.equal(posts.filter((p) => p.action === "clear").length, 0);
        await page.locator(".recovery-tools").getByRole("button", { name: w.clear, exact: true }).click();
        await page.locator(".action-confirm").getByRole("button", { name: w.clear, exact: true }).click();
        await page.waitForFunction(() => !document.querySelector(".recovery-limit").disabled);
        assert.deepEqual(posts.find((p) => p.action === "clear").body, { confirm: true });
        const rects = await page.locator(".compaction-recovery button, .compaction-recovery input").evaluateAll((els) => els.map((e) => {
          const r = e.getBoundingClientRect(); return { left: r.left, right: r.right };
        }));
        for (const r of rects) assert.ok(r.left >= 0 && r.right <= width, JSON.stringify(r));
        assert.equal(await page.locator(".editor select").count(), 0);
        assert.deepEqual(errors, []);
        if (process.env.ARTIFACT_DIR) {
          await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `recovery-${engine}-${lang}-${width}.png`), fullPage: true });
        }
      });
    }
  }
  test(`${engine}: failed provider creation never opts a new provider into local copies`, async (t) => {
    const { page, posts, fail, errors } = await open(t, engine, "en", 1100);
    await page.evaluate(() => duplicateProvider(providers.providers[0]));
    const adv = page.locator(".editor details.advanced");
    if (!await adv.evaluate((d) => d.open)) await adv.locator(":scope > summary").click();
    await page.waitForFunction(() => !document.querySelector(".recovery-copy input")?.disabled);
    await page.locator(".recovery-copy input").check();
    fail();
    await page.locator(".editor .bar").getByRole("button", { name: "Add", exact: true }).click();
    await page.getByText("provider save failed", { exact: true }).waitFor();
    assert.deepEqual(posts.map((p) => p.action), ["save-provider"]);
    assert.deepEqual(errors, []);
  });
  test(`${engine}: new provider creation saves before its local opt-in`, async (t) => {
    const { page, posts, errors } = await open(t, engine, "en", 1100);
    await page.evaluate(() => duplicateProvider(providers.providers[0]));
    const adv = page.locator(".editor details.advanced");
    if (!await adv.evaluate((d) => d.open)) await adv.locator(":scope > summary").click();
    await page.waitForFunction(() => !document.querySelector(".recovery-copy input")?.disabled);
    await page.locator(".recovery-copy input").check();
    await page.locator(".editor .bar").getByRole("button", { name: "Add", exact: true }).click();
    await page.waitForFunction(() => !document.querySelector(".editor"));
    assert.deepEqual(posts.slice(0, 2).map((p) => p.action), ["save-provider", "configure"]);
    assert.equal(posts[0].body.new, true);
    assert.equal(posts[0].body.localRecoveryEnabled, undefined);
    assert.equal(posts[1].body.provider, posts[0].body.id);
    assert.equal(posts[1].body.enabled, true);
    assert.deepEqual(errors, []);
  });
  test(`${engine}: a failed local save keeps the saved provider editable`, async (t) => {
    const { page, posts, failLocal, errors } = await open(t, engine, "en", 1100);
    await page.locator(".recovery-copy input").check();
    failLocal();
    await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
    await page.getByText("The provider was saved, but its local recovery setting failed: local setting could not be saved", { exact: true }).waitFor();
    assert.deepEqual(posts.slice(0, 2).map((p) => p.action), ["save-provider", "configure"]);
    assert.equal(await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).isEnabled(), true);
    assert.equal(await page.evaluate(() => editing), "relay");
    assert.deepEqual(errors, []);
  });
  test(`${engine}: renaming an ID with recovery state is rejected without migrating its opt-in`, async (t) => {
    const { page, posts, copyState, errors } = await open(t, engine, "en", 1100);
    copyState.config.providers = ["relay"];
    await page.locator(".recovery-tools").getByRole("button", { name: "Refresh", exact: true }).click();
    await page.waitForFunction(() => document.querySelector(".recovery-copy input")?.checked);
    await page.evaluate(() => { draft.id = "renamed-relay"; });
    await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
    await page.getByText('cannot change provider ID "relay" while it has local recovery enabled or saved copies; first disable local recovery and delete its copies, or keep the ID and change only the display name', { exact: true }).waitFor();
    assert.equal(await page.evaluate(() => editing), "relay");
    assert.equal(await page.locator(".recovery-copy input").isChecked(), true);
    assert.deepEqual(posts.map((p) => p.action), ["save-provider"]);
    assert.deepEqual(copyState.config.providers, ["relay"]);
    assert.deepEqual(errors, []);
  });
  test(`${engine}: an ID collision configures the added provider and retries its failed local save`, async (t) => {
    const { page, posts, copyState, failLocal, errors } = await open(t, engine, "en", 1100);
    await page.evaluate(() => { duplicateProvider(providers.providers[0]); draft.id = "relay"; });
    const adv = page.locator(".editor details.advanced");
    if (!await adv.evaluate((d) => d.open)) await adv.locator(":scope > summary").click();
    await page.waitForFunction(() => !document.querySelector(".recovery-copy input")?.disabled);
    await page.locator(".recovery-copy input").check();
    failLocal();
    await page.locator(".editor .bar").getByRole("button", { name: "Add", exact: true }).click();
    await page.getByText("The provider was saved, but its local recovery setting failed: local setting could not be saved", { exact: true }).waitFor();
    assert.equal(await page.evaluate(() => editing), "relay-2");
    assert.deepEqual(posts.slice(0, 2).map((p) => p.action), ["save-provider", "configure"]);
    assert.equal(posts[0].body.id, "relay");
    assert.deepEqual(posts[1].body, { provider: "relay-2", enabled: true });
    failLocal(false);
    await page.locator(".editor .bar").getByRole("button", { name: "Save", exact: true }).click();
    await page.waitForFunction(() => !document.querySelector(".editor"));
    assert.deepEqual(copyState.config.providers, ["relay-2"]);
    assert.equal(posts.filter((p) => p.action === "save-provider").length, 2);
    assert.equal(posts.filter((p) => p.action === "save-provider")[1].body.new, false);
    assert.equal(await page.locator(".row.provider").count(), 2);
    assert.deepEqual(errors, []);
  });
  test(`${engine}: a request-only choice requires confirmation and says how to retry`, async (t) => {
    const { page, posts, copyState, errors } = await open(t, engine, "en", 420);
    copyState.choices = [{ id: "request-token", branch: "request", model: "relay/m", reason: "storage_full", actions: ["native-only"] }];
    await page.locator(".recovery-tools").getByRole("button", { name: "Refresh", exact: true }).click();
    await page.getByText("This request has no reliable branch identity. Retry with X-Magpie-Recovery-Token: request-token after allowing it.", { exact: true }).waitFor();
    const allow = page.getByRole("button", { name: "Allow native compaction once without a copy", exact: true });
    await allow.click();
    assert.equal(posts.filter((p) => p.action === "allow").length, 0);
    await page.locator(".action-confirm").getByRole("button", { name: "Allow once", exact: true }).click();
    await page.getByText("Allowed once. Retry this request in the client.", { exact: true }).waitFor();
    assert.deepEqual(posts.find((p) => p.action === "allow").body, { id: "request-token", choice: "native-only" });
    assert.deepEqual(errors, []);
  });
  test(`${engine}: recent call details distinguish every recovery outcome`, async (t) => {
    const { page, errors } = await open(t, engine, "en", 1100);
    const states = ["native-backed", "native-only", "restored", "replayed-this-request", "excerpt", "unavailable"];
    await page.evaluate((states) => {
      providers.gateway.calls = states.map((recovery, n) => ({
        id: n + 1, recovery, time: new Date().toISOString(), model: "relay/m", from: "responses", to: "responses", status: 200, ms: 1,
      }));
      for (const c of providers.gateway.calls) {
        const id = callRow(c);
        expandedCalls.add(id);
        callBodies.set(id, { requestBody: "{}", responseBody: "{}" });
      }
      renderActivity();
    }, states);
    assert.deepEqual(await page.locator(".call-recovery").allTextContents(), [
      "Native compaction, recovery copy available", "Native compaction, no local recovery copy",
      "Restored from local history", "Full history replayed for this request; later requests are assessed again",
      "Local excerpt, some history left out", "Recovery copy missing or no longer available",
    ]);
    assert.deepEqual(errors, []);
  });
  test(`${engine}: only the selected recovery copy is deleted after confirmation`, async (t) => {
    const { page, posts, copyState, errors } = await open(t, engine, "en", 420);
    copyState.records = [
      { id: "copy-a", provider: "relay", model: "m", bytes: 123 },
      { id: "copy-b", provider: "relay", model: "m", bytes: 456 },
    ];
    await page.locator(".recovery-tools").getByRole("button", { name: "Refresh", exact: true }).click();
    const remove = page.getByRole("button", { name: "Delete recovery copy copy-a", exact: true });
    await remove.click();
    assert.equal(posts.filter((p) => p.action === "delete").length, 0);
    await page.locator(".action-confirm").getByRole("button", { name: "Delete", exact: true }).click();
    await remove.waitFor({ state: "detached" });
    assert.deepEqual(posts.find((p) => p.action === "delete").body, { id: "copy-a", confirm: true });
    assert.equal(await page.getByRole("button", { name: "Delete recovery copy copy-b", exact: true }).isVisible(), true);
    assert.deepEqual(errors, []);
  });
}
