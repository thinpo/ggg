#!/bin/bash
# Test harness for ggg. Builds throw-away git remotes and clones in a range of
# states, runs ggg over them and checks the result.
#
#   ./test.sh [path-to-ggg]     (default: ./ggg in this directory)
set -u

HERE=$(cd "$(dirname "$0")" && pwd)
GG=${1:-$HERE/ggg}
# The fixtures live in a temporary directory and the script changes into it,
# so the binary must be addressed by an absolute path.
GG="$(cd "$(dirname "$GG")" && pwd)/$(basename "$GG")"
if [ ! -x "$GG" ]; then
  echo "building ggg..."
  (cd "$HERE" && go build -o ggg .) || exit 1
  GG="$HERE/ggg"
fi

BASE=$(mktemp -d "${TMPDIR:-/tmp}/gggtest.XXXXXX")
REMOTE="$BASE/remotes"; WORK="$BASE/work"
mkdir -p "$REMOTE" "$WORK"
export GIT_AUTHOR_NAME=Test GIT_AUTHOR_EMAIL=t@e.st
export GIT_COMMITTER_NAME=Test GIT_COMMITTER_EMAIL=t@e.st
export GIT_AUTHOR_DATE="2024-01-01 00:00:00" GIT_COMMITTER_DATE="2024-01-01 00:00:00"

pass=0; fail=0
check() { # $1 description, $2 expected, $3 actual
  if [ "$2" = "$3" ]; then
    pass=$((pass+1)); printf '  ok   %s\n' "$1"
  else
    fail=$((fail+1)); printf '  FAIL %s (expected %s, got %s)\n' "$1" "$2" "$3"
  fi
}

seed() { # $1 name [$2 branch]
  local n=$1
  local b=${2:-main}
  git init --bare -q -b "$b" "$REMOTE/$n.git"
  local t="$BASE/seed-$n"; git clone -q "$REMOTE/$n.git" "$t" 2>/dev/null
  (cd "$t" && git checkout -q -B "$b" && echo "hello $n" > file.txt && echo "keep" > keep.txt \
     && git add -A && git commit -q -m init && git push -q origin "$b")
  rm -rf "$t"
}
remote_commit() { # $1 name [$2 branch]
  local n=$1
  local b=${2:-main}
  local t="$BASE/push-$n"; rm -rf "$t"; git clone -q "$REMOTE/$n.git" "$t"
  (cd "$t" && git checkout -q "$b" && echo "remote change" >> keep.txt \
     && git add -A && git commit -q -m "remote: change" && git push -q origin "$b")
  rm -rf "$t"
}
remote_rewrite() { # rewrite file.txt on the remote
  local n=$1
  local t="$BASE/push-$n"; rm -rf "$t"; git clone -q "$REMOTE/$n.git" "$t"
  (cd "$t" && git checkout -q main && echo "THEIR VERSION" > file.txt \
     && git commit -q -am "remote: rewrite" && git push -q origin main)
  rm -rf "$t"
}

echo "== fixtures =="
# 1 clean clone, remote ahead
seed alpha; git clone -q "$REMOTE/alpha.git" "$WORK/alpha"; remote_commit alpha
# 2 dirty tree (modified + untracked)
seed beta; git clone -q "$REMOTE/beta.git" "$WORK/beta"; remote_commit beta
echo "local edit" >> "$WORK/beta/file.txt"; echo "new file" > "$WORK/beta/untracked.txt"
# 3 feature branch, remote main moved ahead
seed gamma; git clone -q "$REMOTE/gamma.git" "$WORK/gamma"
(cd "$WORK/gamma" && git checkout -q -b feature && echo work > feature.txt \
   && git add -A && git commit -q -m "feature work"); remote_commit gamma
# 4 feature branch conflicting with origin/main
seed delta; git clone -q "$REMOTE/delta.git" "$WORK/delta"
(cd "$WORK/delta" && git checkout -q -b feature && echo "feature version" > file.txt \
   && git add -A && git commit -q -m "feature rewrite"); remote_rewrite delta
# 5 no remote at all
mkdir -p "$WORK/noremote"; git init -q -b main "$WORK/noremote"
(cd "$WORK/noremote" && echo x > a.txt && git add -A && git commit -q -m init)
# 6 diverged main branch
seed epsilon; git clone -q "$REMOTE/epsilon.git" "$WORK/epsilon"
(cd "$WORK/epsilon" && echo "local version" > file.txt \
   && git commit -q -am "local rewrite"); remote_rewrite epsilon
