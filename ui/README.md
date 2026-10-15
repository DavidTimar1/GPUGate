# The console page

`index.html` is the GPUGate console, served by the Go program at `/console`. It loads `dist/app.css` and `dist/app.js`, htmx from `vendor/htmx/`, and the IBM Plex fonts from `vendor/fonts/` (SIL Open Font License, see `vendor/fonts/OFL.txt`). The page makes no request to any other host.

## Rebuilding

```
npm --prefix ui run build
```

This copies `src/app.js` and `src/app.css` into `dist/`. Commit both. There is no bundler, no framework and no dependency to install: Node itself is all it needs. CI rebuilds the page and fails if `dist/` differs from what is committed.

The routes the page calls are registered in `internal/console/server.go`, and [the architecture](../docs/architecture.md#the-console-page) says what the page is for.
