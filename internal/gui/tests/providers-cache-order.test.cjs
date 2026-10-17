// The cached Providers list is a placeholder, never the result of a fresh
// read. Run with Node alone; the real api and loadProvidersNow are driven by
// explicitly released network replies so both arrival orders are covered.
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

const source = fs.readFileSync(process.env.MAGPIE_APP_JS || path.resolve(__dirname, "../assets/app.js"), "utf8").replace(/\r\n/g, "\n");
function lift(header) {
  const start = source.indexOf(header), end = source.indexOf("\n}\n", start);
  assert(start >= 0 && end > start, `app.js no longer has ${header}`);
  return source.slice(start, end + 2);
}
const ordering = source.match(/^let providerFlight = .*$/m);
assert(ordering, "app.js no longer has its provider read state");
const code = [ordering[0], lift("async function api("), lift("async function loadProvidersNow()")].join("\n");
const turn = () => new Promise((resolve) => setImmediate(resolve));
const fresh = () => ({ providers: [{ id: "relay", name: "Fresh Relay", headers: { "X-Team": "t1" } }], gateway: {} });
const cached = () => ({ providers: [{ id: "relay", name: "Kept Relay" }], gateway: {}, last: true });

function page() {
  const requests = [];
  const context = vm.createContext({
    providers: null, providersEarly: null, view: "providers", editing: null, adding: false,
    apiRequest(requestPath) {
      return new Promise((resolve) => requests.push({ path: requestPath, resolve }));
    },
    renderSideGw() {}, providersPage() { return false; }, renderProviders() {}, renderProvidersLoading() {},
    renderArchive() {}, providersWhileFetching() {}, loadUpstream() {}, discoverLocalProviders() {},
  });
  vm.runInContext(code, context);
  return { context, requests };
}

for (const first of ["cached", "fresh"]) for (const kept of [true, false]) {
  test(`${first} reply first with ${kept ? "a kept list" : "an empty cache"} keeps the fresh Providers list`, async (t) => {
    const { context, requests } = page();
    const result = vm.runInContext("loadProvidersNow()", context).then(() => null, (error) => error);
    const next = fresh(), last = kept ? cached() : null;
    t.after(async () => { for (const request of requests) request.resolve(next); await result; });
    assert.deepEqual(requests.map((request) => request.path), ["providers", "providers?last=1"]);
    const reply = (which) => requests.find((request) => request.path === (which === "fresh" ? "providers" : "providers?last=1"))
      .resolve(which === "fresh" ? next : last);
    reply(first);
    await turn();
    if (first === "cached") assert.equal(context.providers, last, "the cache is only an early placeholder");
    reply(first === "cached" ? "fresh" : "cached");
    await turn();
    assert.equal(requests.length, 2, "the cache must not invalidate and repeat the fresh request");
    assert.equal(await result, null, "an empty cache must not replace a successful fresh reply");
    assert.equal(context.providers, next, "the final list must come from the authoritative request");
    assert.deepEqual(context.providers.providers[0].headers, { "X-Team": "t1" });
  });
}
