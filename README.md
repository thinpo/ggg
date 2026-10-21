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
| another git process holds a lock file | the command is waited for and tried again — the repository is not failed |
| a submodule of a repository in the same run | synced with its parent (see [Submodules](#submodules)) |
| bare repository (no working tree) | fetched into its remote-tracking branches, and every branch the remote contains is fast-forwarded — a branch with commits of its own is left alone (see [Bare repositories](#bare-repositories)) |
| remote repository is gone (deleted, renamed, no longer visible) | the local copy is all that is left: the repository is skipped and left completely untouched — nothing is synced, nothing is renamed |
| the remote of a repository hidden by ggg 1.1 is back | `.git_old` is renamed back to `.git` and the repository is synced again |

Default branch detection order: `origin/HEAD` → `main` → `master` → first
branch on the remote.

## Submodules

The commit a submodule points at belongs to the repository that points at it:
the parent decides which commit is checked out. A submodule whose parent is
part of the same run is therefore synced **with** its parent and left alone
otherwise:

```
SKIP  subparent/sub   submodule of subparent; skipped (synced with its parent)
```

This is not only the semantically right thing to do, it also avoids two
processes writing the same index: a command in the parent repository (a
`status`, a `stash`) runs inside every submodule and takes the submodule's
index lock. When the submodule is synced on its own at the same time, the two
collide:

```
FAIL  subparent/sub   could not stash local changes: git stash push: error:
      Unable to create '…/.git/modules/sub/index.lock': File exists.
```

Three flags change this:

* *(default)* a submodule whose parent is part of the run is synced with its
  parent.
* `-sync-submodules` syncs every submodule on its own too — the parent keeps
  its pointer, the submodule is fetched.
* `-skip-submodules` leaves every submodule alone, also the ones whose parent
  is not part of the run.

## Bare repositories

A bare repository — a directory with a git directory in it, but no working tree
— is found as well. It has no `.git` inside, so it is recognised by its
layout (`HEAD`, `objects`, `refs`) and confirmed with git.

A bare repository cannot be synced like a normal one: there is no working
tree, so nothing can be stashed and no merge commit can be created. A local
commit can therefore never be reconciled with the remote — that would mean
throwing it away. `ggg` instead:

* fetches into the remote-tracking branches (`refs/remotes/<remote>/*`) and not
  into `refs/heads/*`: a mirror's own refspec writes the remote branches
  straight into the local branches, and it is forced (`+`), so a fetch would
  throw away local commits;
* moves a local branch to the remote branch it tracks, but only when the
  remote contains it — a fast-forward;
* leaves a branch that has commits of its own, or that has diverged, exactly
  where it is.

```
OK   mirror.git   fast-forward to origin/main
WARN stubborn.git  not synced: origin does not contain the local branch
```

A branch that is moved is written with `git update-ref`, and the commit it
pointed at stays in the reflog.

## Lock files

Another git process — an IDE, another terminal, a second `ggg` run — can hold a
lock file of a repository. `ggg` does not fail the repository because of that:

* commands that only look at a repository run with `GIT_OPTIONAL_LOCKS=0`, so
  they never take the optional index lock and never block another git process;
* a command that lost the race for a lock file is waited for and tried again;
* a lock that is never released is reported as such:

```
FAIL  alpha   another git process is using this repository (lock
      /home/me/src/alpha/.git/index.lock), try again later: git stash push: …
```

## Local-only repositories

A remote repository can disappear: it was deleted, renamed, or it is not
visible to this user any more. When that happens the local copy is all that
is left, so there is nothing to sync — `ggg` leaves such a repository alone:

```
SKIP  old    remote repository is gone; only the local copy is left
```

Nothing is fetched into it, nothing is merged, nothing is pushed, and its
metadata is not renamed. The directory stays exactly as it is, with its whole
history and its local work. It is still listed by `-list`, and it is synced
again as soon as its remote is back.

Only a remote that is really gone is reported like this. Authentication
failures (`Permission denied`, `could not read Username`) and network problems
(`Could not resolve host`) leave the repository alone too: it is still alive
and keeps being synced.

### Repositories hidden by ggg 1.1

ggg 1.1 renamed the metadata of a repository whose remote was gone to
`.git_old`, so the directory stopped being a repository. `ggg` does not hide
anything any more, but it still brings those repositories back: the remote of
every hidden repository is checked on each run, and as soon as one answers
again, `.git_old` is renamed back to `.git` and the repository is synced like
any other one.

```
RESTORE  old    remote origin is back; .git_old renamed back to .git
HIDDEN   gone   the remote is still gone
```

A restored repository is a normal repository again: it is listed by `-list`,
it is fetched, merged and fast-forwarded like any other one. One can also be
brought back by hand:

```sh
mv repo/.git_old repo/.git
```

## Safety rules

* Never force-pushes.
* Never `reset --hard` — a repository that has no commits yet is simply
  pointed at the remote branch instead.
* Never drops a stash that did not apply cleanly.
* Never deletes or renames a repository — a gone remote is only reported, and
  the local copy is left exactly as it is.
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
  -skip-submodules   leave every repository that is a submodule of another repository untouched
  -sync-submodules   sync submodules on their own too (the default syncs a submodule
                     together with the repository that points at it)
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
ggg 1.5.0 — syncing 5 repositories under /home/me/src

RESTORE old    remote origin is back; .git_old renamed back to .git
HIDDEN  gone   the remote is still gone

OK   alpha  fast-forward to origin/main
WARN beta   stash apply conflicted (stash kept: 305f50b7)
SKIP noremote  no remote configured
SKIP old    remote repository is gone; only the local copy is left

────────────────────────────────────────────────────────────────────────
OK    alpha     main        fast-forward to origin/main
WARN  beta      main        stash apply conflicted (stash kept: 305f50b7)
SKIP  noremote  -           no remote configured
SKIP  old       -           remote repository is gone; only the local copy is left
────────────────────────────────────────────────────────────────────────
OK 2   WARN 1   FAIL 0   SKIP 2   (5 total)
```

Exit code is `1` only when a repository actually failed.

## Notes

* Repositories are found by looking for a `.git` directory *or* file, so
  worktrees and submodules are picked up too. A directory that is a git
  directory itself — a bare repository — is found by its layout. Nested
  repositories (a repo inside another repo's working tree) are synced as well
  — each one independently.
* A submodule lives on a detached HEAD at the commit its parent pins. `ggg`
  fetches it but does not move it, so the parent's pointer stays put. A
  submodule of a repository in the same run is not synced at all, because the
  parent decides which commit it holds — see [Submodules](#submodules). Use
  `-switch-detached` to move a submodule to the default branch,
  `-sync-submodules` to sync a submodule on its own, or `-skip-submodules` to
  leave submodule repositories completely alone. Syncing a submodule that
  *is* on a branch still moves the parent's pointer, because that is what
  syncing means.
* `node_modules`, `venv`, `__pycache__` and similar directories are skipped by
  default; use `-skip` to add your own patterns.
* `.git_old` directories are skipped as well, so a repository that ggg 1.1
  hid is not descended into — but they are checked, and restored as soon as
  their remote answers again.
* Symlinked directories are not followed.
* The scan reads the working tree only; it never rewrites history.

## Testing

`test.sh` builds throw-away remotes and clones covering every branch of the
algorithm (fast-forward, dirty tree, feature branch, conflicting feature
branch, diverged main, `master` remote, unfinished merge, detached HEAD,
nested repo, non-`origin` remote, repository without commits, bare repository
— fast-forwarded and with a commit of its own, deleted remote
→ local-only repository, repository hidden by ggg 1.1 and restored, submodule
of a repository in the same run, and a repository whose lock file is held by
another process) and asserts the resulting state:

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