# 7 master-only remote
seed kappa master; git clone -q -b master "$REMOTE/kappa.git" "$WORK/kappa"; remote_commit kappa master
# 8 unfinished merge
seed eta; git clone -q "$REMOTE/eta.git" "$WORK/eta"
(cd "$WORK/eta" && git checkout -q -b feature && echo local > file.txt && git commit -q -am "local edit")
(cd "$WORK/eta" && git checkout -q main && echo other > file.txt && git commit -q -am "other edit" \
   && git push -q origin main && git checkout -q feature && git merge main >/dev/null 2>&1; true)
# 9 detached HEAD
seed theta; git clone -q "$REMOTE/theta.git" "$WORK/theta"
(cd "$WORK/theta" && git checkout -q --detach HEAD)
# 10 nested repository
seed outer; git clone -q "$REMOTE/outer.git" "$WORK/outer"
seed inner; git clone -q "$REMOTE/inner.git" "$WORK/outer/vendor/inner"; remote_commit inner
# 11 remote named "upstream"
seed up; git clone -q "$REMOTE/up.git" "$WORK/up"
git -C "$WORK/up" remote rename origin upstream; remote_commit up
# 12 repository without commits
seed fresh; git init -q -b main "$WORK/fresh"
git -C "$WORK/fresh" remote add origin "$REMOTE/fresh.git"
# 13 bare repository: a mirror of a remote that moves ahead
seed bare; git clone --bare -q "$REMOTE/bare.git" "$WORK/bare.git"; remote_commit bare
# 13b bare repository with a commit of its own: the remote does not contain
#     it, and a bare repository cannot merge, so the branch stays where it is
seed stubborn; git clone --bare -q "$REMOTE/stubborn.git" "$WORK/stubborn.git"
git -C "$WORK/stubborn.git" config user.name Test
git -C "$WORK/stubborn.git" config user.email t@e.st
STUBBORN_COMMIT=$(echo stub | git -C "$WORK/stubborn.git" commit-tree \
  "$(git -C "$WORK/stubborn.git" rev-parse 'main^{tree}')" -p main -m "local only commit")
git -C "$WORK/stubborn.git" update-ref refs/heads/main "$STUBBORN_COMMIT"
remote_commit stubborn
# 14 a real submodule inside a parent repository
seed subparent; git clone -q "$REMOTE/subparent.git" "$WORK/subparent"
seed submod
git -C "$WORK/subparent" -c protocol.file.allow=always submodule add -q "$REMOTE/submod.git" sub 2>/dev/null
git -C "$WORK/subparent" commit -q -m "add submodule" 2>/dev/null
git -C "$WORK/subparent" -c protocol.file.allow=always submodule update --init -q 2>/dev/null
# The remote of the submodule moves ahead: the checkout stays behind.
remote_commit submod
# 15 parent whose only local change is a moved submodule pointer: git refuses
#    to stash submodule pointers, so "git stash push" saves nothing.
seed pointer; git clone -q "$REMOTE/pointer.git" "$WORK/pointer"
git -C "$WORK/pointer" -c protocol.file.allow=always submodule add -q "$REMOTE/inner.git" sub 2>/dev/null
git -C "$WORK/pointer" commit -q -m "add submodule" 2>/dev/null
git -C "$WORK/pointer" -c protocol.file.allow=always submodule update --init -q 2>/dev/null
git -C "$WORK/pointer/sub" commit -q --allow-empty -m "submodule moved" 2>/dev/null
# 16 a repository whose remote was deleted: only the local copy is left, so
#    ggg does not sync it and does not touch it.
seed vanished; git clone -q "$REMOTE/vanished.git" "$WORK/vanished"
rm -rf "$REMOTE/vanished.git"
echo "local work" >> "$WORK/vanished/file.txt"
# 17 a repository hidden by ggg 1.1 (its metadata sits in .git_old) whose
#    remote comes back: ggg restores .git from .git_old and syncs it again.
seed reborn; git clone -q "$REMOTE/reborn.git" "$WORK/reborn"
mv "$REMOTE/reborn.git" "$BASE/reborn-away.git"
mv "$WORK/reborn/.git" "$WORK/reborn/.git_old"

echo
echo "== dry run =="
before=$(cd "$WORK/beta" && git status --porcelain; git rev-parse HEAD)
(cd "$WORK" && NO_COLOR=1 "$GG" -dry-run >/dev/null 2>&1)
after=$(cd "$WORK/beta" && git status --porcelain; git rev-parse HEAD)
check "dry run changes nothing" "$before" "$after"

echo
echo "== sync =="
SYNC_OUT=$(cd "$WORK" && NO_COLOR=1 "$GG" 2>&1)
echo "$SYNC_OUT" | tail -25

