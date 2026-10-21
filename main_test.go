package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// testGit runs a git command in dir and fails the test when it does not work.
func testGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=t@e.st",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=t@e.st")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// testRepo creates a repository with one commit.
func testRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	testGit(t, dir, "init", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	testGit(t, dir, "add", "-A")
	testGit(t, dir, "commit", "-q", "-m", "init")
	return dir
}

func TestLockContention(t *testing.T) {
	tests := []struct {
		msg  string
		path string
		lock bool
	}{
		{
			msg:  "git stash push: Unable to create '/tmp/x/.git/modules/arrow-rs/index.lock': File exists.",
			path: "/tmp/x/.git/modules/arrow-rs/index.lock",
			lock: true,
		},
		{
			msg:  "git fetch: Unable to create '/tmp/x/.git/packed-refs.lock': File exists.",
			path: "/tmp/x/.git/packed-refs.lock",
			lock: true,
		},
		{
			msg:  "git commit: error: could not write index",
			lock: true,
		},
		{
			msg:  "git update-ref: cannot lock ref 'refs/heads/main': is at abc but expected def",
			lock: true,
		},
		{
			msg:  "git update-ref: cannot lock ref 'refs/heads/main': reference already exists",
			lock: false,
		},
		{
			msg:  "git fetch: fatal: the remote end hung up unexpectedly",
			lock: false,
		},
		{
			msg:  "git merge: fatal: refusing to merge unrelated histories",
			lock: false,
		},
	}
	for _, tc := range tests {
		path, locked := lockContention(&gitError{msg: tc.msg})
		if locked != tc.lock {
			t.Errorf("lockContention(%q) = %v, want %v", tc.msg, locked, tc.lock)
		}
		if tc.path != "" && path != tc.path {
			t.Errorf("lockContention(%q) path = %q, want %q", tc.msg, path, tc.path)
		}
	}
	if _, locked := lockContention(nil); locked {
		t.Error("lockContention(nil) reported a lock")
	}
}

// gitError is an error with a fixed message.
type gitError struct{ msg string }

func (e *gitError) Error() string { return e.msg }

