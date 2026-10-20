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
//  2. Fetches the remote with --prune.
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

const (
	progName = "ggg"
	version  = "1.0.0"
)

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
	skipRE         *regexp.Regexp
	maxDepth       int
	timeout        time.Duration
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

// ---------------------------------------------------------------------------
// git plumbing
// ---------------------------------------------------------------------------

type git struct {
	dir     string
	timeout time.Duration
	dryRun  bool
	mu      *sync.Mutex
	log     func(string)
}

func (g git) run(args ...string) error {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
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
		return fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return nil
}

// mutate runs a command that changes the repository. In dry-run mode the
// command is printed instead of executed, so read-only inspection still
// reports the real state of the repository.
func (g git) mutate(args ...string) error {
	if g.dryRun {
		g.log("  would run: git " + strings.Join(args, " "))
		return nil
	}
	return g.run(args...)
}

// output runs a command and returns its trimmed stdout.
func (g git) output(args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), g.timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", g.dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// ok reports whether a command succeeds.
func (g git) ok(args ...string) bool {
	return g.run(args...) == nil
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
	return g.run("rev-parse", "--verify", "--quiet", ref+"^{commit}") == nil
}

// isAncestor reports whether a is an ancestor of b (b contains a).
func (g git) isAncestor(a, b string) bool {
	return g.run("merge-base", "--is-ancestor", a, b) == nil
}

func (g git) branchExists(name string) bool {
	return g.run("show-ref", "--verify", "--quiet", "refs/heads/"+name) == nil
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
	return g.run("rev-parse", "--verify", "--quiet", "HEAD") == nil
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

// stash pushes the working tree (including untracked files) onto the stash
// stack and returns the SHA of the new stash entry.
func (g git) stash(message string) (string, error) {
	if err := g.mutate("stash", "push", "--include-untracked", "-m", message); err != nil {
		return "", err
	}
	if g.dryRun {
		return "", nil
	}
	sha, err := g.output("rev-parse", "--verify", "--quiet", "refs/stash")
	if err != nil {
		return "", fmt.Errorf("could not identify the new stash entry")
	}
	return sha, nil
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
// discovery
// ---------------------------------------------------------------------------

var defaultSkipDirs = map[string]bool{
	".git":          true,
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

func findRepos(root string, cfg config) ([]string, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(abs)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		abs = filepath.Dir(abs)
	}

	var repos []string
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
		if path != abs {
			if defaultSkipDirs[d.Name()] {
				return fs.SkipDir
			}
			if cfg.skipRE != nil && cfg.skipRE.MatchString(path) {
				return fs.SkipDir
			}
			if cfg.maxDepth > 0 {
				rel, rerr := filepath.Rel(abs, path)
				if rerr == nil {
					depth := len(strings.Split(rel, string(os.PathSeparator)))
					if depth > cfg.maxDepth {
						return fs.SkipDir
					}
				}
			}
		}
		if isRepoDir(path) {
			repos = append(repos, path)
		}
		return nil
	})
	if walkErr != nil {
		return repos, walkErr
	}
	sort.Strings(repos)
	return repos, nil
}

// ---------------------------------------------------------------------------
// syncer
// ---------------------------------------------------------------------------

type syncer struct {
	cfg config
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
	if s.cfg.skipSubmodules {
		if super := g.superproject(); super != "" {
			return res.skip(start, "submodule of "+filepath.Base(super)+"; skipped")
		}
	}

	// --- 2. fetch -----------------------------------------------------------
	fetchFailed := false
	if err := g.mutate("fetch", "--prune", remote); err != nil {
		fetchFailed = true
		res.st = statusWarn
		res.note("fetch failed: %v", err)
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
	flag.BoolVar(&cfg.skipSubmodules, "skip-submodules", false, "leave repositories that are submodules of another repository untouched")
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

	repos, err := findRepos(root, cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
	}
	if len(repos) == 0 {
		fmt.Printf("%s: no git repositories found under %s\n", progName, root)
		return
	}

	if cfg.listOnly {
		for _, r := range repos {
			fmt.Println(r)
		}
		return
	}

	fmt.Printf("%s %s — syncing %d repositor%s under %s\n",
		progName, version, len(repos), plural(len(repos)), root)
	if cfg.dryRun {
		fmt.Println(paint("90", "dry run: no changes will be made"))
	}
	fmt.Println()

	s := &syncer{cfg: cfg}
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
  -skip-submodules   leave repositories that are submodules of another repository untouched
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