echo
echo "== submodules =="
# The commit of a submodule belongs to the repository that points at it, so a
# submodule whose parent is part of the same run is synced with its parent.
SUB_ORIGIN_BEFORE=$(git -C "$WORK/subparent/sub" rev-parse origin/main)
out=$(cd "$WORK" && NO_COLOR=1 "$GG" 2>&1)
check "submodule skipped by default" "yes" \
  "$(echo "$out" | grep 'subparent/sub' | grep -q 'submodule of subparent; skipped' && echo yes || echo no)"
check "submodule not fetched by default" "$SUB_ORIGIN_BEFORE" \
  "$(git -C "$WORK/subparent/sub" rev-parse origin/main)"
# -sync-submodules syncs the submodule on its own.
out=$(cd "$WORK" && NO_COLOR=1 "$GG" -sync-submodules 2>&1)
check "submodule not skipped with -sync-submodules" "no" \
  "$(echo "$out" | grep 'subparent/sub' | grep -q 'submodule of subparent; skipped' && echo yes || echo no)"
check "submodule fetched with -sync-submodules" "$(git -C "$REMOTE/submod.git" rev-parse main)" \
  "$(git -C "$WORK/subparent/sub" rev-parse origin/main)"
# -skip-submodules leaves every submodule alone.
out=$(cd "$WORK" && NO_COLOR=1 "$GG" -skip-submodules 2>&1)
check "submodule reported as skipped" "yes" \
  "$(echo "$out" | grep 'subparent/sub' | grep -q 'submodule of subparent; skipped' && echo yes || echo no)"
check "contradicting flags rejected" "2" \
  "$(cd "$WORK" && NO_COLOR=1 "$GG" -skip-submodules -sync-submodules >/dev/null 2>&1; echo $?)"

echo
echo "== lock contention =="
# Another git process holds the index lock: ggg waits for it instead of
# failing the repository.
seed locked; git clone -q "$REMOTE/locked.git" "$WORK/locked"
remote_commit locked
echo "local edit" >> "$WORK/locked/file.txt"
touch "$WORK/locked/.git/index.lock"
( sleep 1; rm -f "$WORK/locked/.git/index.lock" ) &
LOCKOUT=$(cd "$WORK" && NO_COLOR=1 "$GG" 2>&1)
check "no failure while a lock is held" "0" "$(echo "$LOCKOUT" | grep -c '^FAIL')"
check "locked repository synced" "yes" \
  "$(git -C "$WORK/locked" merge-base --is-ancestor origin/main HEAD && echo yes || echo no)"
check "locked repository keeps its work" " M file.txt" \
  "$(git -C "$WORK/locked" status --porcelain | head -1)"
check "locked repository has no stash left" "" "$(git -C "$WORK/locked" stash list)"

# A lock that is never released is reported as such.
seed stuck; git clone -q "$REMOTE/stuck.git" "$WORK/stuck"
echo "local edit" >> "$WORK/stuck/file.txt"
touch "$WORK/stuck/.git/index.lock"
STUCKOUT=$(cd "$WORK" && NO_COLOR=1 "$GG" 2>&1)
rm -f "$WORK/stuck/.git/index.lock"
check "a lock that is never released is reported" "yes" \
  "$(echo "$STUCKOUT" | grep -q 'another git process is using this repository' && echo yes || echo no)"

echo
echo "== recovery =="
# The remote of "reborn" comes back, with a new commit on top.
mv "$BASE/reborn-away.git" "$REMOTE/reborn.git"
remote_commit reborn
(cd "$WORK" && NO_COLOR=1 "$GG" 2>&1 | grep -E 'RESTORE|HIDDEN' || true)

