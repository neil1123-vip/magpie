// Run with Node's test runner and Playwright on the module path; see README.md.
// A custom provider's "Searches the web by itself" (#359): a relay in front
// of Anthropic's or OpenAI's own API, whose web search magpie can't tell
// from its host. The editor has a Web search row with it, opening as it was
// saved; Save posts it, ticked or not. A relay with only a Chat address has
// no API to search on: the row comes once an Anthropic one is typed. A
// signed-in account (Codex here) has no such row: magpie knows how it
// searches. Native-search detection tests the draft without saving it.
const assert = require("node:assert/strict");
const fs = require("node:fs/promises");
const path = require("node:path");
const { test } = require("node:test");
const { chromium, webkit } = require("playwright");

const assets = process.env.MAGPIE_SEARCH_ASSETS || path.resolve(__dirname, "../assets");
// the editor folds what few need under Advanced, this field among them:
// opened as a reader would, unless something set in it opened it already
const unfold = async (page) => {
  const adv = page.locator(".editor details.advanced");
  await adv.waitFor({ state: "attached" });
  if (!(await adv.evaluate((d) => d.open))) await adv.locator(":scope > summary").click();
};
const relay = {
  id: "relay", name: "Relay", icon: "generic", chat: "", responses: "", anthropic: "https://relay.example.com", catalog: "",
  models: [{ id: "claude-haiku-4-5", name: "Claude Haiku 4.5", on: true }], agents: [], fallback: [], headers: {},
  key: { set: true, masked: "sk-…one" }, keyList: [], balanceToken: { takes: false, set: false }, proxy: "", searches: false,
};
const searching = { ...relay, id: "searching", name: "Searching", searches: true };
const chatOnly = { ...relay, id: "chatonly", name: "Chat Only", chat: "https://chat.example.com/v1", anthropic: "" };
const keys = { ...relay, id: "keys", name: "Keys", keyList: [{ id: "aaaaaaaaaa", name: "Team", masked: "sk-…one", on: true, active: true }, { id: "bbbbbbbbbb", name: "Spare", masked: "sk-…two", on: true }], accountProxies: { bbbbbbbbbb: "direct" } };
const codex = {
  id: "codex", name: "Codex", icon: "codex-color", chat: "", responses: "", anthropic: "", catalog: "",
  models: [{ id: "gpt-6", name: "GPT-6", on: true }], agents: [], fallback: [], headers: {}, keyList: [],
  account: { agent: "codex", agentName: "Codex", user: "me@example.com", plan: "PLUS", logins: [{ user: "me@example.com", plan: "PLUS", active: true, on: true }] },
  proxy: "",
};

function serve(lang, posts, search) {
  const providers = { providers: [relay, searching, chatOnly, keys, codex], presets: [], excluded: [], gateway: { running: true, window: true } };
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
      if (url.pathname.endsWith("/test-search")) return search(route);
      return json(providers);
    }
    if (url.pathname.startsWith("/api/")) return json({});
    const file = path.join(assets, url.pathname === "/" ? "index.html" : url.pathname);
    const contentType = { ".html": "text/html", ".js": "text/javascript", ".css": "text/css", ".svg": "image/svg+xml", ".png": "image/png" }[path.extname(file)];
    await route.fulfill({ body: await fs.readFile(file), contentType });
  };
}

const words = {
  en: { tick: "Searches the web by itself", save: "Save", more: "More endpoints", anthropic: "Anthropic URL", check: "Check search", ok: "Native search confirmed", unknown: "Not confirmed: no native search result returned", failed: "Search request failed" },
  zh: { tick: "自己能联网搜索", save: "保存", more: "更多端点", anthropic: "Anthropic 地址", check: "检测", ok: "已确认原生搜索", unknown: "未确认：没有返回原生搜索结果", failed: "搜索请求失败" },
  "zh-TW": { tick: "自己能聯網搜尋", save: "儲存", more: "更多端點", anthropic: "Anthropic 位址", check: "檢測", ok: "已確認原生搜尋", unknown: "未確認：未傳回原生搜尋結果", failed: "搜尋請求失敗" },
  ja: { tick: "自分でウェブ検索する", save: "保存", more: "その他のエンドポイント", anthropic: "Anthropic URL", check: "検索を確認", ok: "ネイティブ検索を確認", unknown: "未確認：ネイティブ検索結果が返されませんでした", failed: "検索リクエストに失敗" },
  de: { tick: "Sucht selbst im Web", save: "Speichern", more: "Weitere Endpunkte", anthropic: "Anthropic-URL", check: "Suche prüfen", ok: "Native Suche bestätigt", unknown: "Unbestätigt: kein natives Suchergebnis erhalten", failed: "Suchanfrage fehlgeschlagen" },
};

