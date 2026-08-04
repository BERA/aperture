# Vendored frontend blobs

These are **pre-built, committed** assets so the Aperture binary stays
self-contained (`//go:embed static`) with **no node build** in dev or CI.
Regenerating them is an occasional, documented manual step — never a CI build.

| File | Version (pinned) | Provenance / regeneration |
|---|---|---|
| `alpine.min.js` | Alpine.js 3.14.9 | Copied from orbit (`orbit/internal/server/static/vendor/alpine.min.js`), byte-identical to `https://cdn.jsdelivr.net/npm/alpinejs@3.14.9/dist/cdn.min.js`. Re-fetch: `curl -sSL -o alpine.min.js https://cdn.jsdelivr.net/npm/alpinejs@3.14.9/dist/cdn.min.js` |
| `tailwind.min.css` | Tailwind CSS 2.2.19 | `curl -sSL -o tailwind.min.css https://cdn.jsdelivr.net/npm/tailwindcss@2.2.19/dist/tailwind.min.css`. v2.2.19 is the last line shipping a static, JIT-free utility CSS that vendors without a node pipeline. Available to later domain screens; the shell itself is styled by `../css/styles.css`. |
| `daisyui.full.css` | DaisyUI 4.12.24 | `curl -sSL -o daisyui.full.css https://cdn.jsdelivr.net/npm/daisyui@4.12.24/dist/full.css`. Byte-identical to orbit's committed copy. |
