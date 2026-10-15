// Run with Node's test runner and Playwright on the module path; see README.md.
// These requests stay in the editor's draft: no save and no automatic opt-in.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_COMPACTION_ASSETS || path.resolve(__dirname, "../assets");
const relay = {
  id: "relay", name: "Relay", icon: "generic", chat: "", responses: "https://relay.example.com/v1", anthropic: "", catalog: "",
  models: [{ id: "gpt-6", name: "GPT-6", on: true }], agents: [], fallback: [], headers: {},
  key: { set: true, masked: "sk-…one" }, keyList: [], balanceToken: { takes: false, set: false }, proxy: "", nativeCompaction: false,
};
const chatOnly = { ...relay, id: "chatonly", name: "Chat Only", chat: "https://chat.example.com/v1", responses: "" };
const codex = {
  id: "codex", name: "Codex", icon: "codex-color", chat: "", responses: "", anthropic: "", catalog: "",
  models: [{ id: "gpt-6", name: "GPT-6", on: true }], agents: [], fallback: [], headers: {}, keyList: [],
  account: { agent: "codex", agentName: "Codex", user: "me@example.com", plan: "PLUS", logins: [{ user: "me@example.com", plan: "PLUS", active: true, on: true }] },
  carriesCompaction: true, nativeCompaction: true, proxy: "",
};

