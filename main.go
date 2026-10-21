// ggg — Git Group Sync.
//
// ggg finds every git repository under a directory tree and synchronises each
// one with its remote. The guiding rule is:
//
//	fetch as much of the remote as possible, while keeping every byte of local
//	work (uncommitted changes, untracked files and local commits).
//
// Per repository, ggg:
//
//  1. Detects an unfinished merge/rebase/cherry-pick and refuses to touch the
//     repository (or aborts it with -recover).
//  2. Fetches the remote with --prune. If the remote repository itself is
//     gone (deleted, renamed, no longer visible), the local copy is all that
//     is left: the repository is reported as local-only and left completely
//     untouched - nothing is synced into it and nothing is renamed.
//  3. If the working tree is dirty, stashes it (including untracked files)
//     before touching history and pops it afterwards.
//  4. On the default branch (main/master): fast-forwards when possible, and
//     merges (never resets) when the branch has diverged, so local commits
//     survive.
//  5. On any other branch: merges the branch's own upstream and then
//     origin/main into it. When a merge conflicts, ggg retries with an
//     automatic resolution strategy (-X theirs by default, i.e. the remote
//     wins on conflicting hunks) so the remote history still lands locally.
//     If even that fails, ggg aborts the merge, switches to the default
//     branch, syncs it, and leaves the feature branch intact with its local
//     commits.
//  6. In a bare repository (no working tree): fetches into the
//     remote-tracking branches and fast-forwards the local branches that the
//     remote contains. A branch with commits of its own is left alone - a
//     bare repository cannot merge.
//
// ggg never force-pushes, never resets --hard over commits, and never drops a
// stash that did not apply cleanly.
package main

