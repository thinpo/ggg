# ggg — Git Group Sync

`ggg` finds **every git repository under a directory** and synchronises each one
with its remote.

The rule it optimises for:

> fetch as much of the remote as possible, while keeping every byte of local
> work (uncommitted changes, untracked files and local commits).

```
go build -o ggg .        # in this directory
./ggg                    # sync every repo under the current directory
./ggg -root ~/src        # sync every repo under ~/src
```

## Install

Download the archive for your platform from the
[releases page](https://github.com/thinpo/ggg/releases):

```sh
tar xzf ggg-darwin-arm64_<version>.tar.gz
mv ggg-darwin-arm64 ~/local/bin/ggg
```

or build it from source:

```sh
git clone https://github.com/thinpo/ggg.git
cd ggg && make install        # installs into ~/local/bin/ggg
```

`make` alone runs gofmt, go vet, the test suite and a build.

## What it does per repository

| Situation | What `ggg` does |
| --- | --- |
| unfinished merge / rebase / cherry-pick | refuses to touch the repo (`-recover` aborts it and continues) |
| no remote | skipped |
| dirty working tree | `git stash push --include-untracked` → sync → `git stash pop` |
| on `main`/`master`, remote ahead | fast-forward (`merge --ff-only`) |
| on `main`/`master`, diverged | `merge` (never a reset) so local commits survive |
| on a feature branch | merges the branch's own upstream and then `origin/main` into it |
| merge conflicts | retries with `-X theirs` (remote wins the conflicting hunks) so the remote history still lands locally; your version stays in the commit history |
| merge still conflicts | aborts, keeps the feature branch and its local commits, switches to `main`/`master` and syncs that instead |
| detached HEAD | fetched only and reported — the commit is not moved (`-switch-detached` opts in) |
| stash pop conflicts | the stash is **kept** and the conflict is reported — nothing is lost |
| remote repository is gone (deleted, renamed, no longer visible) | `.git` is renamed to `.git_old`, so the next run skips the directory |

Default branch detection order: `origin/HEAD` → `main` → `master` → first
branch on the remote.

## Gone remotes

When a remote repository no longer exists — it was deleted, renamed, or it is
not visible to this user any more — `ggg` does not keep failing on it. It
renames the git metadata:

```
repo/.git  →  repo/.git_old
```

The directory keeps **all** of its files and the **whole** history; it simply
is not a git repository any more, so the next scan skips it instead of
reporting the same error again. Nothing is deleted, and the repository can be
brought back at any time:

```sh
mv repo/.git_old repo/.git
```

Only a remote that is really gone triggers this. Authentication failures
(`Permission denied`, `could not read Username`) and network problems
(`Could not resolve host`) leave the repository alone: it is still alive and
keeps being synced.

## Safety rules

* Never force-pushes.
* Never `reset --hard` — a repository that has no commits yet is simply
  pointed at the remote branch instead.
* Never drops a stash that did not apply cleanly.
* Never deletes a repository — a gone remote only renames `.git` to `.git_old`.
* Only ever creates merge commits — local history is never rewritten.

## Flags

```
  -root string       directory to scan for git repositories (default ".")
  -remote string     remote to sync from (default "origin")
  -jobs int          repositories synced in parallel (default: min(NumCPU, 8))
  -dry-run           print the plan and the git commands, change nothing
  -auto-resolve str  conflict strategies tried in order: theirs,ours,union,none
                     (default "theirs")
  -merge-upstream    also merge the branch's own upstream into the branch (default true)
  -push              push the current branch to its upstream after syncing
  -submodules        run "git submodule update --init --recursive" afterwards
  -skip-submodules   leave repositories that are submodules of another repository untouched
  -switch-detached   move a detached HEAD to the default branch and sync it
  -recover           abort an unfinished merge/rebase and continue
  -list              list the repositories and exit
  -maxdepth int      maximum directory depth to scan (0 = unlimited)
  -skip regexp      paths matching this regexp are skipped
  -timeout duration  per-git-command timeout (default 10m0s)
  -v                 verbose output
  -version           print the version and exit
```

### `-auto-resolve`

When a merge conflicts, `ggg` aborts it and retries with a strategy driver:

* `theirs` — the remote version wins the conflicting hunks (default)
* `ours` — your version wins the conflicting hunks
* `union` — keep both sides of every conflict

The conflicting side is never lost: it stays in the commit history as the
first parent of the merge commit.

## Output

```
ggg 1.0.0 — syncing 5 repositories under /home/me/src

OK   alpha  fast-forward to origin/main
WARN beta   stash apply conflicted (stash kept: 305f50b7)
SKIP noremote  no remote configured
SKIP old    remote repository is gone; renamed .git to .git_old

────────────────────────────────────────────────────────────────────────
OK    alpha     main        fast-forward to origin/main
WARN  beta      main        stash apply conflicted (stash kept: 305f50b7)
SKIP  noremote  -           no remote configured
SKIP  old       -           remote repository is gone; renamed .git to .git_old
────────────────────────────────────────────────────────────────────────
OK 2   WARN 1   FAIL 0   SKIP 2   (5 total)
```

Exit code is `1` only when a repository actually failed.

## Notes

* Repositories are found by looking for a `.git` directory *or* file, so
  worktrees and submodules are picked up too. Nested repositories (a repo
  inside another repo's working tree) are synced as well — each one
  independently.
* A submodule lives on a detached HEAD at the commit its parent pins. `ggg`
  fetches it but does not move it, so the parent's pointer stays put. Use
  `-switch-detached` to move it to the default branch, or `-skip-submodules`
  to leave submodule repositories completely alone. Syncing a submodule that
  *is* on a branch still moves the parent's pointer, because that is what
  syncing means.
* `node_modules`, `venv`, `__pycache__` and similar directories are skipped by
  default; use `-skip` to add your own patterns.
* `.git_old` directories are skipped as well, so a repository whose remote is
  gone is not descended into.
* Symlinked directories are not followed.
* The scan reads the working tree only; it never rewrites history.

## Testing

`test.sh` builds throw-away remotes and clones covering every branch of the
algorithm (fast-forward, dirty tree, feature branch, conflicting feature
branch, diverged main, `master` remote, unfinished merge, detached HEAD,
nested repo, non-`origin` remote, repository without commits, deleted remote)
and asserts the resulting state:

```
./test.sh          # builds ./ggg first if needed
```

## Releasing

```sh
make release       # ./release.sh <version>
```

Cross-compiles the binaries for every supported platform, writes the
`SHA256SUMS` file, tags `v<version>`, pushes the tag and publishes the
GitHub release with the archives attached. The version embedded in the
binaries comes from the `version` variable in `main.go` and can be overridden
with `-ldflags "-X main.version=..."`.
