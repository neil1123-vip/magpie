// A source save keeps its original editor through account-model writes.
// Late successes and failures leave the next editor and picker alone.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = path.resolve(__dirname, "../assets");
const words = {
  en: { save: "Save", cancel: "Cancel", discard: "Discard" },
  zh: { save: "保存", cancel: "取消", discard: "放弃" },
  "zh-TW": { save: "儲存", cancel: "取消", discard: "放棄" },
  ja: { save: "保存", cancel: "キャンセル", discard: "破棄" },
  de: { save: "Speichern", cancel: "Abbrechen", discard: "Verwerfen" },
};
const key = (id) => ({ id, name: id, masked: "sk-" + id, on: true });
function fixture() {
  const common = {
    chat: "", responses: "", anthropic: "", catalog: "", agents: [], fallback: [], headers: {}, proxy: "",
    models: [{ id: "model-one", on: true }, { id: "model-two", on: true }],
    saveRevision: "saved-revision",
  };
  const codex = {
    ...common, id: "codex", name: "Codex", keyList: [],
    account: { agent: "codex", agentName: "Codex", user: "main@example.com",
      logins: [{ user: "main@example.com", active: true, on: true }, { user: "spare@example.com", on: true }] },
  };
  const relay = {
    ...common, id: "relay", name: "Relay", chat: "https://relay.example/v1",
    key: { set: true, masked: "sk-first" }, keyList: [key("aaaaaaaaaa"), key("bbbbbbbbbb")],
  };
  const other = { ...relay, id: "other", name: "Other", keyList: [key("cccccccccc"), key("dddddddddd")] };
  return { providers: [codex, relay, other], presets: [], excluded: [], gateway: { running: true, window: true } };
}
async function open(browser, lang, width, source, outcome) {
  const context = await browser.newContext({ viewport: { width, height: 760 }, reducedMotion: "reduce" });
  const page = await context.newPage();
  page.setDefaultTimeout(5000);
  page.setDefaultNavigationTimeout(60000);
  const payload = fixture(), saves = [], errors = [];
  let release, started, conflict = outcome === "conflict";
  const pending = new Promise((resolve) => { started = resolve; });
  page.on("pageerror", (e) => errors.push(e.message));
  await page.route("**/*", async (route) => {
    const url = new URL(route.request().url());
    const json = (data, status = 200) => route.fulfill({ status, json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs={lang:${JSON.stringify(lang)},theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json({ agents: [], profiles: [], settings: { lang, theme: "light" } });
    if (url.pathname === "/api/providers") return json(payload);
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname === "/api/provider/accountmodels") {
      started();
      await new Promise((resolve) => { release = resolve; });
      return outcome === "preflight error" ? json({ error: "old model write failed" }, 502) : json(payload);
    }
    if (url.pathname === "/api/provider/save") {
      const body = route.request().postDataJSON();
      saves.push(body);
      if (conflict || body.saveRevision !== "saved-revision") return json({ error: "the provider configuration changed; reopen it before saving" }, 409);
      return json(payload);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    try {
      return await route.fulfill({ body: await fs.readFile(file), contentType: {
        ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png",
      }[path.extname(file)] });
    } catch { return route.fulfill({ status: 404, body: "" }); }
  });
  await page.goto("http://magpie.test/?view=providers");
  await page.locator(`.row.provider[data-id="${source}"]`).click();
  // Wait on the complete save, including its UI work, rather than a response.
  await page.evaluate(() => {
    const action = providerAction;
    providerAction = (...args) => { const result = action(...args); if (args[0] === "save") window.completedSave = result; return result; };
  });
  return { context, page, payload, saves, errors, pending, release: () => release?.(), conflict: () => { conflict = true; } };
}
const editorSave = (page, w) => page.locator(".editor .bar").getByRole("button", { name: w.save, exact: true });
async function pick(page, ref) {
  const row = page.locator(`.editor .accts .acc[data-account-id="${ref}"]`);
  await row.hover();
  await row.locator(".amodels").evaluate((button) => button.click());
  await row.locator(".acct-models .mchip", { hasText: /^model-one$/ }).click();
}

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  test(`${engine}: source saves keep their editor and revision`, async (t) => {
    const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
    t.after(() => browser.close());
    for (const lang of Object.keys(words)) for (const width of [440, 900]) {
      const w = words[lang];
      await t.test(`${lang} ${width}: subscription revision and conflict`, async () => {
        const f = await open(browser, lang, width, "codex", "success");
        try {
          await editorSave(f.page, w).click();
          await f.page.evaluate(() => window.completedSave);
          assert.equal(f.saves[0].saveRevision, "saved-revision");
          assert.equal(await f.page.evaluate(() => editing), null, "the saved editor has released its draft");
          // closeModal removes the old DOM after its exit animation.
          await f.page.locator(".editor").waitFor({ state: "detached" });
          assert.equal(await f.page.locator(".editor").count(), 0);
          await f.page.locator('.row.provider[data-id="codex"]').click();
          f.conflict();
          await editorSave(f.page, w).click();
          await f.page.evaluate(() => window.completedSave);
          assert.equal(await f.page.evaluate(() => draft.saveRevisionConflict), true);
          assert.equal(await editorSave(f.page, w).isDisabled(), true);
          assert.deepEqual(f.errors, []);
        } finally { await f.context.close(); }
      });
      for (const source of ["codex", "relay"]) for (const outcome of ["success", "preflight error", "conflict"]) {
        await t.test(`${lang} ${width}: late ${source} ${outcome}`, async () => {
          const f = await open(browser, lang, width, source, outcome);
          try {
            await pick(f.page, source === "codex" ? "spare@example.com" : "bbbbbbbbbb");
            await editorSave(f.page, w).click();
            await f.pending;
            await f.page.locator(".editor .bar").getByRole("button", { name: w.cancel, exact: true }).click();
            await f.page.locator("dialog.action-confirm[open]").getByRole("button", { name: w.discard, exact: true }).click();
            await f.page.locator(".editor").waitFor({ state: "detached" });
            await f.page.locator('.row.provider[data-id="other"]').click();
            await pick(f.page, "dddddddddd");
            await f.page.evaluate(() => { draft.name = "Unsaved Other"; });
            f.release();
            await f.page.evaluate(() => window.completedSave);
            assert.deepEqual(await f.page.evaluate(() => ({
              editing, name: draft?.name, conflict: !!draft?.saveRevisionConflict,
              picker: acctModels && { id: acctModels.id, ref: acctModels.ref, chosen: acctModels.chosen },
            })), {
              editing: "other", name: "Unsaved Other", conflict: false,
              picker: { id: "other", ref: "dddddddddd", chosen: ["model-one"] },
            });
            assert.equal(await f.page.locator(".editor .editor-error").count(), 0);
            assert.equal(await editorSave(f.page, w).isEnabled(), true);
            assert.equal(f.saves.length, outcome === "preflight error" ? 0 : 1);
            assert.deepEqual(f.errors, []);
          } finally { f.release(); await f.context.close(); }
        });
      }
    }
  });
}
