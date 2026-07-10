# target

Generic network test-target service. Pure Go; see [README.md](README.md).

## Dev loop

- `task check` — build + vet + gofmt check + test (approve once, reuse).
- `task smoke` — boot locally, exercise every path incl. auth'd callbacks.
- `task e2e` — full flow in a Docker container.

<!-- dev-permissions:fs-nav -->
## Filesystem navigation

- Only `cd` to the project root or `.claude/worktrees/<worktree>`. Do **NOT**
  `cd` to any other directory.
- Instead of changing directories, use paths directly in filesystem commands:
  **relative** paths for locations inside the root, **absolute** paths for
  locations outside it.

## Running scripts

- Do **NOT** run inline programs (`python -c`, `bash -c`, `node -e`/`-p`) — they
  re-prompt on every edit and can't be approved once.
- Write the script to a file under the scratch dir (`tmp`), then run the file.
  Approved once, it re-runs after edits without re-prompting.
- Prefer a gitignored scratch dir in the repo over the system temp dir.
<!-- /dev-permissions:fs-nav -->