for (const engine of (process.env.BROWSER ? [process.env.BROWSER] : ["chromium", "webkit"])) {
  for (const lang of Object.keys(words)) {
    const w = words[lang];
    const open = async (t, name, search = (route) => route.fulfill({ json: { results: [] } })) => {
      const browser = await (engine === "webkit" ? webkit.launch() : chromium.launch({ channel: "chromium" }));
      t.after(async () => {
        if (process.env.ARTIFACT_DIR) {
          await fs.mkdir(process.env.ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ARTIFACT_DIR, `${engine}-${lang}-${name}-search.png`), fullPage: true });
        }
        await browser.close();
      });
      const page = await (await browser.newContext({ viewport: { width: 900, height: 760 }, reducedMotion: "reduce" })).newPage();
      page.setDefaultTimeout(5000);
      const errors = [];
      page.on("pageerror", (e) => errors.push(e.message));
      const posts = [];
      await page.route("**/*", serve(lang, posts, search));
      await page.goto("http://magpie.test/?view=providers");
      await page.locator(".row.provider", { hasText: name }).click();
      await unfold(page);
      await page.locator(".editor .bar").waitFor();
      return { page, errors, posts };
    };
    const box = (page) => page.locator(".editor label.tick", { hasText: w.tick }).locator("input");
    const save = async (page, posts) => {
      const n = posts.length;
      await page.locator(".editor .bar").getByRole("button", { name: w.save, exact: true }).click();
      for (let i = 0; i < 50 && posts.length === n; i++) await page.waitForTimeout(50);
      return posts.at(-1);
    };

    test(`${engine} ${lang}: a relay is said to search by itself, and saved so`, async (t) => {
      const { page, errors, posts } = await open(t, "Relay");
      assert.equal(await box(page).isChecked(), false, "a relay opens as not searching");
      await box(page).scrollIntoViewIfNeeded();
      await box(page).check();
      const saved = await save(page, posts);
      assert.equal(saved.action, "save");
      assert.equal(saved.body.id, "relay");
      assert.equal(saved.body.searches, true);
      const missing = await page.evaluate(() => [
        "Web search", "Searches the web by itself",
        "For a relay in front of Anthropic's or OpenAI's own API: Claude Code's WebSearch and Codex's web_search go to it as they were sent, not through magpie's search. magpie doesn't automatically search with it for other models, but you can name it as the searcher.",
      ].filter((k) => !I18N.zh[k]));
      assert.deepEqual(missing, [], "every string has its Chinese");
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: one saved as searching opens ticked, and is saved unticked`, async (t) => {
      const { page, errors, posts } = await open(t, "Searching");
      assert.equal(await box(page).isChecked(), true);
      await box(page).scrollIntoViewIfNeeded();
      await box(page).uncheck();
      const saved = await save(page, posts);
      assert.equal(saved.body.id, "searching");
      assert.equal(saved.body.searches, false);
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: a Chat-only relay has the row once it has an Anthropic URL`, async (t) => {
      const { page, errors, posts } = await open(t, "Chat Only");
      const tick = page.locator(".editor label.tick", { hasText: w.tick });
      assert.equal(await tick.isVisible(), false, "no row with only a Chat address");
      await page.locator(".editor details.more summary", { hasText: w.more }).click();
      const url = page.locator(".editor details.more label", { hasText: w.anthropic }).locator("xpath=following-sibling::div[1]//input");
      await url.fill("https://relay.example.com");
      assert.equal(await tick.isVisible(), true, "the row comes with an Anthropic address");
      await box(page).scrollIntoViewIfNeeded();
      await box(page).check();
      await url.fill("");
      assert.equal(await tick.isVisible(), false, "and goes with it");
      const saved = await save(page, posts);
      assert.equal(saved.body.searches, false, "not saved as searching with nothing to search on");
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: a signed-in account has no such row`, async (t) => {
      const { page, errors } = await open(t, "Codex");
      assert.equal(await page.locator(".editor label.tick", { hasText: w.tick }).count(), 0);
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: native checks validate and send the current provider and key proxies`, async (t) => {
      const { page, errors, posts } = await open(t, "Keys");
      const button = page.locator(".search-test").getByRole("button", { name: w.check, exact: true });
      const status = page.locator(".search-test [role=status]");
      const custom = await page.evaluate(() => t("Custom"));
      const follows = await page.evaluate(() => t("Provider's proxy"));
      await page.locator(".editor .proxy-pick").getByRole("button", { name: custom, exact: true }).click();
      await button.click();
      assert.equal(posts.length, 0, "a missing provider proxy cannot fall back to the saved proxy");
      await status.getByText(await page.evaluate(() => t("Proxy: type its address, like http://127.0.0.1:7890")), { exact: true }).waitFor();
      await page.locator(".editor .proxy-pick .proxy-url").fill("http://main.invalid:7890");
      const own = page.locator('.editor .acct-proxy[data-key="aaaaaaaaaa"]');
      await own.getByRole("button", { name: custom, exact: true }).click();
      await button.click();
      assert.equal(posts.length, 0, "a missing key proxy cannot fall back to its old proxy");
      await status.getByText(await page.evaluate(() => t("Proxy of {user}: type its address, like http://127.0.0.1:7890", { user: "Team" })), { exact: true }).waitFor();
      await own.locator(".proxy-url").fill("http://key.invalid:7891");
      await button.click();
      await page.waitForFunction(() => !document.querySelector(".search-test button").disabled);
      assert.equal(posts[0].body.proxy, "http://main.invalid:7890");
      assert.deepEqual(posts[0].body.accountProxies, { aaaaaaaaaa: "http://key.invalid:7891", bbbbbbbbbb: "direct" });
      await own.getByRole("button", { name: follows, exact: true }).click();
      await button.click();
      await page.waitForFunction(() => !document.querySelector(".search-test button").disabled);
      assert.deepEqual(posts[1].body.accountProxies, { bbbbbbbbbb: "direct" }, "following the provider clears a previously typed key proxy");
      assert.equal(posts.every((p) => p.action === "test-search"), true);
      assert.deepEqual(errors, []);
    });

    test(`${engine} ${lang}: native search checks the draft once, reports evidence and never saves`, async (t) => {
      let release;
      let started;
      const pending = new Promise((resolve) => { started = resolve; });
      let calls = 0;
      const { page, errors, posts } = await open(t, "Relay", async (route) => {
        calls++;
        if (calls === 1) {
          await new Promise((resolve) => { release = resolve; started(); });
          return route.fulfill({ json: { results: [{ protocol: "anthropic", model: "claude-draft", ok: true }] } });
        }
        if (calls === 2) return route.fulfill({ json: { results: [{ protocol: "anthropic", model: "claude-draft", ok: false }] } });
        if (calls === 3) return route.fulfill({ json: { results: [{ protocol: "anthropic", model: "claude-draft", ok: false, status: 400, error: "tool unsupported" }] } });
        return route.fulfill({ status: 400, json: { error: "probe failed" } });
      });
      await page.setViewportSize({ width: 440, height: 760 });
      await page.evaluate(() => {
        draft.key = "sk-draft";
        draft.anthropic = "https://draft.example.com";
        draft.headers = [["X-Relay", "draft", false]];
        draft.authHeader = "bearer";
        draft.extraBody = '{"relay_field":"draft"}';
        draft.chosen = ["claude-draft"];
      });
      const button = page.locator(".search-test").getByRole("button", { name: w.check, exact: true });
      assert.equal(await button.count(), 1, "native search has a detection button beside its checkbox");
      await button.click();
      await pending;
      await page.waitForFunction(() => document.querySelector(".search-test button").disabled);
      await button.dispatchEvent("click");
      assert.equal(posts.length, 1, "a pending detection cannot be repeated");
      assert.equal(posts[0].action, "test-search");
      assert.equal(posts[0].body.id, "relay");
      assert.equal(posts[0].body.key, "sk-draft");
      assert.equal(posts[0].body.anthropic, "https://draft.example.com");
      assert.equal(posts[0].body.authHeader, "bearer");
      assert.deepEqual(posts[0].body.headers, { "X-Relay": "draft" });
      assert.deepEqual(posts[0].body.picked, ["claude-draft"]);
      assert.deepEqual(posts[0].body.extraBody, { relay_field: "draft" });
      release();
      const status = page.locator(".search-test [role=status]");
      await status.getByText(w.ok, { exact: false }).waitFor();
      assert.equal(await box(page).isChecked(), false, "confirmation never turns the option on");
      await button.click();
      await status.getByText(w.unknown, { exact: false }).waitFor();
      await button.click();
      await status.getByText(w.failed, { exact: false }).waitFor();
      assert.match(await status.textContent(), /400.*tool unsupported/);
      await button.click();
      await status.getByText("probe failed", { exact: true }).waitFor();
      assert.equal(await button.isEnabled(), true, "a failed request can be retried");
      assert.equal(posts.every((p) => p.action === "test-search"), true, "no save or refresh is sent");
      await page.evaluate(() => { draft.extraBody = "{"; });
      await button.click();
      assert.equal(posts.length, 4, "invalid extra JSON never reaches the provider");
      const layout = await page.locator(".search-test").evaluate((el) => {
        const a = el.querySelector("label").getBoundingClientRect();
        const b = el.querySelector("button").getBoundingClientRect();
        const root = document.documentElement;
        return { overlap: a.left < b.right && b.left < a.right && a.top < b.bottom && b.top < a.bottom, overflow: root.scrollWidth > root.clientWidth };
      });
      assert.deepEqual(layout, { overlap: false, overflow: false });
      assert.deepEqual(errors, []);
    });
  }
}
