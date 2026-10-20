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
var version = "1.4.0"

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

// isRepoDir reports whether dir is the top level of a git repository.
func isRepoDir(dir string) bool {
	st, err := os.Stat(filepath.Join(dir, ".git"))
	if err != nil {
		return false
	}
	// A directory for normal clones, a file for worktrees and submodules.
	return st.IsDir() || st.Mode().IsRegular()
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

// scan walks the tree once and returns the git repositories it finds plus the
// hidden ones: directories that hold a .git_old but no .git, i.e. repositories
// whose remote was gone.
func scan(root string, cfg config) (repos, hidden []string, err error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, nil, err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	walkErr := filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// Unreadable directory: skip it rather than aborting the scan.
			if d != nil && d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if skipWalkDir(path, abs, d, cfg) {
			return fs.SkipDir
		}
		switch {
		case isRepoDir(path):
			repos = append(repos, path)
		case hasGitOld(path):
			hidden = append(hidden, path)
		}
		return nil
	})
	if walkErr != nil {
		return repos, hidden, walkErr
	}
	sort.Strings(repos)
	sort.Strings(hidden)
	return repos, hidden, nil
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
	if out, err := g.output("rev-parse", "--is-bare-repository"); err == nil && strings.TrimSpace(out) == "true" {
		return res.skip(start, "bare repository")
	}
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
	if err := g.mutate("fetch", "--prune", remote); err != nil {
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
	flag.Usage = usage
	flag.Parse()

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

	root, err := filepath.Abs(cfg.root)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		os.Exit(2)
	}
	// Use the absolute path everywhere so that paths are shown relative to
	// the scan root.
	cfg.root = root

	s := &syncer{cfg: cfg}

	repos, hidden, err := scan(root, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
	}
	if cfg.listOnly {
		for _, r := range repos {
			fmt.Println(r)
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

	// The repositories of this run: a submodule whose parent is one of them
	// is synced with its parent, not on its own.
	s.cfg.repos = make(map[string]bool, len(repos))
	for _, r := range repos {
		s.cfg.repos[canonical(r)] = true
	}

	if len(repos) == 0 {
		fmt.Printf("%s: no git repositories found under %s\n", progName, root)
		return
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
  %s [flags]

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
  -timeout duration  per-git-command timeout (default 10m0s)
  -v                 verbose output
  -version           print the version and exit
`, progName, version, progName)
}