import (
	"bytes"
	"context"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const progName = "ggg"

// version is the release version. It is overridden at build time with
// -ldflags "-X main.version=1.2.3".
var version = "1.6.0"

// ---------------------------------------------------------------------------
// configuration
// ---------------------------------------------------------------------------

type config struct {
	root           string
	remote         string
	candidates     []string
	jobs           int
	dryRun         bool
	autoResolve    []string
	mergeUptr      bool
	push           bool
	submodules     bool
	recover        bool
	listOnly       bool
	verbose        bool
	switchDetached bool
	skipSubmodules bool
	syncSubmodules bool
	// repos holds the repositories of this run, keyed by their canonical
	// path. A submodule whose parent is in the set is synced with its
	// parent, not on its own.
	repos    map[string]bool
	skipRE   *regexp.Regexp
	maxDepth int
	timeout  time.Duration
	// followSymlinks makes the scan walk symlinked directories too.
	followSymlinks bool
}

func (c *config) resolveStrategies() []string {
	var out []string
	for _, s := range c.autoResolve {
		s = strings.TrimSpace(s)
		switch s {
		case "", "none":
			continue
		case "theirs", "ours", "union":
			out = append(out, s)
		default:
			fmt.Fprintf(os.Stderr, "%s: ignoring unknown auto-resolve strategy %q\n", progName, s)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// result model
// ---------------------------------------------------------------------------

type status int

const (
	statusOK status = iota
	statusWarn
	statusFail
	statusSkip
)

func (s status) label() string {
	switch s {
	case statusOK:
		return "OK"
	case statusWarn:
		return "WARN"
	case statusFail:
		return "FAIL"
	case statusSkip:
		return "SKIP"
	}
	return "?"
}

type result struct {
	repo     string
	st       status
	branch   string
	summary  string
	details  []string
	duration time.Duration
}

func (r *result) note(format string, args ...any) {
	r.details = append(r.details, fmt.Sprintf(format, args...))
}

// restoreResult describes what happened to a hidden repository.
type restoreResult struct {
	path     string
	restored bool
	summary  string
}

// ---------------------------------------------------------------------------
// git plumbing
// ---------------------------------------------------------------------------

type git struct {
	dir     string
	timeout time.Duration
	dryRun  bool
	mu      *sync.Mutex
	log     func(string)
	// gitDirOverride runs the commands against this git directory instead
	// of the one in dir. It is used for the metadata of a hidden
	// repository, which is not reachable as a repository any more.
	gitDirOverride string
}

// prefix returns the git options that select the repository.
func (g git) prefix() []string {
	p := []string{"-C", g.dir}
	if g.gitDirOverride != "" {
		p = append(p, "--git-dir", g.gitDirOverride)
	}
	return p
}

// readOnlyEnv is the environment of a read-only git command.
//
// GIT_OPTIONAL_LOCKS=0 tells git not to take the optional index lock: a
// command that only looks at a repository then never writes the index and
// never blocks - and is never blocked by - another git process that is
// writing the very same index. This matters for submodules: a status in the
// parent repository runs a status inside the submodule, which would take the
// submodule's index lock.
var readOnlyEnv = []string{"GIT_OPTIONAL_LOCKS=0"}

// execGit runs a git command in the repository and returns its stdout.
//
// extraEnv is added to the environment of the command.
func (g git) execGit(extraEnv []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append(g.prefix(), args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Env = append(cmd.Env, extraEnv...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = strings.TrimSpace(stdout.String())
		}
		if msg == "" {
			msg = err.Error()
		}
		if ctx.Err() == context.DeadlineExceeded {
			msg = "timed out after " + g.timeout.String()
		}
		return stdout.String(), fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return stdout.String(), nil
}

func (g git) run(args ...string) error {
	_, err := g.execGit(nil, args...)
	return err
}

// runRead runs a command that only looks at the repository.
func (g git) runRead(args ...string) error {
	_, err := g.execGit(readOnlyEnv, args...)
	return err
}

// mutate runs a command that changes the repository. In dry-run mode the
// command is printed instead of executed, so read-only inspection still
// reports the real state of the repository.
func (g git) mutate(args ...string) error {
	if g.dryRun {
		g.log("  would run: git " + strings.Join(args, " "))
		return nil
	}
	var err error
	for attempt := 0; attempt <= len(lockDelays); attempt++ {
		if attempt > 0 {
			g.log("  waiting " + lockDelays[attempt-1].String() + " for the lock to free up ...")
			time.Sleep(lockDelays[attempt-1])
		}
		err = g.run(args...)
		if err == nil {
			return nil
		}
		path, locked := lockContention(err)
		if !locked {
			return err
		}
		if attempt == len(lockDelays) {
			return lockHeld(path, err)
		}
	}
	return err
}

// output runs a command and returns its trimmed stdout.
//
// The command is read-only, so it runs without the optional index lock.
func (g git) output(args ...string) (string, error) {
	out, err := g.execGit(readOnlyEnv, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimRight(out, "\n"), nil
}

// ok reports whether a command succeeds.
func (g git) ok(args ...string) bool {
	return g.runRead(args...) == nil
}

// lockDelays is how long ggg waits before it tries again when another git
// process holds a lock file.
var lockDelays = []time.Duration{500 * time.Millisecond, 1500 * time.Millisecond}

// lockFileRE finds the lock file in the message of a failed git command.
var lockFileRE = regexp.MustCompile(`Unable to create '([^']+)': File exists`)

// lockContention reports whether a failed git command lost the race for a
// lock file, and returns the path of the lock.
func lockContention(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	msg := err.Error()
	if m := lockFileRE.FindStringSubmatch(msg); m != nil {
		return m[1], true
	}
	// A reference that already exists is not a lock another process holds:
	// git words it like a failed lock ("cannot lock ref ...: reference
	// already exists"), but no one has to release anything.
	if strings.Contains(msg, "reference already exists") {
		return "", false
	}
	switch {
	case strings.Contains(msg, "index.lock"),
		strings.Contains(msg, "packed-refs.lock"),
		strings.Contains(msg, "could not write index"),
		strings.Contains(msg, "cannot lock ref"),
		strings.Contains(msg, "Unable to create") && strings.Contains(msg, ".lock"):
		return "", true
	}
	return "", false
}

// lockHeld describes a lock file that another process holds.
func lockHeld(path string, err error) error {
	if path == "" {
		return fmt.Errorf("another git process is using this repository, try again later: %w", err)
	}
	return fmt.Errorf("another git process is using this repository (lock %s), try again later: %w", path, err)
}

func (g git) gitDir() string {
	out, err := g.output("rev-parse", "--absolute-git-dir")
	if err != nil {
		return ""
	}
	return out
}

// refExists reports whether a commit-ish resolves.
func (g git) refExists(ref string) bool {
	return g.runRead("rev-parse", "--verify", "--quiet", ref+"^{commit}") == nil
}

// isAncestor reports whether a is an ancestor of b (b contains a).
func (g git) isAncestor(a, b string) bool {
	return g.runRead("merge-base", "--is-ancestor", a, b) == nil
}

func (g git) branchExists(name string) bool {
	return g.runRead("show-ref", "--verify", "--quiet", "refs/heads/"+name) == nil
}

// unmerged reports the number of conflicted paths in the index.
func (g git) unmerged() int {
	out, err := g.output("ls-files", "--unmerged")
	if err != nil {
		return 0
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return 0
	}
	return len(strings.Split(out, "\n"))
}

func (g git) hasCommits() bool {
	return g.runRead("rev-parse", "--verify", "--quiet", "HEAD") == nil
}

// isBare reports whether the repository has no working tree.
func (g git) isBare() bool {
	out, err := g.output("rev-parse", "--is-bare-repository")
	return err == nil && strings.TrimSpace(out) == "true"
}

// remoteBranchSHAs lists the branches of the remote with their commit.
//
// A bare repository is often a mirror: it has no remote-tracking branches, so
// the remote itself is asked.
func (g git) remoteBranchSHAs(remote string) map[string]string {
	out, err := g.output("ls-remote", "--heads", remote)
	if err != nil {
		return nil
	}
	shas := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		shas[strings.TrimPrefix(fields[1], "refs/heads/")] = fields[0]
	}
	return shas
}

// localBranches lists the branches of the repository, without the
// refs/heads/ prefix.
func (g git) localBranches() []string {
	out, err := g.output("for-each-ref", "--format=%(refname)", "refs/heads")
	if err != nil {
		return nil
	}
	var names []string
	for _, l := range strings.Split(out, "\n") {
		l = strings.TrimSpace(l)
		if name := strings.TrimPrefix(l, "refs/heads/"); name != "" && name != l {
			names = append(names, name)
		}
	}
	return names
}

// inProgressOp names the unfinished git operation, if any.
func (g git) inProgressOp() string {
	dir := g.gitDir()
	if dir == "" {
		return ""
	}
	checks := []struct{ path, name string }{
		{"MERGE_HEAD", "merge"},
		{"rebase-merge", "rebase"},
		{"rebase-apply", "rebase/apply"},
		{"CHERRY_PICK_HEAD", "cherry-pick"},
		{"REVERT_HEAD", "revert"},
	}
	for _, c := range checks {
		if _, err := os.Stat(filepath.Join(dir, c.path)); err == nil {
			return c.name
		}
	}
	return ""
}

// remotes lists the configured remotes.
func (g git) remotes() []string {
	out, err := g.output("remote")
	if err != nil {
		return nil
	}
	var rs []string
	for _, l := range strings.Split(out, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			rs = append(rs, l)
		}
	}
	return rs
}

// currentBranch returns the checked-out branch, or "" when detached.
func (g git) currentBranch() string {
	if name, err := g.output("symbolic-ref", "--quiet", "--short", "HEAD"); err == nil && name != "" {
		return name
	}
	return ""
}

// superproject returns the working tree of the repository that has this
// repository as a submodule, or "" when it is a standalone repository.
func (g git) superproject() string {
	out, err := g.output("rev-parse", "--show-superproject-working-tree")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

func (g git) upstreamOf(branch string) string {
	if branch == "" {
		return ""
	}
	out, err := g.output("rev-parse", "--abbrev-ref", "--symbolic-full-name", branch+"@{upstream}")
	if err != nil {
		return ""
	}
	return out
}

func (g git) isDirty() bool {
	out, err := g.output("status", "--porcelain")
	if err != nil {
		return false
	}
	return strings.TrimSpace(out) != ""
}

// stashRef returns the SHA of the top of the stash stack, or "" when the
// stack is empty.
func (g git) stashRef() string {
	sha, err := g.output("rev-parse", "--verify", "--quiet", "refs/stash")
	if err != nil {
		return ""
	}
	return sha
}

// stash pushes the working tree (including untracked files) onto the stash
// stack and returns the SHA of the new stash entry.
//
// An empty SHA means "nothing was stashed": git does not record submodule
// pointer changes, so a tree whose only modification is a moved submodule
// stays dirty and produces no stash entry at all. git stash push still exits
// successfully in that case, so this is not an error.
func (g git) stash(message string) (string, error) {
	before := g.stashRef()
	if err := g.mutate("stash", "push", "--include-untracked", "-m", message); err != nil {
		return "", err
	}
	if g.dryRun {
		return "", nil
	}
	after := g.stashRef()
	if after == "" || after == before {
		// No new entry: either there was nothing to stash or the stash stack
		// was left untouched. Never guess - the old top of the stack is
		// somebody else's stash and must not be applied or dropped.
		return "", nil
	}
	return after, nil
}

// restoreStash re-applies a stash entry and drops it only when the apply
// succeeded. A failed apply keeps the stash entry intact.
func (g git) restoreStash(sha string) error {
	if sha == "" {
		return nil
	}
	if err := g.mutate("stash", "apply", sha); err != nil {
		// Clear the unmerged index entries but leave the working tree alone:
		// the stash still holds every change, so nothing is lost.
		_ = g.mutate("reset")
		return fmt.Errorf("stash apply conflicted (stash kept: %s); the working tree may show conflict "+
			"markers - run `git checkout -- .` and then `git stash pop` to retry: %w", shortSHA(sha), err)
	}
	if idx, ok := g.stashIndex(sha); ok {
		_ = g.mutate("stash", "drop", "stash@{"+strconv.Itoa(idx)+"}")
	}
	return nil
}

// stashIndex returns the position of a stash SHA inside the stash stack.
func (g git) stashIndex(sha string) (int, bool) {
	out, err := g.output("stash", "list", "--format=%H")
	if err != nil {
		return 0, false
	}
	for i, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == sha {
			return i, true
		}
	}
	return 0, false
}

// defaultBranch picks the branch that represents the remote's mainline.
func (g git) defaultBranch(remote string, candidates []string) string {
	// 1. origin/HEAD
	if out, err := g.output("symbolic-ref", "--quiet", "refs/remotes/"+remote+"/HEAD"); err == nil {
		if name := strings.TrimPrefix(out, "refs/remotes/"+remote+"/"); name != "" {
			return name
		}
	}
	// 2. candidate names present on the remote
	var remoteBranches []string
	if out, err := g.output("for-each-ref", "--format=%(refname:short)", "refs/remotes/"+remote); err == nil {
		for _, l := range strings.Split(out, "\n") {
			l = strings.TrimSpace(l)
			if l == "" || strings.HasSuffix(l, "/HEAD") {
				continue
			}
			remoteBranches = append(remoteBranches, strings.TrimPrefix(l, remote+"/"))
		}
	}
	has := func(name string) bool {
		for _, b := range remoteBranches {
			if b == name {
				return true
			}
		}
		return false
	}
	for _, c := range candidates {
		if has(c) {
			return c
		}
	}
	// 3. candidate names present locally
	for _, c := range candidates {
		if g.branchExists(c) {
			return c
		}
	}
	// 4. any remote branch, first alphabetically
	if len(remoteBranches) > 0 {
		sort.Strings(remoteBranches)
		return remoteBranches[0]
	}
	return ""
}

// ---------------------------------------------------------------------------
// gone remotes and hidden repositories
// ---------------------------------------------------------------------------

// goneRemotePhrases are the error fragments git prints when the remote
// repository itself no longer exists: it was deleted, renamed, or is not
// visible to this user any more.
//
// Authentication and network failures are deliberately absent: in those cases
// the repository is still alive and must keep being synced.
var goneRemotePhrases = []string{
	"repository not found",
	"repo not found",
	"repository does not exist",
	"remote repository does not exist",
	"does not appear to be a git repository",
	"requested url returned error: 404",
	"the project you were looking for could not be found",
}

// isGoneRemote reports whether err means that the remote repository is gone.
func isGoneRemote(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, p := range goneRemotePhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	// "fatal: repository 'https://host/user/name.git/' not found"
	if strings.Contains(msg, "not found") &&
		(strings.Contains(msg, "repository") || strings.Contains(msg, "repo") || strings.Contains(msg, "project")) {
		return true
	}
	return false
}

// hiddenGitDir returns the git directory the hidden metadata points at.
//
// For a normal clone that is the .git_old directory itself. For a worktree or
// a submodule the .git_old file points at the real git directory.
func hiddenGitDir(path, old string, st os.FileInfo) (string, error) {
	if st.IsDir() {
		abs, err := filepath.Abs(old)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	data, err := os.ReadFile(old)
	if err != nil {
		return "", fmt.Errorf("cannot read .git_old: %w", err)
	}
	line := strings.TrimSpace(string(data))
	if !strings.HasPrefix(line, "gitdir:") {
		return "", fmt.Errorf("unexpected .git_old content: %q", line)
	}
	dir := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(path, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		return "", fmt.Errorf("the git directory %s is gone", dir)
	}
	return dir, nil
}

// restoreHidden brings a hidden repository back when its remote answers
// again.
//
// The .git_old metadata is renamed back to .git and the repository is synced
// like any other one. Nothing happens while the remote is still gone or
// cannot be reached: the repository stays hidden.
//
// Hidden repositories are a leftover of ggg 1.1, which renamed the metadata of
// a repository whose remote was gone. ggg does not hide anything any more: a
// repository whose remote is gone stays a normal, local-only repository.
func (s *syncer) restoreHidden(path string) (bool, string) {
	old := filepath.Join(path, ".git_old")
	st, err := os.Lstat(old)
	if err != nil {
		return false, "cannot read .git_old: " + err.Error()
	}
	gitDir, err := hiddenGitDir(path, old, st)
	if err != nil {
		return false, err.Error()
	}

	g := git{
		dir:            path,
		gitDirOverride: gitDir,
		timeout:        s.cfg.timeout,
		dryRun:         s.cfg.dryRun,
	}
	remotes := g.remotes()
	if len(remotes) == 0 {
		return false, "no remote configured"
	}

	// The repository is back as soon as one of its remotes answers again.
	var lastErr error
	for _, r := range remotes {
		lastErr = g.runRead("ls-remote", r)
		if lastErr != nil {
			continue
		}
		if s.cfg.dryRun {
			return false, "remote " + r + " is back; would rename .git_old back to .git and sync it"
		}
		if err := os.Rename(old, filepath.Join(path, ".git")); err != nil {
			return false, "could not rename .git_old back to .git: " + err.Error()
		}
		return true, "remote " + r + " is back; .git_old renamed back to .git"
	}
	if isGoneRemote(lastErr) {
		return false, "the remote is still gone"
	}
	return false, "the remote could not be reached: " + lastErr.Error()
}

// ---------------------------------------------------------------------------
// discovery
// ---------------------------------------------------------------------------

var defaultSkipDirs = map[string]bool{
	".git":          true,
	".git_old":      true,
	"node_modules":  true,
	"__pycache__":   true,
	".venv":         true,
	"venv":          true,
	"site-packages": true,
	".mypy_cache":   true,
	".pytest_cache": true,
	".tox":          true,
	".cache":        true,
	".gradle":       true,
	".terraform":    true,
	".idea":         true,
	".vscode":       true,
}

// reportEmpty explains that no repository was found, and what was not
// looked at, so that an empty result is not a dead end.
//
// The message goes to stderr: the list of repositories stays readable.
func reportEmpty(root string, cfg config) {
	fmt.Fprintf(os.Stderr, "%s: no git repositories found under %s\n", progName, root)
	if !cfg.followSymlinks {
		fmt.Fprintf(os.Stderr, "%s: symlinked directories are not walked (use -follow-symlinks)\n", progName)
	}
	fmt.Fprintf(os.Stderr, "%s: %s and similar directories are skipped\n", progName, strings.Join(skipDirNames(), ", "))
}

// skipDirNames lists the directory names that are not walked, for the
// message that explains an empty result.
func skipDirNames() []string {
	names := make([]string, 0, len(defaultSkipDirs))
	for name := range defaultSkipDirs {
		if name == ".git" || name == ".git_old" {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// isRepoDir reports whether dir is the top level of a git repository.
func isRepoDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	// A directory for normal clones, a file for worktrees and submodules.
	return st.IsDir() || st.Mode().IsRegular()
}

// isBareRepoDir reports whether dir looks like the git directory of a bare
// repository: a directory with the layout of a git directory (HEAD, objects
// and refs) and no working tree of its own.
//
// A bare repository has no .git inside, so it is found by its layout instead.
// The layout is only a hint - isBareRepository confirms the candidates with
// git.
func isBareRepoDir(dir string) bool {
	if isRepoDir(dir) {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, "HEAD"))
	if err != nil || !st.Mode().IsRegular() {
		return false
	}
	for _, sub := range []string{"objects", "refs"} {
		st, err := os.Stat(filepath.Join(dir, sub))
		if err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

// isBareRepository confirms with git that dir is a bare repository.
func isBareRepository(dir string, timeout time.Duration) bool {
	g := git{dir: dir, timeout: timeout}
	out, err := g.output("rev-parse", "--is-bare-repository")
	return err == nil && strings.TrimSpace(out) == "true"
}

// walker walks a directory tree and calls fn for every directory it finds.
//
// A symlinked directory is walked only when follow is set. The walk keeps
// track of the directories it has already been in - by their real path, so
// that a symlink leading back into the tree, or to a directory that was
// walked already, cannot send it into a loop and cannot make it report the
// same repository twice.
type walker struct {
	follow bool
	seen   map[string]bool
	// root is the directory the walk started in, rootReal its real path.
	root     string
	rootReal string
}

// realPath returns the real path of a directory below base.
//
// base is the real path of the directory the walk is in, so the real path of
// a directory below it is the base and its name: no symbolic link is
// resolved, which keeps the walk cheap.
func realPath(base, name string) string {
	return filepath.Join(base, name)
}

// walk calls fn for the directory dir and for every directory below it.
//
// fn is called with the path of the directory and the entry it was found
// as. It can return fs.SkipDir to keep the walk from descending into it.
func (w *walker) walk(dir, realBase string, fn func(path string, d fs.DirEntry) error) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		// Unreadable directory: skip it rather than aborting the scan.
		return nil
	}
	for _, e := range entries {
		path := filepath.Join(dir, e.Name())
		switch {
		case e.IsDir():
			real := realPath(realBase, e.Name())
			if w.seen[real] {
				continue
			}
			w.seen[real] = true
			if err := fn(path, e); err != nil {
				if err == fs.SkipDir {
					continue
				}
				return err
			}
			if err := w.walk(path, real, fn); err != nil {
				return err
			}
		case e.Type()&fs.ModeSymlink != 0 && w.follow:
			// A symlink to a directory: walk it, unless it leads somewhere
			// the walk has been already.
			info, err := os.Stat(path)
			if err != nil || !info.IsDir() {
				continue
			}
			real := canonical(path)
			if w.seen[real] {
				continue
			}
			w.seen[real] = true
			if err := fn(path, e); err != nil {
				if err == fs.SkipDir {
					continue
				}
				return err
			}
			if err := w.walk(path, real, fn); err != nil {
				return err
			}
		}
	}
	return nil
}

// hasGitOld reports whether dir holds the metadata of a hidden repository: a
// .git_old without a .git.
func hasGitOld(dir string) bool {
	st, err := os.Lstat(filepath.Join(dir, ".git_old"))
	if err != nil {
		return false
	}
	return st.IsDir() || st.Mode().IsRegular()
}

// skipWalkDir reports whether the walk must not descend into path.
func skipWalkDir(path, root string, d fs.DirEntry, cfg config) bool {
	if path == root {
		return false
	}
	name := d.Name()
	if defaultSkipDirs[name] {
		return true
	}
	// The metadata of a hidden repository and the archived copies of it.
	if strings.HasPrefix(name, ".git_old") {
		return true
	}
	if cfg.skipRE != nil && cfg.skipRE.MatchString(path) {
		return true
	}
	if cfg.maxDepth > 0 {
		rel, err := filepath.Rel(root, path)
		if err == nil {
			depth := len(strings.Split(rel, string(os.PathSeparator)))
			if depth > cfg.maxDepth {
				return true
			}
		}
	}
	return false
}

// scan walks the tree once and returns the git repositories it finds, the
// bare repositories it finds and the hidden ones: directories that hold a
// .git_old but no .git, i.e. repositories whose remote was gone.
func scan(root string, cfg config) (repos, bare, hidden []string, err error) {
	abs, err := scanRoot(root)
	if err != nil {
		return nil, nil, nil, err
	}

	w := &walker{
		follow:   cfg.followSymlinks,
		seen:     map[string]bool{},
		root:     abs,
		rootReal: canonical(abs),
	}
	w.seen[w.rootReal] = true

	visit := func(path string, d fs.DirEntry) error {
		if skipWalkDir(path, abs, d, cfg) {
			return fs.SkipDir
		}
		switch {
		case isRepoDir(path):
			repos = append(repos, path)
		case hasGitOld(path):
			hidden = append(hidden, path)
		case isBareRepoDir(path):
			// A bare repository has no working tree, so there is nothing
			// to find inside it.
			if isBareRepository(path, cfg.timeout) {
				bare = append(bare, path)
				return fs.SkipDir
			}
		}
		return nil
	}

	// The root itself is a directory of the tree.
	if info := mustStat(abs); info != nil {
		rootEntry := fs.FileInfoToDirEntry(info)
		if err := visit(abs, rootEntry); err != nil {
			return repos, bare, hidden, err
		}
	}
	if err := w.walk(abs, w.rootReal, visit); err != nil {
		return repos, bare, hidden, err
	}
	sort.Strings(repos)
	sort.Strings(bare)
	sort.Strings(hidden)
	return repos, bare, hidden, nil
}

// mustStat returns the information about a path that is known to exist.
func mustStat(path string) os.FileInfo {
	info, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return info
}

// dropGitDirs removes the bare candidates that are the git directory of a
// repository that was found as well.
//
// A repository can keep its metadata outside of its working tree (git init
// --separate-git-dir). Such a git directory is not a repository of its own:
// the working tree that points at it is. Syncing both would write the same
// references twice.
func dropGitDirs(bare, repos []string, timeout time.Duration) []string {
	if len(bare) == 0 || len(repos) == 0 {
		return bare
	}
	gitDirs := make(map[string]bool, len(repos))
	for _, r := range repos {
		g := git{dir: r, timeout: timeout}
		out, err := g.output("rev-parse", "--absolute-git-dir")
		if err != nil {
			continue
		}
		gitDirs[canonical(out)] = true
	}
	kept := make([]string, 0, len(bare))
	for _, b := range bare {
		if gitDirs[canonical(b)] {
			continue
		}
		kept = append(kept, b)
	}
	return kept
}

// ---------------------------------------------------------------------------
// syncer
// ---------------------------------------------------------------------------

type syncer struct {
	cfg config
}

// skipSubmodule reports whether a repository must be left alone because it is
// a submodule of another repository.
//
// The commit a submodule points at belongs to the parent: the parent decides
// which commit is checked out. Syncing the submodule on its own therefore
// fights the parent - and both write the same index, which is how a parallel
// run ends up in "Unable to create index.lock: File exists".
//
// A submodule whose parent is part of the same run is therefore skipped.
// -skip-submodules leaves every submodule alone, also the ones whose parent
// is not part of the run, and -sync-submodules syncs them all.
func (s *syncer) skipSubmodule(g git) (bool, string) {
	super := g.superproject()
	if super == "" {
		return false, ""
	}
	if s.cfg.syncSubmodules {
		return false, ""
	}
	if s.cfg.skipSubmodules {
		return true, "submodule of " + filepath.Base(super) + "; skipped"
	}
	if s.cfg.repos[canonical(super)] {
		return true, "submodule of " + filepath.Base(super) + "; skipped (synced with its parent)"
	}
	return false, ""
}

// canonical returns the path that identifies a location: the absolute path
// with symbolic links resolved, so that two ways of naming the same
// directory compare equal.
func canonical(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		abs = path
	}
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		return resolved
	}
	return abs
}

func (s *syncer) syncOne(path string) result {
	start := time.Now()
	res := result{repo: path, st: statusOK}

	var logMu sync.Mutex
	g := git{
		dir:     path,
		timeout: s.cfg.timeout,
		dryRun:  s.cfg.dryRun,
		mu:      &logMu,
		log: func(msg string) {
			logMu.Lock()
			defer logMu.Unlock()
			if s.cfg.verbose || s.cfg.dryRun {
				fmt.Printf("%s%s\n", indent(path, s.cfg.root), msg)
			}
		},
	}

	// --- 0. sanity checks ---------------------------------------------------
	// A bare repository has no working tree: it is synced on its own terms.
	bare := g.isBare()
	if op := g.inProgressOp(); op != "" {
		if !s.cfg.recover {
			return res.skip(start, "unfinished "+op+" in progress")
		}
		switch op {
		case "merge":
			_ = g.mutate("merge", "--abort")
		case "rebase", "rebase/apply":
			_ = g.mutate("rebase", "--abort")
		case "cherry-pick":
			_ = g.mutate("cherry-pick", "--abort")
		case "revert":
			_ = g.mutate("revert", "--abort")
		}
		if op = g.inProgressOp(); op != "" {
			return res.skip(start, "unfinished "+op+" in progress (could not abort)")
		}
		res.note("aborted unfinished %s to continue the sync", op)
	}

	// --- 1. remote ----------------------------------------------------------
	remote := s.resolveRemote(g)
	if remote == "" {
		return res.skip(start, "no remote configured")
	}

	// The commit a submodule points at belongs to the parent repository, not
	// to us: leave those repositories alone unless asked otherwise.
	if skip, reason := s.skipSubmodule(g); skip {
		return res.skip(start, reason)
	}

	// --- 2. fetch -----------------------------------------------------------
	fetchFailed := false
	if s.cfg.dryRun {
		// The planned fetch is not executed, so the remote is probed with a
		// read-only ls-remote: the plan has to know whether the remote is
		// still there.
		if _, err := g.output("ls-remote", "--exit-code", remote); err != nil && isGoneRemote(err) {
			return s.localOnly(start, &res, err)
		}
	}
	fetchArgs := []string{"fetch", "--prune", remote}
	if bare {
		// A bare repository is often a mirror: its configured refspec writes
		// the remote branches straight into refs/heads, and it is forced (+),
		// so a fetch would throw away local commits. ggg fetches into the
		// remote-tracking branches instead and moves the local branches
		// itself - but only when that is a fast-forward.
		fetchArgs = []string{"fetch", "--prune", remote, "+refs/heads/*:refs/remotes/" + remote + "/*"}
	}
	if err := g.mutate(fetchArgs...); err != nil {
		fetchFailed = true
		res.st = statusWarn
		res.note("fetch failed: %v", err)
		if isGoneRemote(err) {
			// The remote repository itself is gone: the local copy is the
			// only one that is left. Nothing is synced and nothing is
			// changed - the repository is left exactly as it is.
			return s.localOnly(start, &res, err)
		}
	}

	if bare {
		return s.syncBareRepo(g, remote, &res, start)
	}

	// --- 3. branch layout ---------------------------------------------------
	branch := g.currentBranch()
	res.branch = branch
	detached := branch == ""

	defBranch := g.defaultBranch(remote, s.cfg.candidates)
	if defBranch == "" {
		return res.skip(start, "no default branch found on "+remote)
	}
	defRef := remote + "/" + defBranch
	if !g.refExists(defRef) {
		return res.skip(start, "remote branch "+defRef+" does not exist")
	}

	// A repository without any commit cannot be merged or stashed.
	if !g.hasCommits() {
		if g.isDirty() {
			return res.skip(start, "repository has no commits yet and has local changes")
		}
		// Move to the default branch without touching any existing history:
		// an existing local branch is only checked out, a missing one is
		// created from the remote.
		if g.branchExists(defBranch) {
			if err := g.mutate("checkout", defBranch); err != nil {
				return res.fail(start, "could not switch to %s: %v", defBranch, err)
			}
		} else {
			if err := g.mutate("checkout", "-B", defBranch, "--track", defRef); err != nil {
				return res.fail(start, "could not create %s from %s: %v", defBranch, defRef, err)
			}
			res.branch = defBranch
			res.summary = "create " + defBranch + " from " + defRef
			if fetchFailed {
				res.st = statusWarn
			}
			return res.finish(start)
		}
		res.branch = defBranch
		res.summary = "switch to " + defBranch
		if fetchFailed {
			res.st = statusWarn
		}
		// The repository now has commits: continue with the normal flow.
		branch = defBranch
	}

	// --- 4. stash the working tree -----------------------------------------
	// This happens before any checkout so that a dirty tree can never block
	// a branch switch.
	stashSHA := ""
	if g.isDirty() {
		label := branch
		if label == "" {
			label = "detached HEAD"
		}
		msg := "ggg: pre-sync " + label + " " + time.Now().Format("2006-01-02 15:04:05")
		sha, err := g.stash(msg)
		if err != nil {
			return res.fail(start, "could not stash local changes: %v", err)
		}
		stashSHA = sha
		if sha != "" {
			res.note("stashed local changes as %s", shortSHA(sha))
		} else if g.isDirty() && !g.dryRun {
			// The tree is still dirty although the stash succeeded: the only
			// thing left is submodule pointer movement, which git cannot
			// stash. Those pointers are preserved on purpose.
			res.note("local changes are submodule pointers only; kept in place (git cannot stash them)")
		}
	}

	// --- 5. detached HEAD ---------------------------------------------------
	if detached {
		res.branch = "(detached)"
		// A detached HEAD is how submodules (and pinned checkouts) are
		// stored. Moving it changes the parent repository's pointer, so the
		// default is to only fetch and report.
		if !s.cfg.switchDetached {
			sha, _ := g.output("rev-parse", "--short", "HEAD")
			return res.skip(start, "detached HEAD at "+sha+"; fetched only")
		}
		if !g.isAncestor("HEAD", defRef) {
			n, _ := g.output("rev-list", "--count", defRef+"..HEAD")
			// The working tree was stashed above: put it back before giving up.
			if stashSHA != "" {
				if err := g.restoreStash(stashSHA); err != nil {
					res.st = statusWarn
					res.note("%v", err)
				}
			}
			return res.skip(start, "detached HEAD with "+strings.TrimSpace(n)+" local commit(s); left untouched")
		}
		res.note("detached HEAD was contained in " + defRef + "; switching to " + defBranch)
		if err := g.mutate("checkout", defBranch); err != nil {
			return res.fail(start, "could not switch to "+defBranch+": %v", err)
		}
		res.branch = defBranch
		branch = defBranch
	}

	// --- 6. sync ------------------------------------------------------------
	if branch == defBranch {
		res.summary = s.syncDefaultBranch(g, remote, defBranch, defRef, &res)
	} else {
		res.summary = s.syncFeatureBranch(g, remote, defBranch, defRef, branch, &res)
	}

	// --- 7. restore the working tree ---------------------------------------
	if stashSHA != "" {
		if err := g.restoreStash(stashSHA); err != nil {
			res.st = statusWarn
			res.note("%v", err)
		} else {
			res.note("restored local changes from %s", shortSHA(stashSHA))
		}
	}

	// --- 8. optional push ---------------------------------------------------
	if s.cfg.push && !s.cfg.dryRun {
		if up := g.upstreamOf(g.currentBranch()); up != "" {
			if err := g.mutate("push", remote, g.currentBranch()); err != nil {
				res.st = statusWarn
				res.note("push failed: %v", err)
			} else {
				res.note("pushed %s to %s", g.currentBranch(), up)
			}
		}
	}

	// --- 9. submodules ------------------------------------------------------
	if s.cfg.submodules {
		if err := g.mutate("submodule", "update", "--init", "--recursive"); err != nil {
			res.st = statusWarn
			res.note("submodule update failed: %v", err)
		}
	}

	return res.finish(start)
}

// syncBareRepo synchronises a bare repository.
//
// A bare repository has no working tree: there is nothing to stash and no
// merge commit can be created, so a local commit can never be reconciled with
// the remote - that would mean throwing it away. ggg therefore only moves a
// branch when the remote branch contains it (a fast-forward) and leaves every
// other branch exactly as it is.
func (s *syncer) syncBareRepo(g git, remote string, res *result, start time.Time) result {
	branch := g.currentBranch()
	if branch == "" {
		return res.skip(start, "detached HEAD; fetched only")
	}
	res.branch = branch

	defBranch := g.defaultBranch(remote, s.cfg.candidates)
	if defBranch == "" {
		return res.skip(start, "no default branch found on "+remote)
	}

	// The branch the HEAD points at does not exist yet: the repository is
	// empty (or only has other branches). Create it from the remote, so that
	// the repository has a valid HEAD.
	if !g.branchExists(branch) {
		source := remote + "/" + branch
		if !g.refExists(source) {
			source = remote + "/" + defBranch
		}
		if !g.refExists(source) {
			return res.skip(start, "remote branch "+source+" does not exist")
		}
		sha, err := g.output("rev-parse", source)
		if err != nil {
			return res.fail(start, "could not read %s: %v", source, err)
		}
		if err := g.mutate("-c", "core.logAllRefUpdates=true", "update-ref", "-m",
			"ggg: create "+branch+" from "+source, "refs/heads/"+branch, sha, ""); err != nil {
			return res.fail(start, "could not create %s from %s: %v", branch, source, err)
		}
		res.summary = "create " + branch + " from " + source
		return res.finish(start)
	}

	// Every branch that the remote also has is fast-forwarded - but only
	// when the remote branch contains the local one.
	var moved, planned, blocked []string
	// The remote branches the local branches are compared with. They are
	// known locally after a fetch; a mirror that never fetched is asked
	// directly, so that the plan of a dry run is not empty.
	remoteSHAs := map[string]string{}
	if s.cfg.dryRun {
		remoteSHAs = g.remoteBranchSHAs(remote)
	}
	for _, b := range g.localBranches() {
		local := "refs/heads/" + b
		target := remote + "/" + b
		targetSHA, err := g.output("rev-parse", target)
		known := err == nil
		if !known {
			sha, ok := remoteSHAs[b]
			if !ok {
				// The remote does not have this branch: it is a local one.
				continue
			}
			targetSHA = sha
		}
		localSHA, err := g.output("rev-parse", local)
		if err != nil {
			continue
		}
		if localSHA == targetSHA {
			continue
		}
		if !known {
			// The commit of the remote branch is not in this repository, so
			// it cannot be checked whether the remote contains the local
			// branch: the plan says what is going to be tried.
			planned = append(planned, b)
			res.note("%s is at %s, the remote branch %s is at %s; it is fast-forwarded when the remote "+
				"contains it", local, shortSHA(localSHA), target, shortSHA(targetSHA))
			if s.cfg.dryRun {
				_ = g.mutate("-c", "core.logAllRefUpdates=true", "update-ref", "-m",
					"ggg: fast-forward to "+target, local, targetSHA, localSHA)
			}
			continue
		}
		if !g.isAncestor(local, target) {
			// The remote does not contain the local branch: it has commits
			// of its own, or the two have diverged. A bare repository cannot
			// merge them, so the branch stays where it is.
			blocked = append(blocked, b)
			res.note("%s was not moved: %s does not contain it and a bare repository cannot merge",
				local, target)
			continue
		}
		if err := g.mutate("-c", "core.logAllRefUpdates=true", "update-ref", "-m",
			"ggg: fast-forward to "+target, local, targetSHA, localSHA); err != nil {
			res.st = statusWarn
			res.note("could not fast-forward %s: %v", local, err)
			blocked = append(blocked, b)
			continue
		}
		moved = append(moved, b)
	}

	switch {
	case len(moved) == 0 && len(planned) == 0 && len(blocked) == 0:
		res.summary = "up to date"
	case len(moved) == 0 && len(planned) == 0:
		res.st = statusWarn
		res.summary = "not synced: " + remote + " does not contain the local branch"
	case len(moved) == 0:
		res.summary = "sync " + strings.Join(planned, ", ") + " with " + remote
	case len(moved) == 1:
		res.summary = "fast-forward to " + remote + "/" + moved[0]
	default:
		res.summary = "fast-forward " + strconv.Itoa(len(moved)) + " branches to " + remote
	}
	if len(blocked) > 0 {
		res.st = statusWarn
	}

	// --- optional push ------------------------------------------------------
	if s.cfg.push && !s.cfg.dryRun {
		if up := g.upstreamOf(branch); up != "" {
			if err := g.mutate("push", remote, branch); err != nil {
				res.st = statusWarn
				res.note("push failed: %v", err)
			} else {
				res.note("pushed %s to %s", branch, up)
			}
		}
	}

	return res.finish(start)
}

// localOnly reports a repository whose remote repository no longer exists.
//
// The local copy is the only one that is left, so there is nothing to sync:
// the repository is left exactly as it is - no fetch into it, no merge, no
// push and no renaming of its metadata. A repository that is local-only stays
// a normal repository, so it is still listed and it is still synced as soon as
// its remote is back.
func (s *syncer) localOnly(start time.Time, res *result, fetchErr error) result {
	res.note("%v", fetchErr)
	res.note("nothing was synced and nothing was changed")
	return res.skip(start, "remote repository is gone; only the local copy is left")
}

// resolveRemote picks the remote to sync from.
func (s *syncer) resolveRemote(g git) string {
	remotes := g.remotes()
	if len(remotes) == 0 {
		return ""
	}
	if s.cfg.remote != "" {
		for _, r := range remotes {
			if r == s.cfg.remote {
				return r
			}
		}
		// The requested remote does not exist; fall back to origin or the
		// only configured remote.
	}
	for _, r := range remotes {
		if r == "origin" {
			return "origin"
		}
	}
	if len(remotes) == 1 {
		return remotes[0]
	}
	return ""
}

// syncDefaultBranch updates the mainline branch of a repository.
func (s *syncer) syncDefaultBranch(g git, remote, defBranch, defRef string, res *result) string {
	switch {
	case g.isAncestor("HEAD", defRef):
		// The remote is strictly ahead: fast-forward.
		if err := g.mutate("merge", "--ff-only", defRef); err != nil {
			res.st = statusWarn
			res.note("fast-forward of %s failed: %v", defBranch, err)
			return "fast-forward failed"
		}
		return "fast-forward to " + defRef
	case g.isAncestor(defRef, "HEAD"):
		// Local is ahead of the remote; nothing to fetch.
		return "up to date (local is ahead of " + defRef + ")"
	default:
		// Diverged: merge so that local commits survive.
		mergeErr := g.mutate("merge", "--no-edit", defRef)
		if mergeErr == nil {
			res.note("merged %s into %s (branches had diverged)", defRef, defBranch)
			return "merge " + defRef + " (diverged)"
		}
		if g.unmerged() > 0 {
			_ = g.mutate("merge", "--abort")
			if resolved, strategy := s.autoResolveMerge(g, defRef); resolved {
				res.note("merged %s into %s, conflicts resolved with -X %s (the remote won the conflicting "+
					"hunks, your commit is still in the history)", defRef, defBranch, strategy)
				return "merge " + defRef + " (diverged, -X " + strategy + ")"
			}
			res.st = statusWarn
			res.note("%s and %s diverged and could not be merged automatically; local commits kept", defBranch, defRef)
			return "diverged from " + defRef
		}
		res.st = statusWarn
		res.note("merge of %s failed: %v", defRef, mergeErr)
		return "merge failed"
	}
}

// syncFeatureBranch pulls the mainline into a feature branch, falling back to
// the default branch when the merge cannot be resolved.
func (s *syncer) syncFeatureBranch(g git, remote, defBranch, defRef, branch string, res *result) string {
	// Targets: the branch's own upstream first (it is closer to the local
	// history), then the remote mainline.
	var targets []string
	if s.cfg.mergeUptr {
		if up := g.upstreamOf(branch); up != "" && up != defRef && g.refExists(up) {
			targets = append(targets, up)
		}
	}
	targets = append(targets, defRef)

	conflict := false
	for _, target := range targets {
		if !g.refExists(target) {
			res.note("%s does not exist, skipped", target)
			continue
		}
		if g.isAncestor(target, "HEAD") {
			res.note("%s is already contained in %s", target, branch)
			continue
		}
		mergeErr := g.mutate("merge", "--no-edit", target)
		if mergeErr == nil {
			res.note("merged %s into %s", target, branch)
			continue
		}
		if g.unmerged() == 0 {
			// Failed for a reason other than a content conflict.
			_ = g.mutate("merge", "--abort")
			res.st = statusWarn
			res.note("merge of %s failed: %v", target, mergeErr)
			continue
		}
		// Conflict: try the automatic resolution strategies.
		_ = g.mutate("merge", "--abort")
		if resolved, strategy := s.autoResolveMerge(g, target); resolved {
			res.note("merged %s into %s, conflicts resolved with -X %s (the remote won the conflicting "+
				"hunks, your version is still in the commit history)", target, branch, strategy)
			continue
		}
		conflict = true
		res.note("could not merge %s into %s automatically", target, branch)
		break
	}

	if !conflict {
		return "sync " + branch + " with " + defRef
	}

	// Fallback: keep the feature branch (and its local commits) intact, switch
	// to the mainline and sync that instead.
	n, _ := g.output("rev-list", "--count", defRef+".."+branch)
	localCommits := strings.TrimSpace(n)

	if err := s.checkoutBranch(g, defBranch, remote, defRef); err != nil {
		res.st = statusWarn
		res.note("could not switch to %s: %v", defBranch, err)
		return "could not merge " + defRef + " and could not switch to " + defBranch
	}
	res.branch = defBranch
	res.note("branch %s keeps %s local commit(s) that could not be merged automatically", branch, localCommits)
	res.note("switched to %s and synced it instead", defBranch)

	summary := s.syncDefaultBranch(g, remote, defBranch, defRef, res)
	return "could not merge " + defRef + " into " + branch + "; " + summary
}

// autoResolveMerge retries a conflicted merge with the configured strategies.
// It returns the strategy that worked, if any.
func (s *syncer) autoResolveMerge(g git, target string) (bool, string) {
	for _, strategy := range s.cfg.resolveStrategies() {
		if err := g.mutate("merge", "--no-edit", "-X", strategy, target); err == nil && g.unmerged() == 0 {
			return true, strategy
		}
		_ = g.mutate("merge", "--abort")
	}
	return false, ""
}

// checkoutBranch switches to a branch, creating it from the remote when the
// local branch does not exist yet.
func (s *syncer) checkoutBranch(g git, branch, remote, defRef string) error {
	if g.branchExists(branch) {
		return g.mutate("checkout", branch)
	}
	return g.mutate("checkout", "-b", branch, "--track", defRef)
}

func (r *result) skip(start time.Time, reason string) result {
	r.st = statusSkip
	r.summary = reason
	return r.finish(start)
}

func (r *result) fail(start time.Time, format string, args ...any) result {
	r.st = statusFail
	r.summary = fmt.Sprintf(format, args...)
	return r.finish(start)
}

func (r *result) finish(start time.Time) result {
	r.duration = time.Since(start)
	return *r
}

// ---------------------------------------------------------------------------
// output
// ---------------------------------------------------------------------------

var color = os.Getenv("NO_COLOR") == "" && isTerminal(os.Stdout)

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	if err != nil {
		return false
	}
	return st.Mode()&os.ModeCharDevice != 0
}

func paint(code, s string) string {
	if !color {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func statusPaint(st status) string {
	switch st {
	case statusOK:
		return paint("32", "OK  ")
	case statusWarn:
		return paint("33", "WARN")
	case statusFail:
		return paint("31", "FAIL")
	case statusSkip:
		return paint("90", "SKIP")
	}
	return "    "
}

// label renders the status of a repository. In dry-run mode every repository
// is shown as PLAN, because nothing actually happened.
func label(res result, dryRun bool) string {
	if dryRun {
		return paint("36", "PLAN")
	}
	return statusPaint(res.st)
}

func indent(path, root string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return path
	}
	return rel
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

// ---------------------------------------------------------------------------
// main
// ---------------------------------------------------------------------------

// parseArgs parses the flags and the optional directory argument.
//
// The flag package stops at the first argument that is not a flag, so the
// arguments are parsed again and again: everything that is not a flag is
// collected, and the flags behind it are still parsed.
//
//	ggg ~/src          scan ~/src
//	ggg ~/src -list    scan ~/src and only list the repositories
func parseArgs() (string, error) {
	var positional []string
	args := os.Args[1:]
	for len(args) > 0 {
		flag.CommandLine.Parse(args)
		rest := flag.CommandLine.Args()
		if len(rest) == 0 {
			break
		}
		positional = append(positional, rest[0])
		args = rest[1:]
	}
	switch len(positional) {
	case 0:
		return "", nil
	case 1:
		return positional[0], nil
	default:
		return "", fmt.Errorf("at most one directory can be given, got %d", len(positional))
	}
}

// scanRoot turns the directory to scan into an absolute path.
//
// A .git file belongs to the working tree next to it, so pointing at one
// scans that working tree. Anything else that is not a directory is an
// error: scanning the directory around it would be a surprise.
func scanRoot(root string) (string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return abs, nil
	}
	if filepath.Base(abs) == ".git" {
		return filepath.Dir(abs), nil
	}
	return "", fmt.Errorf("%s is not a directory", abs)
}

func main() {
	cfg := config{}

	flag.StringVar(&cfg.root, "root", ".", "directory to scan for git repositories")
	flag.StringVar(&cfg.remote, "remote", "origin", "remote to sync from")
	jobs := flag.Int("jobs", 0, "number of repositories to sync in parallel (0 = auto)")
	flag.BoolVar(&cfg.dryRun, "dry-run", false, "show what would be done without changing anything")
	autoResolve := flag.String("auto-resolve", "theirs", "comma separated conflict strategies tried in order (theirs,ours,union,none)")
	flag.BoolVar(&cfg.mergeUptr, "merge-upstream", true, "also merge the branch's own upstream into the branch")
	flag.BoolVar(&cfg.push, "push", false, "push the current branch to its upstream after syncing")
	flag.BoolVar(&cfg.submodules, "submodules", false, "run 'git submodule update --init --recursive' after syncing")
	flag.BoolVar(&cfg.skipSubmodules, "skip-submodules", false, "leave every repository that is a submodule of another repository untouched")
	flag.BoolVar(&cfg.syncSubmodules, "sync-submodules", false, "sync repositories that are submodules of another repository too (the default syncs a submodule with its parent)")
	flag.BoolVar(&cfg.switchDetached, "switch-detached", false, "move a detached HEAD to the default branch and sync it")
	flag.BoolVar(&cfg.recover, "recover", false, "abort an unfinished merge/rebase and continue")
	flag.BoolVar(&cfg.listOnly, "list", false, "list the repositories and exit")
	flag.BoolVar(&cfg.verbose, "v", false, "verbose output")
	showVersion := flag.Bool("version", false, "print the version and exit")
	skip := flag.String("skip", "", "regexp of paths to skip")
	flag.IntVar(&cfg.maxDepth, "maxdepth", 0, "maximum directory depth to scan (0 = unlimited)")
	timeout := flag.Duration("timeout", 10*time.Minute, "per-git-command timeout")
	flag.BoolVar(&cfg.followSymlinks, "follow-symlinks", false, "walk symlinked directories too")
	flag.Usage = usage

	// The directory to scan can be given as an argument instead of with
	// -root: the flag package stops at the first argument that is not a
	// flag, so the arguments are sorted out separately and the flags behind
	// them are still parsed.
	dir, err := parseArgs()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		os.Exit(2)
	}
	if dir != "" {
		rootGiven := false
		flag.Visit(func(f *flag.Flag) {
			if f.Name == "root" {
				rootGiven = true
			}
		})
		if rootGiven {
			fmt.Fprintf(os.Stderr, "%s: give the directory either as an argument or with -root, not both\n", progName)
			os.Exit(2)
		}
		cfg.root = dir
	}

	if *showVersion {
		fmt.Printf("%s %s\n", progName, version)
		return
	}

	if *jobs > 0 {
		cfg.jobs = *jobs
	} else {
		cfg.jobs = runtime.NumCPU()
		if cfg.jobs > 8 {
			cfg.jobs = 8
		}
	}
	if cfg.jobs < 1 {
		cfg.jobs = 1
	}
	if *skip != "" {
		re, err := regexp.Compile(*skip)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: invalid -skip regexp: %v\n", progName, err)
			os.Exit(2)
		}
		cfg.skipRE = re
	}
	if cfg.skipSubmodules && cfg.syncSubmodules {
		fmt.Fprintf(os.Stderr, "%s: -skip-submodules and -sync-submodules contradict each other\n", progName)
		os.Exit(2)
	}
	cfg.autoResolve = strings.Split(*autoResolve, ",")
	cfg.timeout = *timeout

	candidates := []string{"main", "master"}
	cfg.candidates = candidates

	root, err := scanRoot(cfg.root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		os.Exit(2)
	}
	// Use the absolute path everywhere so that paths are shown relative to
	// the scan root.
	cfg.root = root

	s := &syncer{cfg: cfg}

	repos, bare, hidden, err := scan(root, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
	}
	// A bare candidate that is the git directory of a repository that was
	// found as well belongs to that repository, not to a repository of its
	// own.
	bare = dropGitDirs(bare, repos, cfg.timeout)
	repos = append(repos, bare...)
	sort.Strings(repos)

	if cfg.listOnly {
		for _, r := range repos {
			fmt.Println(r)
		}
		if len(repos) == 0 {
			reportEmpty(root, cfg)
		}
		return
	}

	// A hidden repository whose remote is back gets its .git back and is
	// synced like any other repository.
	var restores []restoreResult
	for _, h := range hidden {
		ok, summary := s.restoreHidden(h)
		restores = append(restores, restoreResult{path: h, restored: ok, summary: summary})
		if ok {
			repos = append(repos, h)
		}
	}
	sort.Strings(repos)

	if len(repos) == 0 {
		reportEmpty(root, cfg)
		return
	}

	// The repositories of this run: a submodule whose parent is one of them
	// is synced with its parent, not on its own.
	s.cfg.repos = make(map[string]bool, len(repos))
	for _, r := range repos {
		s.cfg.repos[canonical(r)] = true
	}

	fmt.Printf("%s %s — syncing %d repositor%s under %s\n",
		progName, version, len(repos), plural(len(repos)), root)
	if cfg.dryRun {
		fmt.Println(paint("90", "dry run: no changes will be made"))
	}
	fmt.Println()

	for _, r := range restores {
		tag := paint("90", "HIDDEN")
		if r.restored {
			tag = paint("32", "RESTORE")
		}
		fmt.Printf("%s %s  %s\n", tag, indent(r.path, root), r.summary)
	}
	if len(restores) > 0 {
		fmt.Println()
	}

	results := s.runAll(repos)

	if cfg.dryRun {
		for i := range results {
			results[i].summary = "plan: " + results[i].summary
		}
	}
	printSummary(results, root, cfg.dryRun)

	// Exit non-zero only when something actually failed.
	for _, r := range results {
		if r.st == statusFail {
			os.Exit(1)
		}
	}
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

func (s *syncer) runAll(repos []string) []result {
	results := make([]result, len(repos))
	sem := make(chan struct{}, s.cfg.jobs)
	var wg sync.WaitGroup
	var printMu sync.Mutex

	for i, r := range repos {
		wg.Add(1)
		go func(i int, r string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := s.syncOne(r)
			results[i] = res
			printMu.Lock()
			fmt.Printf("%s %s  %s\n", label(res, s.cfg.dryRun), indent(res.repo, s.cfg.root), res.summary)
			printMu.Unlock()
		}(i, r)
	}
	wg.Wait()
	return results
}

func printSummary(results []result, root string, dryRun bool) {
	width := 0
	for _, r := range results {
		if n := len(indent(r.repo, root)); n > width {
			width = n
		}
	}
	if width > 60 {
		width = 60
	}

	counts := map[status]int{}
	for _, r := range results {
		counts[r.st]++
	}

	fmt.Println()
	fmt.Println(strings.Repeat("─", 72))
	for _, r := range results {
		name := indent(r.repo, root)
		if len(name) > width {
			name = "…" + name[len(name)-width+1:]
		}
		branch := r.branch
		if branch == "" {
			branch = "-"
		}
		fmt.Printf("%s  %-*s  %-10s  %s", label(r, dryRun), width, name, branch, r.summary)
		if len(r.details) > 0 {
			fmt.Printf("\n      %s", paint("90", r.details[0]))
			for _, d := range r.details[1:] {
				fmt.Printf("\n      %s", paint("90", d))
			}
		}
		fmt.Println()
	}
	fmt.Println(strings.Repeat("─", 72))
	fmt.Printf("OK %d   WARN %d   FAIL %d   SKIP %d   (%d total)\n",
		counts[statusOK], counts[statusWarn], counts[statusFail], counts[statusSkip], len(results))
}

func usage() {
	out := os.Stderr
	fmt.Fprintf(out, `%s %s — find git repositories under a directory and sync them with their remotes.

Usage:
  %s [flags] [directory]

  The directory to scan can be given as an argument or with -root; it
  defaults to the current directory.

Flags:
  -root string       directory to scan for git repositories (default ".")
  -remote string     remote to sync from (default "origin")
  -jobs int          repositories synced in parallel (default: min(NumCPU, 8))
  -dry-run           show the commands that would run, change nothing
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
  -follow-symlinks   walk symlinked directories too
  -timeout duration  per-git-command timeout (default 10m0s)
  -v                 verbose output
  -version           print the version and exit
`, progName, version, progName)
}
