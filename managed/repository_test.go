package managed

import "testing"

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
