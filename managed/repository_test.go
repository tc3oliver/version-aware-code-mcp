package managed

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// TestAddAcceptsAnSSHIdentity keeps the refusal of a credential-bearing URL from
// swallowing the authentication decision-4 expects people to use: `git@host` is
// an SSH login name, not a secret, and the key behind it never goes near vacmcp.
//
// The refusal itself, and what a hostile URL is able to make happen, are in
// cmd/vacmcp's repo_security_test.go against real git: what is being checked
// there is git's behaviour, and only the rule for reading a URL is here.
func TestAddAcceptsAnSSHIdentity(t *testing.T) {
	for _, url := range []string{
		"ssh://git@example.invalid/x.git",
		"git@example.invalid:org/x.git",
		"https://example.invalid/x.git",
		"file:///srv/git/x.git",
		"/srv/git/x.git",
		"https://example.invalid:8443/x.git",
	} {
		if embedsCredential(url) {
			t.Errorf("embedsCredential(%q) = true, want false: no secret is in that URL", url)
		}
	}
}

// TestConcurrentAddsLeaveOneHealthyRepository is why Add runs under the
// repository's lock. The check that the repository is not managed yet and the
// clone it permits are one operation: without the lock every caller passes the
// check, they all clone into the same directory, and the ones that lose the
// race record FAILED over the record of the clone that worked — a repository
// reported as broken when what is on disk is fine.
func TestConcurrentAddsLeaveOneHealthyRepository(t *testing.T) {
	source := gitSource(t)
	data := t.TempDir()
	m, err := NewRepositoryManager(data)
	if err != nil {
		t.Fatalf("NewRepositoryManager: %v", err)
	}

	// Released at once, so the callers are inside Add together rather than
	// merely started together.
	const callers = 4
	start := make(chan struct{})
	failures := make([]error, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			_, failures[i] = m.Add(context.Background(), "demo", source)
		}()
	}
	close(start)
	wg.Wait()

	added := 0
	for i, err := range failures {
		if err == nil {
			added++
			continue
		}
		// Every caller but one is refused for the one reason a second add is
		// ever refused, and not by a clone that collided with another.
		if code := codeFor(t, err); code != vacerr.InvalidArgument {
			t.Errorf("caller %d failed with %s (%v), want %s: already managed", i, code, err, vacerr.InvalidArgument)
		}
	}
	if added != 1 {
		t.Fatalf("%d of %d concurrent adds succeeded, want exactly 1", added, callers)
	}

	r, err := openStore(t, data).Repository("demo")
	if err != nil {
		t.Fatalf("Repository(demo): %v", err)
	}
	if r.State != RepositoryReady {
		t.Errorf("record.State = %q, want %s: the clone worked, so nothing may record it as failed", r.State, RepositoryReady)
	}

	// And what is on disk is one clone that git can read, not one that a second
	// clone landed in the middle of.
	repos := filepath.Join(data, "repos")
	entries, err := os.ReadDir(repos)
	if err != nil {
		t.Fatalf("ReadDir(%s): %v", repos, err)
	}
	if len(entries) != 1 {
		t.Errorf("%s holds %d entries, want the one clone", repos, len(entries))
	}
	if out, err := exec.Command("git", "-C", filepath.Join(repos, "demo"), "cat-file", "-p", "HEAD:one.txt").CombinedOutput(); err != nil {
		t.Errorf("the clone cannot be read after concurrent adds: %v\n%s", err, out)
	} else if strings.TrimSpace(string(out)) != "one" {
		t.Errorf("HEAD:one.txt in the clone = %q, want the source's content", out)
	}
}

// gitSource creates a git repository with one commit and returns its path. It
// stands in for the remote a user would add: a local path is a git remote URL
// like any other, so the clone above is a real one and no network is involved.
func gitSource(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "source")
	mustGit(t, "init", "-q", "-b", "main", dir)
	if err := os.WriteFile(filepath.Join(dir, "one.txt"), []byte("one\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	mustGit(t, "-C", dir, "add", "one.txt")
	// The caller's global git configuration decides neither the identity nor
	// whether this commit is signed or hooked.
	mustGit(t, "-C", dir,
		"-c", "user.name=vacmcp test", "-c", "user.email=test@example.invalid", "-c", "commit.gpgsign=false",
		"commit", "--no-verify", "-q", "-m", "one")
	return dir
}

func mustGit(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}
