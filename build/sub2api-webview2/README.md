# Sub2API WebView2 Build

Windows desktop builds use pinned patches for Wails `v3.0.0-beta.24` and
go-toast `v2.0.3`. Prepare them from the repository root:

```sh
go run ./build/sub2api-webview2
go test -modfile .tmp-sub2api-wails/go.mod git.sr.ht/~jackmordaunt/go-toast/v2/internal/winrt/ui/notifications -run TestToastFactories -count=1
go build -tags production,sub2api_webview2 -modfile .tmp-sub2api-wails/go.mod .
```

`make build`, `make install`, `make test`, `make dev-once` on Windows, and
`make release-windows` prepare and use the same modfile. A bare `go build`
leaves browser login unavailable and retains upstream's unsafe toast calls;
use the prepared modfile for Windows desktop builds.

The preparer checks the resolved module version and the SHA-256 of every
patched upstream file before copying each module and applying
`wails-native-hooks.patch` and `toast-factories.patch`. Generated files stay
under `.tmp-sub2api-wails/`; the generated modfile uses relative replacements.
Go forbids overlays for files inside its module cache. The shared cache and
root `go.mod`/`go.sum` are unchanged. A reused private copy is verified in
full. An upstream replacement, version change or changed file is an error
requiring review.

The patch adds an opt-in raw-message-only mode, native navigation and
popup denial, worker-aware request filtering, denied permissions, a
document-start script and a native user agent for the isolated host.
Ordinary Wails windows retain their original behavior.

The toast patch passes the factory interface as `this` to both WinRT
notification factory methods and releases the factories. Upstream passes a
null receiver, which can terminate the entire app with an access violation
when a background alert fires. This patch covers Wails notifications and
Sub2API's direct COM delivery. `TestToastFactories` exercises each factory
in its own Windows child process without showing a notification or writing
application registration keys.

When upgrading Wails, recheck the linked COM interface layouts and each
patch, update the pinned version and file hashes, then prepare, compile
both Windows architectures and repeat the native host checks. A successful
build does not prove a real Cloudflare site accepts browser login or the
subsequent Go API session.
