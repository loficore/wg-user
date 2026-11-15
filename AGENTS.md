# AGENTS.md

Read [`CONTRIBUTING.md`](CONTRIBUTING.md) before changing anything in this
repository. It is short, and three of its rules are absolute:

1. **Never publish a release whose tests are not finished.** A `v*` tag creates
   a public GitHub release within minutes; wait for `make test`, `make verify`,
   `pnpm run check`, the pull request's three checks and — for anything that can
   affect the device — a smoke test on a real phone.
2. **Never leave a tag that only exists locally.** Push it immediately and
   confirm with `git ls-remote --tags origin "refs/tags/v$V"`.
3. **Never push to `main`.** Branch protection rejects it, admins included.
   Every change goes through a bookmark and a pull request.

`CONTRIBUTING.md` also carries the release checklist and the list of what CI
enforces (including that the required-check contexts are the job **display
names**, so renaming a job silently weakens branch protection).
