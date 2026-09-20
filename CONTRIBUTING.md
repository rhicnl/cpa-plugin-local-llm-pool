# Contributing

Thanks for looking at `local-llm-pool`. This is a small, focused plugin, and the rules below
exist mostly to keep it that way.

## Before you write code

**Open an issue first for anything non-trivial.** A bug fix with an obvious cause is fine to
send straight as a pull request. A new config key, a new routing behaviour, a new capability
or a refactor is worth agreeing on first, because the plugin's config surface and its
`/status` payload are contracts other people depend on.

## Working on it

```bash
make build            # cgo c-shared plugin -> bin/local-llm-pool.{so,dylib,dll}
make test             # go test -race -count=1 ./...
make vet              # go vet ./...
make package          # release archive for this platform -> dist/
make verify-package   # re-open that archive and assert the store's layout rules
```

`make build` needs cgo and a C toolchain, because the host loads the plugin with `dlopen`.

**Never commit an environment file.** `.env` and `.env.*` are gitignored, and only the
`.env.example`, `.env.sample` and `.env.template` names are allowed through. If you need a
local API key or proxy URL to try the plugin against a real host, keep it in an ignored file
or your shell, and keep it out of issues, pull requests and test fixtures too.

## Architecture rules

These are not style preferences; the layout is what makes the plugin testable at all.

- **All routing logic lives in `internal/tracker`, as pure Go with no cgo.** It takes an
  injectable clock (`New(cfg, now)`), so behaviour that depends on time is tested with a fake
  clock rather than by waiting.
- **`main.go` stays a thin cgo ABI shim.** It exports the C entry points and nothing else.
  `dispatch.go` is the JSON envelope dispatch and should stay close to a switch statement.
- **Release archive rules live in `internal/pkgzip`.** The Makefile and the release workflow
  both call it, so the packaging that CI publishes is the packaging that local tests check.
  Do not reimplement zip layout or checksum formatting anywhere else.

## Tests

- **No wall-clock sleeps.** Not `time.Sleep`, not a retry loop that waits for something to
  expire. Advance the fake clock instead. A test that sleeps is a test that will flake on a
  loaded CI runner.
- Run the suite with `-race`. Concurrency is the whole point of this plugin, and the race
  detector has caught real bugs here.
- New behaviour needs a test that fails without the change. If you are fixing a bug, the
  most useful thing you can do is write the failing test first and say so in the PR.

## Compatibility

The plugin is configured by other people's proxy configs and read by their monitoring, so:

- **Existing config keys keep working.** The shorthand top-level `group_aliases` plus
  `backends` form is pinned by a compatibility test and must keep parsing to one pool with
  the same fill order and limits.
- **Existing `/status` keys keep working.** `backends`, `totals` and `config` are part of the
  contract. Adding keys is fine; renaming or removing them is not.
- Anything that does break one of those needs a major version bump and a note in the pull
  request explaining the migration.

## Commits and pull requests

Use [Conventional Commits](https://www.conventionalcommits.org/): `feat:`, `fix:`, `docs:`,
`test:`, `refactor:`, `build:`, `ci:`, `chore:`. Keep a pull request to one logical change.

Checklist before you open one:

- [ ] `gofmt -l .` is empty
- [ ] `go vet ./...` is clean
- [ ] `go test -race -count=1 ./...` passes
- [ ] Tests added or updated for the change, with no wall-clock sleeps
- [ ] README updated if behaviour or configuration changed
- [ ] No breaking change to existing config keys or `/status` keys

## Licensing

Contributions are accepted under the [MIT license](LICENSE). By opening a pull request you
agree your contribution is licensed under those terms.

## Releases

Releases are cut by the maintainer by pushing a `v*.*.*` tag, which builds all five platform
archives and publishes them. Please do not push tags or open pull requests that bump a
version number.
