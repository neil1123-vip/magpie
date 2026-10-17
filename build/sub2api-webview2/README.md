# Sub2API WebView2 Build

The Windows login host uses a pinned patch for Wails `v3.0.0-beta.24`.
Prepare it from the repository root before enabling `sub2api_webview2`:

```sh
go run ./build/sub2api-webview2
go build -tags production,sub2api_webview2 -modfile .tmp-sub2api-wails/go.mod .
```

`make build`, `make install`, `make test`, `make dev-once` on Windows, and
`make release-windows` prepare and use the same modfile. The normal build
without this tag leaves browser login unavailable.

The preparer checks the resolved module version and the SHA-256 of every
patched upstream file before copying the module and applying
`wails-native-hooks.patch`. Generated files stay under `.tmp-sub2api-wails/`;
the generated modfile uses a relative replacement for that private module.
Go forbids overlays for files inside its module cache. The shared cache and
root `go.mod`/`go.sum` are unchanged. A reused private copy is verified in
full. An upstream replacement, version change or changed file is an error
requiring review.

The patch adds an opt-in raw-message-only mode, native navigation and
popup denial, worker-aware request filtering, denied permissions, a
document-start script and a native user agent for the isolated host.
Ordinary Wails windows retain their original behavior.

When upgrading Wails, recheck the linked COM interface layouts and each
patch, update the pinned version and file hashes, then prepare, compile
both Windows architectures and repeat the native host checks. A successful
build does not prove a real Cloudflare site accepts browser login or the
subsequent Go API session.
