# Vendored: pi-workflows-lean

| | |
|---|---|
| package | `@zhyx111999/pi-workflows-lean` 3.13.0-lean.1 |
| source | local checkout `C:/Users/nsus/src/pi-workflows-parent-review` (upstream `github.com/zhyx111999/pi-workflows-lean`) |
| commit | `c061a7160d6d4e6388f921cbe5803f3e7224fcba` (working tree clean when copied) |
| license | MIT (`pi-workflows/LICENSE`: QuintinShaw, Michael Livs) |
| copied with | `git archive <commit> LICENSE README.md package.json extensions dist skills src docs/*.md docs/workflow-context-surfaces.json` |

Files are byte-identical to the commit. Left out: `node_modules`, `.git`, `.github`, `tests`, `scripts`,
lint/tsconfig files, `package-lock.json`, `docs/media/demo.gif`. `dist/` is the package's own committed
build and is what runs; `src/` is kept so it can be audited against it.

The bridge depends on it as `"@zhyx111999/pi-workflows-lean": "file:vendor/pi-workflows"`; `.npmrc` sets
`install-links=true`, so npm installs a copy with only its production dependency (`acorn`).

To update: replace the directory from a new commit with the same command, update this file, run
`npm install && npm test` in `runtime/pi`.

Not vendored on purpose: any web-search extension (the Tavily extension was dropped from scope).
