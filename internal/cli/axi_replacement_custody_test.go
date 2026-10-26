package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kunchenguid/no-mistakes/internal/config"
	"github.com/kunchenguid/no-mistakes/internal/custody"
	"github.com/kunchenguid/no-mistakes/internal/db"
	"github.com/kunchenguid/no-mistakes/internal/gate"
	"github.com/kunchenguid/no-mistakes/internal/git"
	"github.com/kunchenguid/no-mistakes/internal/ipc"
	"github.com/kunchenguid/no-mistakes/internal/paths"
	"github.com/kunchenguid/no-mistakes/internal/types"
)

const replacementBranch = "feature/replace"

type replacementCustodyFixture struct {
	p                                                       *paths.Paths
	local, gate, submitted, reviewed, obsolete, replacement string
	runID, obsoleteArchive, replacementArchive              string
	repo                                                    *db.Repo
}

// newReplacementCustodyFixture reproduces the custody sequence that follows an
// explicitly invalidated active correction round, with synthetic repositories:
// the operator submitted S; the pipeline rebased it onto a newer main as R
// (resolving a conflict on the same line, so S's content does not merge
// cleanly into anything built on R) and committed the round's fix F on top;
// the operator cancelled that round, so F was never pushed and the gate lane
// still holds S. The operator's corrected replacement C is a sibling of F on R.
//
// The fixture leaves the operator at the point the original sequence reached
// after following every printed action: custody was returned at F by plain
// `--recover`, F is archived under refs/heads/archive/*, and the clean branch
// has been moved to C.
func newReplacementCustodyFixture(t *testing.T) replacementCustodyFixture {
	t.Helper()
	t.Setenv("NM_HOME", makeSocketSafeTempDir(t))
	root := t.TempDir()
	remote := filepath.Join(root, "remote.git")
	cliGit(t, root, "init", "--bare", remote)
	local := filepath.Join(root, "operator")
	cliGit(t, root, "init", "-b", "main", local)
	cliGit(t, local, "config", "user.name", "Test")
	cliGit(t, local, "config", "user.email", "test@example.com")
	commit := func(dir, name, content, message string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		cliGit(t, dir, "add", name)
		cliGit(t, dir, "commit", "-m", message)
		return cliGit(t, dir, "rev-parse", "HEAD")
	}
	base := commit(local, "file.txt", "base\n", "base")
	cliGit(t, local, "checkout", "-b", replacementBranch)
	submitted := commit(local, "file.txt", "feature v1\n", "feature")
	cliGit(t, local, "checkout", "main")
	newerMain := commit(local, "file.txt", "main v2\n", "advance main")
	cliGit(t, local, "checkout", replacementBranch)

	p, err := paths.New()
	if err != nil {
		t.Fatal(err)
	}
	if err := p.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	database, err := db.Open(p.DB())
	if err != nil {
		t.Fatal(err)
	}
	registeredRoot, err := git.FindGitRoot(local)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := database.InsertRepo(registeredRoot, remote, "main")
	if err != nil {
		t.Fatal(err)
	}
	gateDir := p.RepoDir(repo.ID)
	cliGit(t, filepath.Dir(gateDir), "init", "--bare", gateDir)
	cliGit(t, gateDir, "config", "receive.advertisePushOptions", "true")
	cliGit(t, local, "remote", "add", gate.RemoteName, gateDir)
	cliGit(t, local, "push", gateDir, "refs/heads/"+replacementBranch+":refs/heads/"+replacementBranch)

	// The run's own worktree: rebase with a conflict resolution, then the
	// correction round's fix. Neither commit reaches the gate lane.
	pipeline := filepath.Join(root, "pipeline")
	cliGit(t, root, "-c", "core.autocrlf=false", "clone", local, pipeline)
	cliGit(t, pipeline, "config", "user.name", "Pipeline")
	cliGit(t, pipeline, "config", "user.email", "pipeline@example.com")
	cliGit(t, pipeline, "checkout", "-b", "rebased", newerMain)
	reviewed := commit(pipeline, "file.txt", "feature v1 on main v2\n", "feature")
	obsolete := commit(pipeline, "fix.txt", "obsolete round fix\n", "no-mistakes(review): fix")

	run, err := database.InsertRun(repo.ID, replacementBranch, submitted, base)
	if err != nil {
		t.Fatal(err)
	}
	// Terminalization pins the verified unpublished head before the managed
	// worktree is removed.
	cliGit(t, pipeline, "push", gateDir, obsolete+":"+custody.RecoveryRef(run.ID))
	if err := database.UpdateRunStatusWithVerifiedHead(run.ID, types.RunCancelled, obsolete); err != nil {
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}

	// The operator's corrected replacement is built on the reviewed head.
	cliGit(t, local, "fetch", "--no-tags", pipeline, "refs/heads/rebased")
	cliGit(t, local, "reset", "--keep", reviewed)
	replacement := commit(local, "contract.txt", "clarified contract\n", "clarify the contract")
	replacementArchive := "refs/heads/archive/replacement"
	cliGit(t, local, "update-ref", replacementArchive, replacement)
	chdir(t, local)

	// Step 1: the replacement diverges from the preserved head, so nothing
	// can prove containment.
	status, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("initial status: %v\n%s", err, status)
	}
	for _, want := range []string{"state: pipeline_owned", "relation: diverged", "safety: blocked_recover_manual_reconciliation"} {
		if !strings.Contains(status, want) {
			t.Fatalf("initial status missing %q:\n%s", want, status)
		}
	}

	// Step 2: at the reviewed head the preserved head is a strict
	// fast-forward, so plain recovery is offered and returns custody at F.
	cliGit(t, local, "reset", "--keep", reviewed)
	status, err = executeCmd("axi", "status")
	if err != nil || !strings.Contains(status, "code: recover_custody") || !strings.Contains(status, "command: no-mistakes axi sync --recover\n") {
		t.Fatalf("behind status should offer plain recovery: %v\n%s", err, status)
	}
	if out, err := executeCmd("axi", "sync", "--recover"); err != nil || !strings.Contains(out, "state: custody_returned") {
		t.Fatalf("recover: %v\n%s", err, out)
	}
	if got := cliGit(t, local, "rev-parse", "HEAD"); got != obsolete {
		t.Fatalf("recovered HEAD = %s, want %s", got, obsolete)
	}

	// Step 3: preserve the obsolete head under an archive and move the clean
	// branch to the authorized replacement.
	obsoleteArchive := "refs/heads/archive/obsolete-round"
	cliGit(t, local, "update-ref", obsoleteArchive, obsolete)
	cliGit(t, local, "reset", "--keep", replacement)

	return replacementCustodyFixture{
		p: p, local: local, gate: gateDir, submitted: submitted, reviewed: reviewed, obsolete: obsolete,
		replacement: replacement, runID: run.ID, obsoleteArchive: obsoleteArchive,
		replacementArchive: replacementArchive, repo: repo,
	}
}

