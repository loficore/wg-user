# Contributing

## Hard rules

These are not style preferences. Each one exists because breaking it ships
something we cannot take back.

### 1. Never publish a release whose tests are not finished

A `v*` tag triggers `.github/workflows/release.yml`, which creates a public
GitHub release within minutes. A published release is already in the hands of
anyone who downloaded it; it cannot be honestly retracted, only superseded.

Tag only when **all** of these are true:

- local `make test`, `make verify` and `pnpm run check` pass;
- the pull request's three required CI jobs are green;
- if the change can affect the running device (transport, config parsing, the
  daemon, the installer), a smoke test on a real phone has passed:
  `wg-user status` reports `running`, the transport reports `connected`, and a
  request through SOCKS5 reaches the far side.

If a check is still running, wait for it. "Tag now, patch later" is not a
release process.

### 2. Never leave a tag that only exists locally

A tag that exists on one machine is invisible to the release workflow and to
everyone else, so it silently becomes a release that never happened — and the
next `v*` tag ends up published from a different commit than the one that was
tested.

Push the tag in the same breath as creating it, and verify it landed:

```bash
V=$(sed -n 's/^version=//p' module.prop)
jj tag set "v$V" -r main
jj git push --tag "v$V"
git ls-remote --tags origin "refs/tags/v$V"     # must print a line
```

If the wrong commit got tagged, remove both copies — `git push origin
--delete "v$V"`, `git tag -d "v$V"`, then delete the GitHub release — before
tagging again. Never move a tag that has been published.

### 3. Never push to `main`

Branch protection rejects direct pushes, owner included. That is deliberate:
every change — features, fixes, version bumps, changelog entries, release
commits — goes through a branch and a pull request, so the three CI jobs run
on exactly the tree that will be merged.

```bash
jj new main                        # start from main
# ...edit...
jj bookmark create my-change -r @
jj git push --bookmark my-change
gh pr create --base main --head my-change --fill
```

`main` requires the branch to be up to date with the three checks green, so
after a push that moves `main`, rebase and push again.

## Release checklist

Copy this into the pull request that prepares the release.

- [ ] `module.prop` has `version` and `versionCode` bumped; `webui/package.json`
      has the same version (CI compares them and fails on a mismatch).
- [ ] `docs/CHANGELOG.md` has an entry for the version, in the same voice as
      the previous ones: what broke, why, what changed.
- [ ] Every behaviour change is reflected in `docs/` and, where a user can see
      it, in `README.md`.
- [ ] `webroot/` rebuilt (`make webui`) and committed if the WebUI changed; CI
      fails on a stale bundle.
- [ ] Local: `make test`, `make verify`, `pnpm run check` (in `webui/`), then
      `make clean && make package`; the archive check lists all required
      entries.
- [ ] Pull request opened; all three required checks green.
- [ ] Device smoke test, if the change can affect the device (see rule 1).
- [ ] Merged; local tree synced to the merge commit.
- [ ] Tag created on the **merged** `main` and pushed immediately (rule 2).
- [ ] Release assets verified: `gh release download "v$V"`, `sha256sum -c
      SHA256SUMS`, `module.prop` inside the zip matches, one `wg-user` per ABI,
      and the notes describe the actual change instead of the auto-generated
      commit list.

## What CI already enforces

| Job | What it checks |
|---|---|
| `Shell + packaging checks` | `make verify`; no real config (`etc/wireproxy.conf` / `etc/tunnel.conf*`) is tracked; `module.prop` and `webui/package.json` versions agree |
| `WebUI (tsc + build + smoke test)` | `pnpm install --frozen-lockfile` → `tsc --noEmit` → esbuild → headless smoke test; fails if `webroot/` is stale |
| `wireproxy (4 arches)` | `go vet` + `go test`, the wstunnel v11.0.0 interop gate (binary pinned by sha256), then cross-compiles all four arches |

The job **display names** above are the contexts branch protection matches on.
Renaming a job silently drops it from the required set, so a rename has to
update the protection rule in the same breath.

Nothing in CI can check rules 1 and 2: the first needs judgement about what
"tested" means for a change, and the second is a property of your machine, not
of the repository.