// TestReadOnlyDoesNotTouchTheIndex shows that a read-only command does not
// write the index of the repository it looks at.
//
// This is what keeps ggg out of the way of another git process: a status in
// a parent repository runs a status inside every submodule, and that status
// would otherwise take the submodule's index lock.
func TestReadOnlyDoesNotTouchTheIndex(t *testing.T) {
	dir := testRepo(t)
	g := git{dir: dir, timeout: time.Minute, log: func(string) {}}

	index := filepath.Join(dir, ".git", "index")

	// A read-only command: the index must stay untouched.
	touch(t, filepath.Join(dir, "file.txt"))
	before := modTime(t, index)
	time.Sleep(20 * time.Millisecond)
	if _, err := g.output("status", "--porcelain"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if after := modTime(t, index); !after.Equal(before) {
		t.Errorf("a read-only status rewrote the index: %v -> %v", before, after)
	}

	// A command that writes: the index is refreshed.
	touch(t, filepath.Join(dir, "file.txt"))
	before = modTime(t, index)
	time.Sleep(20 * time.Millisecond)
	if err := g.run("status", "--porcelain"); err != nil {
		t.Fatalf("status: %v", err)
	}
	if after := modTime(t, index); after.Equal(before) {
		t.Errorf("a status without the optional lock did not refresh the index")
	}
}

// TestMutateWaitsForALock shows that a command that loses the race for a
// lock file is run again instead of failing the repository.
func TestMutateWaitsForALock(t *testing.T) {
	dir := testRepo(t)
	waits := 0
	g := git{dir: dir, timeout: time.Minute, log: func(msg string) {
		if strings.Contains(msg, "waiting") {
			waits++
		}
	}}

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	go func() {
		time.Sleep(700 * time.Millisecond)
		_ = os.Remove(lock)
	}()

	if err := g.mutate("checkout", "-q", "-B", "other"); err != nil {
		t.Fatalf("mutate did not wait for the lock: %v", err)
	}
	if waits == 0 {
		t.Error("the command was not tried again after the lock was taken")
	}
	if _, err := g.output("rev-parse", "--verify", "--quiet", "other"); err != nil {
		t.Errorf("the command was not run: %v", err)
	}
}

// TestMutateGivesUpOnALock shows that a lock that is never released is
// reported instead of hanging forever.
func TestMutateGivesUpOnALock(t *testing.T) {
	dir := testRepo(t)
	g := git{dir: dir, timeout: time.Minute, log: func(string) {}}

	lock := filepath.Join(dir, ".git", "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(lock)

	err := g.mutate("checkout", "-q", "-B", "other")
	if err == nil {
		t.Fatal("mutate did not fail on the lock")
	}
	if !strings.Contains(err.Error(), "another git process") {
		t.Errorf("the error does not mention the lock: %v", err)
	}
}

func TestCanonical(t *testing.T) {
	dir := t.TempDir()
	if canonical(dir) != canonical(dir+"/") {
		t.Error("a trailing slash changed the path")
	}
	if canonical(dir) == canonical(filepath.Join(dir, "sub")) {
		t.Error("two different directories have the same canonical path")
	}
}

// TestSkipSubmodule shows which submodules are synced on their own.
func TestSkipSubmodule(t *testing.T) {
	parent := testRepo(t)
	sub := filepath.Join(parent, "sub")
	testGit(t, parent, "-c", "protocol.file.allow=always", "submodule", "add", "-q", parent, "sub")
	testGit(t, parent, "commit", "-q", "-m", "add submodule")

	g := git{dir: sub, timeout: time.Minute, log: func(string) {}}
	if g.superproject() == "" {
		t.Fatal("the submodule has no superproject")
	}

	// The parent is part of the run: the submodule is synced with it.
	s := &syncer{cfg: config{repos: map[string]bool{canonical(parent): true}}}
	if skip, _ := s.skipSubmodule(g); !skip {
		t.Error("a submodule of a repository in the run was not skipped")
	}

	// The parent is not part of the run: the submodule is synced on its own.
	s = &syncer{cfg: config{repos: map[string]bool{}}}
	if skip, _ := s.skipSubmodule(g); skip {
		t.Error("a submodule whose parent is not in the run was skipped")
	}

	// -sync-submodules syncs them all.
	s = &syncer{cfg: config{syncSubmodules: true, repos: map[string]bool{canonical(parent): true}}}
	if skip, _ := s.skipSubmodule(g); skip {
		t.Error("-sync-submodules did not sync the submodule")
	}

	// -skip-submodules leaves every submodule alone.
	s = &syncer{cfg: config{skipSubmodules: true, repos: map[string]bool{}}}
	if skip, _ := s.skipSubmodule(g); !skip {
		t.Error("-skip-submodules did not skip the submodule")
	}

	// A repository that is not a submodule is never skipped.
	plain := git{dir: testRepo(t), timeout: time.Minute, log: func(string) {}}
	s = &syncer{cfg: config{repos: map[string]bool{canonical(parent): true}}}
	if skip, _ := s.skipSubmodule(plain); skip {
		t.Error("a repository that is not a submodule was skipped")
	}
}

// TestScanFindsBareRepositories shows that a bare repository under a
// directory is found, although it has no .git inside.
func TestScanFindsBareRepositories(t *testing.T) {
	root := t.TempDir()

	// A normal repository in a subdirectory.
	nested := filepath.Join(root, "a", "b")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, nested, "init", "-q", "-b", "main")

	// A bare repository in a subdirectory.
	bare := filepath.Join(root, "c", "d", "mirror.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, bare, "init", "--bare", "-q", "-b", "main")

	repos, bareRepos, hidden, err := scan(root, config{timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Errorf("the scan found %d hidden repositories", len(hidden))
	}
	if len(repos) != 1 || repos[0] != nested {
		t.Errorf("the scan found %v, want [%s]", repos, nested)
	}
	if len(bareRepos) != 1 || bareRepos[0] != bare {
		t.Errorf("the scan found the bare repositories %v, want [%s]", bareRepos, bare)
	}
}

// TestIsBareRepoDir shows which directories are taken for a bare repository.
func TestIsBareRepoDir(t *testing.T) {
	dir := t.TempDir()

	// A plain directory is not one.
	if isBareRepoDir(dir) {
		t.Error("a plain directory was taken for a bare repository")
	}

	// A directory with the layout of a git directory is a candidate.
	gitDir := filepath.Join(dir, "mirror.git")
	if err := os.MkdirAll(filepath.Join(gitDir, "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(gitDir, "refs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !isBareRepoDir(gitDir) {
		t.Error("a directory with the layout of a git directory was not taken for a bare repository")
	}

	// A working tree is not one, even when it holds a .git.
	work := filepath.Join(dir, "work")
	if err := os.MkdirAll(filepath.Join(work, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if isBareRepoDir(work) {
		t.Error("a working tree was taken for a bare repository")
	}
}

// TestDropGitDirs shows that a git directory outside of its working tree is
// not synced as a repository of its own.
func TestDropGitDirs(t *testing.T) {
	root := t.TempDir()

	// A repository that keeps its metadata outside of the working tree.
	shared := filepath.Join(root, "shared.git")
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}
	testGit(t, work, "init", "-q", "--separate-git-dir="+shared, "-b", "main")

	repos, bare, _, err := scan(root, config{timeout: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || repos[0] != work {
		t.Errorf("the scan found %v, want [%s]", repos, work)
	}
	// git knows the working tree the git directory belongs to, so it is not
	// a bare repository.
	if len(bare) != 0 {
		t.Errorf("the scan found the bare repositories %v, want none", bare)
	}
	// And even when it is taken for one, the git directory of a working tree
	// is dropped: the working tree that points at it is synced.
	if kept := dropGitDirs([]string{shared}, repos, time.Minute); len(kept) != 0 {
		t.Errorf("the git directory of a working tree was kept: %v", kept)
	}
}

func touch(t *testing.T, path string) {
	t.Helper()
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		t.Fatal(err)
	}
}
func modTime(t *testing.T, path string) time.Time {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return st.ModTime()
}