func (f replacementCustodyFixture) snapshot(t *testing.T) string {
	t.Helper()
	return strings.Join([]string{
		cliGit(t, f.local, "rev-parse", "HEAD"),
		cliGit(t, f.local, "status", "--porcelain=v1", "--untracked-files=all"),
		cliGit(t, f.local, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
		cliGit(t, f.gate, "for-each-ref", "--format=%(refname) %(objectname) %(symref)"),
	}, "\n---\n")
}

// triggerReplacementRun drives the same trigger `axi run` uses against a
// minimal daemon that registers whatever head reaches the gate lane.
func triggerReplacementRun(t *testing.T, f replacementCustodyFixture) (string, error) {
	t.Helper()
	d, err := db.Open(f.p.DB())
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	srv := ipc.NewServer()
	srv.Handle(ipc.MethodGetRunsForHead, func(context.Context, json.RawMessage) (interface{}, error) {
		return &ipc.GetRunsResult{}, nil
	})
	srv.Handle(ipc.MethodGetActiveRun, func(context.Context, json.RawMessage) (interface{}, error) {
		head, err := git.Run(context.Background(), f.gate, "rev-parse", "refs/heads/"+replacementBranch)
		if err != nil {
			return &ipc.GetActiveRunResult{}, nil
		}
		return &ipc.GetActiveRunResult{Run: &ipc.RunInfo{
			ID: "01M5FRESHREPLACEMENTRUN0000", RepoID: f.repo.ID, Branch: replacementBranch,
			HeadSHA: strings.TrimSpace(head), Status: types.RunRunning,
		}}, nil
	})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(f.p.Socket()) }()
	defer func() { srv.Close(); <-done }()
	var client *ipc.Client
	deadline := time.Now().Add(3 * time.Second)
	for {
		client, err = ipc.Dial(f.p.Socket())
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal(err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	defer client.Close()
	env := &axiEnv{p: f.p, d: d, repo: f.repo, cfg: config.DefaultGlobalConfig(), client: client}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return triggerRun(ctx, env, replacementBranch, nil, "", "", false, "", nil)
}

// TestReplacementHeadAfterReturnedCustodyStartsOneFreshRun is the regression
// for the stale custody loop after an invalidated correction round. Before the
// fix, status advertised adopt_published although nothing was ever published
// and the gate lane no longer held the recovered head, so the exact advertised
// command could only refuse, and binding the archive was refused because
// custody had already returned. The bound archive is now the operator's
// authorization for exactly this replacement head: the guarded keep-local
// recovery moves only the stale gate lane, both obsolete heads stay pinned,
// and one fresh run starts.
func TestReplacementHeadAfterReturnedCustodyStartsOneFreshRun(t *testing.T) {
	f := newReplacementCustodyFixture(t)

	status, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("replacement status: %v\n%s", err, status)
	}
	for _, want := range []string{
		"state: custody_returned",
		"relation: diverged",
		"safety: blocked_replacement_archive_required",
		"code: inspect_and_reconcile_manually",
		"--bind-archive-ref",
	} {
		if !strings.Contains(status, want) {
			t.Errorf("replacement status missing %q:\n%s", want, status)
		}
	}
	if strings.Contains(status, "adopt_published") || strings.Contains(status, "--adopt-published") {
		t.Fatalf("status advertised published-head adoption although nothing was published:\n%s", status)
	}

	// Without the binding a fresh run still refuses: the stale lane's
	// submitted commits are neither contained in the replacement nor anchored.
	before := f.snapshot(t)
	if _, err := triggerReplacementRun(t, f); err == nil || !strings.Contains(err.Error(), "refusing to reconcile private mirror") {
		t.Fatalf("unbound replacement run should refuse at gate reconciliation, got %v", err)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("refused fresh run changed a branch, ref, or worktree")
	}

	// An archive at any other head is not evidence for the obsolete head.
	wrong, err := executeCmd("axi", "sync", "--bind-archive-ref", f.replacementArchive)
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(wrong, "safety: blocked_recover_archive_moved") {
		t.Fatalf("binding an archive that does not pin the obsolete head should refuse, got %v\n%s", err, wrong)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("refused binding changed a branch, ref, or worktree")
	}

	bound, err := executeCmd("axi", "sync", "--bind-archive-ref", f.obsoleteArchive)
	if err != nil {
		t.Fatalf("bind obsolete archive after custody returned: %v\n%s", err, bound)
	}
	for _, want := range []string{
		"state: custody_returned",
		"safety: replacement_recoverable",
		"source: bound_archive",
		"required_head: " + f.replacement,
		"preserved_head: " + f.obsolete,
		"archive_ref: " + f.obsoleteArchive,
		"proof: verified",
		"code: recover_custody",
		"command: no-mistakes axi sync --recover --keep-local",
	} {
		if !strings.Contains(bound, want) {
			t.Errorf("bound replacement missing %q:\n%s", want, bound)
		}
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("binding archive evidence changed a branch, ref, or worktree")
	}

	refused, err := executeCmd("axi", "sync", "--recover")
	if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(refused, "safety: blocked_recover_archive_requires_keep_local") {
		t.Fatalf("plain recovery should refuse the replacement plan, got %v\n%s", err, refused)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("refused plain recovery changed a branch, ref, or worktree")
	}

	recovered, err := executeCmd("axi", "sync", "--recover", "--keep-local")
	if err != nil {
		t.Fatalf("replacement keep-local recovery: %v\n%s", err, recovered)
	}
	for _, want := range []string{"recovered: true", "state: custody_returned", "safety: gate_ready", "code: run_pipeline"} {
		if !strings.Contains(recovered, want) {
			t.Errorf("replacement recovery missing %q:\n%s", want, recovered)
		}
	}
	if got := cliGit(t, f.local, "rev-parse", "HEAD"); got != f.replacement {
		t.Fatalf("recovery moved the worktree to %s, want %s", got, f.replacement)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/"+replacementBranch); got != f.replacement {
		t.Fatalf("gate lane = %s, want replacement %s", got, f.replacement)
	}
	if got := cliGit(t, f.gate, "rev-parse", custody.RecoveryGateRef(f.runID)); got != f.submitted {
		t.Fatalf("stale gate lane anchor = %s, want submitted head %s", got, f.submitted)
	}
	if got := cliGit(t, f.gate, "rev-parse", custody.RecoveryRef(f.runID)); got != f.obsolete {
		t.Fatalf("gate recovery ref = %s, want obsolete head %s", got, f.obsolete)
	}
	if got := cliGit(t, f.local, "rev-parse", f.obsoleteArchive); got != f.obsolete {
		t.Fatalf("archive = %s, want obsolete head %s", got, f.obsolete)
	}
	if out, err := executeCmd("axi", "sync", "--recover", "--keep-local"); err != nil || !strings.Contains(out, "recovered: true") || !strings.Contains(out, "safety: gate_ready") {
		t.Fatalf("repeated replacement recovery should be an idempotent no-op: %v\n%s", err, out)
	}

	runID, err := triggerReplacementRun(t, f)
	if err != nil {
		t.Fatalf("fresh run from the authorized replacement: %v", err)
	}
	if runID == "" {
		t.Fatal("authorized replacement started no run")
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/"+replacementBranch); got != f.replacement {
		t.Fatalf("fresh run left gate lane at %s, want %s", got, f.replacement)
	}
}

// TestReplacementArchiveFailsClosedWithoutGitMutation revalidates the binding
// on every read: once the archive, the replacement head, the gate lane, or the
// worktree no longer matches it, status falls back to manual reconciliation
// and the keep-local recovery refuses without moving any ref.
func TestReplacementArchiveFailsClosedWithoutGitMutation(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, f replacementCustodyFixture)
		safety string
	}{
		{
			name: "archive moved",
			mutate: func(t *testing.T, f replacementCustodyFixture) {
				cliGit(t, f.local, "update-ref", f.obsoleteArchive, f.reviewed)
			},
			safety: "blocked_recover_archive_moved",
		},
		{
			name: "replacement head moved",
			mutate: func(t *testing.T, f replacementCustodyFixture) {
				cliGit(t, f.local, "commit", "--allow-empty", "-m", "unbound follow-up")
			},
			safety: "blocked_recover_archive_required_head_mismatch",
		},
		{
			name: "gate lane moved independently",
			mutate: func(t *testing.T, f replacementCustodyFixture) {
				cliGit(t, f.local, "push", f.gate, f.reviewed+":refs/heads/independent")
				cliGit(t, f.gate, "update-ref", "refs/heads/"+replacementBranch, f.reviewed, f.submitted)
			},
			safety: "blocked_recover_archive_gate_head_mismatch",
		},
		{
			name: "dirty worktree",
			mutate: func(t *testing.T, f replacementCustodyFixture) {
				if err := os.WriteFile(filepath.Join(f.local, "scratch.txt"), []byte("untracked\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
			safety: "blocked_recover_archive_dirty",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newReplacementCustodyFixture(t)
			if out, err := executeCmd("axi", "sync", "--bind-archive-ref", f.obsoleteArchive); err != nil {
				t.Fatalf("bind: %v\n%s", err, out)
			}
			tc.mutate(t, f)
			before := f.snapshot(t)
			status, err := executeCmd("axi", "status")
			if err != nil {
				t.Fatalf("status: %v\n%s", err, status)
			}
			for _, want := range []string{"state: custody_returned", "safety: " + tc.safety, "code: inspect_and_reconcile_manually"} {
				if !strings.Contains(status, want) {
					t.Errorf("status missing %q:\n%s", want, status)
				}
			}
			out, err := executeCmd("axi", "sync", "--recover", "--keep-local")
			var ee *exitError
			if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(out, "safety: "+tc.safety) {
				t.Fatalf("keep-local recovery should refuse with %s, got %v\n%s", tc.safety, err, out)
			}
			if after := f.snapshot(t); after != before {
				t.Fatal("refused replacement recovery changed a branch, ref, or worktree")
			}
		})
	}
}

// TestReplacementArchiveBindingRefusals keeps the binding narrow: it needs a
// divergent replacement, and the append-only record admits one candidate.
func TestReplacementArchiveBindingRefusals(t *testing.T) {
	f := newReplacementCustodyFixture(t)

	cliGit(t, f.local, "reset", "--keep", f.obsolete)
	before := f.snapshot(t)
	out, err := executeCmd("axi", "sync", "--bind-archive-ref", f.obsoleteArchive)
	var ee *exitError
	if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(out, "safety: blocked_recover_archive_not_divergent") {
		t.Fatalf("binding at the recovered head should refuse, got %v\n%s", err, out)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("refused binding changed a branch, ref, or worktree")
	}

	cliGit(t, f.local, "reset", "--keep", f.replacement)
	if out, err := executeCmd("axi", "sync", "--bind-archive-ref", f.obsoleteArchive); err != nil {
		t.Fatalf("bind: %v\n%s", err, out)
	}
	if out, err := executeCmd("axi", "sync", "--bind-archive-ref", f.obsoleteArchive); err != nil || !strings.Contains(out, "safety: replacement_recoverable") {
		t.Fatalf("re-binding the same archive should be idempotent: %v\n%s", err, out)
	}
	second := "refs/heads/archive/obsolete-round-copy"
	cliGit(t, f.local, "update-ref", second, f.obsolete)
	before = f.snapshot(t)
	out, err = executeCmd("axi", "sync", "--bind-archive-ref", second)
	if err == nil || !asExitError(err, &ee) || ee.code != 1 || !strings.Contains(out, "safety: blocked_recover_archive_ambiguous") {
		t.Fatalf("a second candidate should refuse, got %v\n%s", err, out)
	}
	if after := f.snapshot(t); after != before {
		t.Fatal("refused second binding changed a branch, ref, or worktree")
	}
}

// TestReplacementArchiveLeavesPublishedAdoptionUnchanged keeps the existing
// published-rewrite contract: while the gate lane still holds the recovered
// head, an unbound diverged branch is still offered adopt_published. Only an
// explicit binding switches it to the replacement recovery, which then needs
// no extra anchor because the lane head is the already-pinned recovered head.
func TestReplacementArchiveLeavesPublishedAdoptionUnchanged(t *testing.T) {
	f := newReplacementCustodyFixture(t)
	cliGit(t, f.local, "push", f.gate, f.obsolete+":refs/heads/obsolete-staging")
	cliGit(t, f.gate, "update-ref", "refs/heads/"+replacementBranch, f.obsolete, f.submitted)
	cliGit(t, f.gate, "update-ref", "-d", "refs/heads/obsolete-staging")

	status, err := executeCmd("axi", "status")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, status)
	}
	for _, want := range []string{"safety: recovery_required", "code: adopt_published", "command: no-mistakes axi sync --adopt-published"} {
		if !strings.Contains(status, want) {
			t.Errorf("unbound status missing %q:\n%s", want, status)
		}
	}

	if out, err := executeCmd("axi", "sync", "--bind-archive-ref", f.obsoleteArchive); err != nil || !strings.Contains(out, "safety: replacement_recoverable") {
		t.Fatalf("bind: %v\n%s", err, out)
	}
	if out, err := executeCmd("axi", "sync", "--recover", "--keep-local"); err != nil || !strings.Contains(out, "safety: gate_ready") {
		t.Fatalf("replacement recovery from the recovered lane head: %v\n%s", err, out)
	}
	if got := cliGit(t, f.gate, "rev-parse", "refs/heads/"+replacementBranch); got != f.replacement {
		t.Fatalf("gate lane = %s, want replacement %s", got, f.replacement)
	}
	if _, err := git.Run(context.Background(), f.gate, "rev-parse", "--verify", custody.RecoveryGateRef(f.runID)); err == nil {
		t.Fatal("the already-pinned recovered head was anchored a second time")
	}
}