echo
echo "== checks =="
check "alpha fast-forwarded" "$(git -C "$WORK/alpha" rev-parse origin/main)" "$(git -C "$WORK/alpha" rev-parse HEAD)"
check "alpha clean" "" "$(git -C "$WORK/alpha" status --porcelain)"
check "beta local edit kept" " M file.txt" "$(git -C "$WORK/beta" status --porcelain | head -1)"
check "beta untracked kept" "?? untracked.txt" "$(git -C "$WORK/beta" status --porcelain | tail -1)"
check "beta no stash left" "" "$(git -C "$WORK/beta" stash list)"
check "beta synced" "origin/main" "$(git -C "$WORK/beta" rev-parse --abbrev-ref '@{upstream}')"
check "gamma contains origin/main" "yes" "$(git -C "$WORK/gamma" merge-base --is-ancestor origin/main feature && echo yes || echo no)"
check "gamma local commit kept" "1" "$(git -C "$WORK/gamma" log --oneline feature | grep -c 'feature work')"
check "delta remote content won" "THEIR VERSION" "$(cat "$WORK/delta/file.txt")"
check "delta local commit kept" "1" "$(git -C "$WORK/delta" log --oneline feature | grep -c 'feature rewrite')"
check "epsilon contains origin/main" "yes" "$(git -C "$WORK/epsilon" merge-base --is-ancestor origin/main HEAD && echo yes || echo no)"
check "epsilon local commit kept" "1" "$(git -C "$WORK/epsilon" log --oneline HEAD | grep -c 'local rewrite')"
check "kappa uses master" "master" "$(git -C "$WORK/kappa" rev-parse --abbrev-ref HEAD)"
check "eta untouched" "UU file.txt" "$(git -C "$WORK/eta" status --porcelain)"
check "theta stays detached" "HEAD" "$(git -C "$WORK/theta" rev-parse --abbrev-ref HEAD)"
check "nested repo synced" "origin/main" "$(git -C "$WORK/outer/vendor/inner" rev-parse --abbrev-ref '@{upstream}')"
check "upstream remote used" "upstream/main" "$(git -C "$WORK/up" rev-parse --abbrev-ref '@{upstream}')"
check "fresh repo initialised" "main" "$(git -C "$WORK/fresh" rev-parse --abbrev-ref HEAD)"
check "fresh repo has commits" "1" "$(git -C "$WORK/fresh" rev-list --count HEAD)"
check "bare repository found" "yes" \
  "$(cd "$WORK" && NO_COLOR=1 "$GG" -list | grep -q 'bare.git' && echo yes || echo no)"
check "bare repository fast-forwarded" "$(git -C "$REMOTE/bare.git" rev-parse main)" \
  "$(git -C "$WORK/bare.git" rev-parse refs/heads/main)"
check "bare repository keeps its remote-tracking branch" "$(git -C "$REMOTE/bare.git" rev-parse main)" \
  "$(git -C "$WORK/bare.git" rev-parse refs/remotes/origin/main)"
check "bare repository with a local commit kept" "$STUBBORN_COMMIT" \
  "$(git -C "$WORK/stubborn.git" rev-parse refs/heads/main)"
check "bare repository with a local commit reported" "yes" \
  "$(echo "$SYNC_OUT" | grep 'stubborn.git' | grep -q 'not synced' && echo yes || echo no)"
check "bare repository with a local commit is fetched" "$(git -C "$REMOTE/stubborn.git" rev-parse main)" \
  "$(git -C "$WORK/stubborn.git" rev-parse refs/remotes/origin/main)"
check "nothing failed" "0" "$(echo "$SYNC_OUT" | grep -c '^FAIL')"
check "pointer parent synced" "origin/main" "$(git -C "$WORK/pointer" rev-parse --abbrev-ref '@{upstream}')"
check "pointer submodule kept" " M sub" "$(git -C "$WORK/pointer" status --porcelain | head -1)"
check "pointer no stash left" "" "$(git -C "$WORK/pointer" stash list)"
check "gone remote left untouched" "yes" "$([ -d "$WORK/vanished/.git" ] && echo yes || echo no)"
check "gone remote not hidden" "no" "$([ -e "$WORK/vanished/.git_old" ] && echo yes || echo no)"
check "gone remote history intact" "init" "$(git -C "$WORK/vanished" log --oneline -1 | cut -d' ' -f2-)"
check "gone remote keeps local work" " M file.txt" "$(git -C "$WORK/vanished" status --porcelain | head -1)"
check "gone remote not synced" "yes" \
  "$(echo "$SYNC_OUT" | grep 'vanished' | grep -q 'only the local copy is left' && echo yes || echo no)"
check "gone remote still listed" "yes" \
  "$(cd "$WORK" && NO_COLOR=1 "$GG" -list | grep -q 'vanished' && echo yes || echo no)"
check "gone remote left alone next run" "yes" "$([ -d "$WORK/vanished/.git" ] && echo yes || echo no)"
check "recovered repo restored" "yes" "$([ -d "$WORK/reborn/.git" ] && echo yes || echo no)"
check "recovered repo has no .git_old" "no" "$([ -e "$WORK/reborn/.git_old" ] && echo yes || echo no)"
check "recovered repo synced" "$(git -C "$WORK/reborn" rev-parse origin/main)" \
  "$(git -C "$WORK/reborn" rev-parse HEAD)"
check "recovered repo got the new commit" "1" \
  "$(grep -c 'remote change' "$WORK/reborn/keep.txt")"
check "recovered repo in the sync list" "yes" \
  "$(cd "$WORK" && NO_COLOR=1 "$GG" -list | grep -q 'reborn' && echo yes || echo no)"

echo
echo "passed: $pass  failed: $fail"
echo "test tree: $BASE"
[ "$fail" -eq 0 ]
