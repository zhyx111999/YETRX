# YETRX validation · 2026-10-13

Validation applies to the initial YETRX fork and its persistent pi conversation integration.

- Production Next.js static export and TypeScript checks passed (32 pages).
- Actual pi SDK runtime suite: **40 passed, 0 failed**. Covers nine tools, three skills, real subagent/background delivery, model and prompt updates, canceling pending input, history restoration and credential isolation. No real upstream keys are needed.
- `pibridge` package passed, including actual Go process client → pi SDK → authenticated Go relay → fake model, a real file read, prompt replacement, session restart and activity persistence.
- `db` package passed with explicit disposable PostgreSQL 16, including migration preservation, concurrency, collision handling, rollback/retry, idempotence and retired-trigger scheduling.
- Server history/JWT and release-cache regressions passed. `go vet ./pibridge ./server` passed.
- Norma image encoders and per-request thinking regressions passed; relay tests cover image content and thinking-signature replay.
- Linux amd64 production binary built with embedded static UI and CGO disabled.

The full Windows `go test ./...` run does **not** pass. The same unmodified upstream checkout (`a9d11cb`) reproduces the company-scope test failure, evidence directory-sync access-denied errors, related finding/traffic errors and slash-versus-backslash skill-path assertions. One full-run task test also encountered a temporary-directory cleanup race; it passed when rerun in the isolated baseline subset. A shared-database concurrent package run caused a metering-table race; serial package execution passed metering.

The inherited Norma whole-package suite also contains an HTTP-client-nil retry test failure reported by the relay worker. These unrelated upstream failures are not represented as successful validation or silently modified by this fork.

Release bundles contain the matching pi runtime source and lockfile. Install its dependencies with Node 24 using `npm --prefix runtime/pi ci --omit=dev`. The web updater provides release/source links and rejects binary-only updates or rollback; full bundle installation is required.