function serve(lang, posts, compaction) {
  const providers = { providers: [relay, chatOnly, codex], presets: [{ id: "openai", name: "OpenAI", kind: "vendor", icon: "openai", chat: "https://api.openai.example/v1", responses: "https://api.openai.example/v1" }], excluded: [], gateway: { running: true, window: true } };
  const state = { agents: [], profiles: [], settings: { lang, theme: "light" } };
  return async (route) => {
    const url = new URL(route.request().url());
    const json = (data) => route.fulfill({ json: data });
    if (url.pathname === "/boot.js") return route.fulfill({ contentType: "text/javascript", body: `window.bootPrefs = {lang:"${lang}",theme:"light",web:true};` });
    if (url.pathname === "/wails/runtime.js") return route.fulfill({ contentType: "text/javascript", body: "export const Window = {};" });
    if (url.pathname === "/api/state") return json(state);
    if (url.pathname === "/api/providers") return json(providers);
    if (url.pathname === "/api/groups") return json({ groups: [] });
    if (url.pathname.startsWith("/api/provider/") && route.request().method() === "POST") {
      posts.push({ action: url.pathname.slice("/api/provider/".length), body: route.request().postDataJSON() });
      if (url.pathname.endsWith("/test-compaction")) return compaction(route);
      return json(providers);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

const words = {
  en: { more: "More endpoints", responses: "Responses URL", check: "Check compaction", ok: "Native compaction confirmed", unknown: "Not confirmed: no encrypted compaction returned", failed: "Compaction request failed" },
  zh: { more: "更多端点", responses: "Responses 地址", check: "检测", ok: "已确认原生压缩", unknown: "未确认：没有返回加密压缩内容", failed: "压缩请求失败" },
  "zh-TW": { more: "更多端點", responses: "Responses 位址", check: "檢測", ok: "已確認原生壓縮", unknown: "未確認：未傳回加密壓縮內容", failed: "壓縮請求失敗" },
  ja: { more: "その他のエンドポイント", responses: "Responses URL", check: "圧縮を確認", ok: "ネイティブ圧縮を確認", unknown: "未確認：暗号化された圧縮結果が返されませんでした", failed: "圧縮リクエストに失敗" },
  de: { more: "Weitere Endpunkte", responses: "Responses-URL", check: "Kompaktierung prüfen", ok: "Native Kompaktierung bestätigt", unknown: "Unbestätigt: keine verschlüsselte Kompaktierung erhalten", failed: "Kompaktierungsanfrage fehlgeschlagen" },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of Object.keys(words)) {
    const w = words[lang];
    const open = async (t, name, compaction = (route) => route.fulfill({ json: { results: [] } })) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      const page = await (await browser.newContext({ viewport: { width: 900, height: 760 }, reducedMotion: "reduce" })).newPage();
      t.after(async () => {
        if (process.env.ARTIFACT_DIR) {
          await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-${name}-compaction.png`), fullPage: true });
        }
        await browser.close();
      });
      page.setDefaultTimeout(5000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      const posts = [];
      await page.route("**/*", serve(lang, posts, compaction));
      await page.goto("http://magpie.test/?view=providers");
      if (name === "OpenAI") {
        await page.locator("#addProvider").click();
        await page.locator('#addSheet .tile[data-pick="OpenAI"]').click();
      } else await page.locator(".row.provider", { hasText: name }).click();
      const advanced = page.locator(".editor details.advanced");
      await advanced.waitFor({ state: "attached" });
      if (!(await advanced.evaluate((d) => d.open))) await advanced.locator(":scope > summary").click();
      await page.locator(".editor .bar").waitFor();
      return { page, errors, posts };
    };
    const box = (page) => page.locator(".editor label.native-compaction input");

    test(`${engine} ${lang}: a new preset checks its effective Responses endpoint`, async (t) => {
      const { page, errors, posts } = await open(t, "OpenAI");
      const button = page.locator(".compaction-test").getByRole("button", { name: w.check, exact: true });
      await button.click();
      await page.waitForFunction(() => !document.querySelector(".compaction-test button").disabled);
      assert.equal(posts.length, 1);
      assert.equal(posts[0].action, "test-compaction");
      assert.equal(posts[0].body.responses, "https://api.openai.example/v1", "the preset's default is the address being checked");
      assert.equal(posts[0].body.id, undefined, "a new preset is never mistaken for an existing provider");
      assert.equal(await box(page).isChecked(), false);
      await page.evaluate(() => { draft.responses = ""; });
      await button.click();
      await page.waitForFunction(() => !document.querySelector(".compaction-test button").disabled);
      assert.equal(posts[1].body.responses, "", "an explicitly cleared endpoint never falls back to the preset");
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: compaction detection follows the draft Responses URL`, async (t) => {
      const { page, errors, posts } = await open(t, "Chat Only");
      const control = page.locator(".compaction-test");
      assert.equal(await control.isVisible(), false, "no detection without a Responses endpoint");
      await page.locator(".editor details.more summary", { hasText: w.more }).click();
      const url = page.locator(".editor details.more label", { hasText: w.responses }).locator("xpath=following-sibling::div[1]//input");
      await url.fill("https://relay.example.com/v1");
      assert.equal(await control.isVisible(), true, "detection appears with a Responses endpoint");
      assert.equal(await control.getByRole("button", { name: w.check, exact: true }).count(), 1);
      await url.fill("");
      assert.equal(await control.isVisible(), false, "detection goes away when the URL is cleared");
      assert.deepEqual(posts, [], "editing never makes a detection request");
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: a subscription keeps its compaction option without detection`, async (t) => {
      const { page, errors, posts } = await open(t, "Codex");
      assert.equal(await box(page).isChecked(), true);
      assert.equal(await page.locator(".compaction-test").count(), 0, "API-key detection is not offered for subscriptions");
      assert.deepEqual(posts, []);
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: native compaction checks the draft once and never saves or opts in`, async (t) => {
      let release;
      let started;
      const pending = new Promise((resolve) => { started = resolve; });
      let calls = 0;
      const { page, errors, posts } = await open(t, "Relay", async (route) => {
        calls++;
        if (calls === 1) {
          await new Promise((resolve) => { release = resolve; started(); });
          return route.fulfill({ json: { results: [{ protocol: "responses", model: "gpt-draft", ok: true }] } });
        }
        if (calls === 2) return route.fulfill({ json: { results: [{ protocol: "responses", model: "gpt-draft", ok: false }] } });
        if (calls === 3) return route.fulfill({ json: { results: [{ protocol: "responses", model: "gpt-draft", ok: false, status: 400, error: "compaction unsupported" }] } });
        return route.fulfill({ status: 400, json: { error: "probe failed" } });
      });
      await page.setViewportSize({ width: 440, height: 760 });
      await page.evaluate(() => {
        draft.key = "sk-draft";
        draft.responses = "https://draft.example.com/v1";
        draft.headers = [["X-Relay", "draft", false]];
        draft.extraBody = '{"relay_field":"draft"}';
        draft.chosen = ["gpt-draft"];
      });
      const button = page.locator(".compaction-test").getByRole("button", { name: w.check, exact: true });
      assert.equal(await button.count(), 1, "native compaction has a detection button beside its checkbox");
      await button.click();
      await pending;
      await page.waitForFunction(() => document.querySelector(".compaction-test button").disabled);
      await button.dispatchEvent("click");
      assert.equal(posts.length, 1, "a pending detection cannot be repeated");
      assert.equal(posts[0].action, "test-compaction");
      assert.equal(posts[0].body.id, "relay");
      assert.equal(posts[0].body.typed, true);
      assert.equal(posts[0].body.key, "sk-draft");
      assert.equal(posts[0].body.responses, "https://draft.example.com/v1");
      assert.deepEqual(posts[0].body.headers, { "X-Relay": "draft" });
      assert.deepEqual(posts[0].body.picked, ["gpt-draft"]);
      assert.deepEqual(posts[0].body.extraBody, { relay_field: "draft" });
      release();
      const status = page.locator(".compaction-test [role=status]");
      await status.getByText(w.ok, { exact: false }).waitFor();
      assert.equal(await box(page).isChecked(), false, "confirmation never turns the option on");
      if (process.env.ARTIFACT_DIR) {
        await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-compaction-success.png`), fullPage: true });
      }
      await button.click();
      await status.getByText(w.unknown, { exact: false }).waitFor();
      await button.click();
      await status.getByText(w.failed, { exact: false }).waitFor();
      assert.match(await status.textContent(), /400.*compaction unsupported/);
      await button.click();
      await status.getByText("probe failed", { exact: true }).waitFor();
      assert.equal(await button.isEnabled(), true, "a failed request can be retried");
      assert.equal(posts.every((p) => p.action === "test-compaction"), true, "no save or refresh is sent");
      await page.evaluate(() => { draft.extraBody = "{"; });
      await button.click();
      assert.equal(posts.length, 4, "invalid extra JSON never reaches the provider");
      const layout = await page.locator(".compaction-test").evaluate((el) => {
        const a = el.querySelector("label").getBoundingClientRect();
        const b = el.querySelector("button").getBoundingClientRect();
        const root = document.documentElement;
        return { overlap: a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom, overflow: root.scrollWidth > root.clientWidth };
      });
      assert.deepEqual(layout, { overlap: false, overflow: false });
      assert.equal(await box(page).isChecked(), false);
      assert.deepEqual(errors, []);
    });
  }
}
