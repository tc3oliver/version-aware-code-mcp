package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/vacerr"
)

// The repo commands are the only part of vacmcp that reaches a network, and the
// two arguments they take — a repository name and a remote URL — are the only
// user input that becomes part of a git command line. These tests are the
// adversarial half of that: what a hostile name or URL is able to make happen,
// and what a credential handed to vacmcp is able to leave behind.
//
// They are written against real git, for the same reason the rest of the repo
// tests are: what is being checked is git's behaviour, and a fake git would
// happily confirm whatever this file assumed about it.

// markerScript is a command a successful injection would run. Nothing in vacmcp
// is supposed to be able to reach a shell, so the file it would create is the
// evidence: if it exists, something interpreted a string that should have stayed
// an argument.
func markerFor(t *testing.T, name string) string {
	t.Helper()
	return filepath.Join(t.TempDir(), name)
}

func mustNotExist(t *testing.T, path, what string) {
	t.Helper()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s exists at %s (err = %v), want nothing there", what, path, err)
	}
}

// findInDataDir returns the files under root whose name or content contains
// needle. It is how "the credential did not land anywhere" is checked without
// having to know which file would have held it.
func findInDataDir(t *testing.T, root, needle string) []string {
	t.Helper()
	var found []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.Contains(path, needle) {
			found = append(found, path)
		}
		if entry.IsDir() {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), needle) {
			found = append(found, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// TestRepoAddRefusesAURLThatEmbedsACredential is AC #3. A credential in a URL is
// refused at the door rather than masked afterwards, because git writes the URL
// it was handed into remote.origin.url: a password vacmcp masked in its own
// record would still be in plain text in the clone's .git/config, inside the
// data directory. Nothing that runs after the clone can undo that, so nothing
// that runs after the clone is what this checks.
func TestRepoAddRefusesAURLThatEmbedsACredential(t *testing.T) {
	const secret = "s3cr3t-do-not-store"

	for _, url := range []string{
		"https://alice:" + secret + "@example.invalid/x.git",
		"http://alice:" + secret + "@example.invalid/x.git",
		"HTTPS://alice:" + secret + "@example.invalid/x.git",
		// A forge token in the username field is a credential too, and nothing
		// can tell it from a login name, so HTTP userinfo goes altogether.
		"https://" + secret + "@example.invalid/x.git",
		// A password is a secret under any transport, SSH included.
		"ssh://alice:" + secret + "@example.invalid/x.git",
		"git://alice:" + secret + "@example.invalid/x.git",
		"alice:" + secret + "@example.invalid:x.git",
	} {
		data := t.TempDir()

		out, err := repoRun(t, data, "add", "demo", "--url", url)
		if got := codeFor(t, err); got != vacerr.InvalidArgument {
			t.Errorf("repo add %q code = %q, want %q", url, got, vacerr.InvalidArgument)
		}
		// The refusal itself must not repeat what it refused: an error is the
		// one thing here that reaches a log.
		if strings.Contains(err.Error(), secret) {
			t.Errorf("repo add %q error repeats the credential: %v", url, err)
		}
		if strings.Contains(out, secret) {
			t.Errorf("repo add %q printed the credential: %q", url, out)
		}
		// Nothing was recorded, nothing was cloned, and no trace of the secret
		// is anywhere under the data directory.
		if _, err := openStore(t, data).Repository("demo"); codeFor(t, err) != vacerr.RepositoryNotFound {
			t.Errorf("repo add %q left a record: %v", url, err)
		}
		if leaked := findInDataDir(t, data, secret); len(leaked) > 0 {
			t.Errorf("repo add %q left the credential in %v", url, leaked)
		}
	}
}

// TestRepoAddAcceptsAnSSHIdentity keeps the refusal above from swallowing the
// authentication decision-4 expects people to use: `git@host` is an SSH login
// name, not a secret, and the key behind it never goes near vacmcp.
func TestRepoAddAcceptsAnSSHIdentity(t *testing.T) {
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

// TestRepoAddTreatsAHostileArgumentAsAnArgument is AC #2: a name or a URL full
// of shell metacharacters reaches git as one literal argument and gets no
// further than a clean failure.
func TestRepoAddTreatsAHostileArgumentAsAnArgument(t *testing.T) {
	source := sourceRepo(t)
	marker := markerFor(t, "pwned")

	hostile := []string{
		"; touch " + marker,
		"$(touch " + marker + ")",
		"`touch " + marker + "`",
		"| touch " + marker,
		"&& touch " + marker,
		"\ntouch " + marker + "\n",
		"x\x00touch " + marker,
		// A URL that looks like an option: --end-of-options is what keeps git
		// from reading it as one, and --upload-pack is the option that would
		// have run a command.
		"--upload-pack=touch " + marker + ";git-upload-pack",
		"-u touch " + marker,
	}

	for _, argument := range hostile {
		data := t.TempDir()

		// As a URL: the name is fine, so this reaches git.
		if _, err := repoRun(t, data, "add", "demo", "--url", argument); err == nil {
			t.Errorf("repo add --url %q returned nil, want a failure", argument)
		}
		// As a name, for the ones that can be one: the flag package takes an
		// argument beginning with a dash for a flag, so those never reach a
		// name at all, and the store's allowlist refuses the rest before git is
		// reached.
		if !strings.HasPrefix(argument, "-") {
			if _, err := repoRun(t, data, "add", argument, "--url", source); err == nil {
				t.Errorf("repo add %q returned nil, want a failure", argument)
			}
		}
		mustNotExist(t, marker, "the marker an injected command would have created")
		mustNotExist(t, filepath.Join(data, "repos", "demo"), "a clone of an unusable URL")

		// A URL git could not clone is a recorded failure, not a repository
		// that quietly half-exists.
		if r, err := openStore(t, data).Repository("demo"); err != nil || r.State != repoFailed {
			t.Errorf("record after repo add --url %q = %+v (err %v), want state %s", argument, r, err, repoFailed)
		}
	}
}

// TestRepoAddCannotRunAGitRemoteHelper is the other half of AC #2, and the one
// that has teeth: avoiding a shell is not enough, because git's remote helpers
// are a shell of their own. `ext::sh -c ...` runs a command by design, and a
// user who enabled that transport in their own git configuration would be
// running it from whatever URL vacmcp passed on. The configuration below is
// exactly that user, and this test fails without the hardening in runGit.
func TestRepoAddCannotRunAGitRemoteHelper(t *testing.T) {
	data := t.TempDir()
	marker := markerFor(t, "pwned")

	gitconfig := filepath.Join(t.TempDir(), "gitconfig")
	if err := os.WriteFile(gitconfig, []byte("[protocol \"ext\"]\n\tallow = user\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("GIT_CONFIG_GLOBAL", gitconfig)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")

	// git-remote-ext's own escape: "% " is a space inside one argument.
	if _, err := repoRun(t, data, "add", "demo", "--url", "ext::sh -c touch% "+marker); err == nil {
		t.Error("repo add of an ext:: URL returned nil, want the transport refused")
	}
	mustNotExist(t, marker, "the marker the ext transport would have created")
}

// TestRepoAddAuthenticatesThroughTheInheritedEnvironment is AC #1: vacmcp holds
// no credential and passes none, so authentication has to be reaching git
// through the process environment — the same channel SSH_AUTH_SOCK, HOME and a
// credential helper travel on. The fake ssh below is how that is observed
// without a server: git only runs it if the environment got through.
//
// It then fails the way an unusable credential does, which is the second half of
// this: the failure is reported, the repository is recorded as FAILED, and the
// message carries nothing but the URL the user typed.
func TestRepoAddAuthenticatesThroughTheInheritedEnvironment(t *testing.T) {
	data := t.TempDir()
	proof := markerFor(t, "ssh-ran")

	fakeSSH := filepath.Join(t.TempDir(), "fake-ssh")
	script := "#!/bin/sh\ntouch " + proof + "\necho 'Permission denied (publickey).' >&2\nexit 255\n"
	if err := os.WriteFile(fakeSSH, []byte(script), 0o700); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	t.Setenv("GIT_SSH_COMMAND", fakeSSH)

	_, err := repoRun(t, data, "add", "demo", "--url", "ssh://git@127.0.0.1/x.git")
	if err == nil {
		t.Fatal("repo add with an unusable credential returned nil, want a failure")
	}
	if _, statErr := os.Stat(proof); statErr != nil {
		t.Fatalf("git did not run GIT_SSH_COMMAND (%v): the process environment is not reaching git, so no SSH agent or credential helper could either", statErr)
	}
	if r, err := openStore(t, data).Repository("demo"); err != nil || r.State != repoFailed {
		t.Errorf("record = %+v (err %v), want state %s", r, err, repoFailed)
	}
}

// TestRepoRecordsCarryNoCredentialField is AC #1's other half: there is no field
// to put a token in. The record written by a real `repo add` is read back as the
// JSON on disk, so a field added later without thinking about this shows up here.
func TestRepoRecordsCarryNoCredentialField(t *testing.T) {
	data := t.TempDir()
	if _, err := repoRun(t, data, "add", "demo", "--url", sourceRepo(t)); err != nil {
		t.Fatalf("repo add: %v", err)
	}

	record, err := os.ReadFile(filepath.Join(data, "repositories", "demo.json"))
	if err != nil {
		t.Fatalf("reading the record: %v", err)
	}
	for _, credential := range []string{"token", "password", "secret", "passphrase", "key", "auth"} {
		if strings.Contains(strings.ToLower(string(record)), credential) {
			t.Errorf("the repository record mentions %q, want no credential field:\n%s", credential, record)
		}
	}
}
