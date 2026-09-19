//go:build integration

package demorepo_test

import (
	"net/http"
	"sync"
	"testing"

	"github.com/tc3oliver/version-aware-code-mcp/internal/demorepo"
)

// TestConcurrentZoektServersEachGetTheirOwnPort is what the port handling has
// to survive now that the integration tests run several at a time.
//
// A port is chosen by binding one and letting it go, so between that and Zoekt
// binding it there is a window. One server at a time almost never loses that
// race; several starting together is how it stops being theoretical — two
// helpers can be handed the same number by the kernel in the moment neither
// holds it.
//
// What this asserts is the outcome rather than the mechanism: every server is
// reachable, and no two of them are the same server. A collision that was
// silently survived by retrying is a pass, which is the point — the helper is
// allowed to lose a port, and not allowed to hand back one that is not its
// own.
func TestConcurrentZoektServersEachGetTheirOwnPort(t *testing.T) {
	fixture := demorepo.Prepared(t)

	const servers = 6
	urls := make([]string, servers)

	var group sync.WaitGroup
	for i := range servers {
		group.Add(1)
		go func() {
			defer group.Done()
			// StartZoekt registers its own cleanup against t, which is safe
			// from several goroutines and runs when this test ends.
			urls[i] = demorepo.StartZoekt(t, fixture.ZoektIndex)
		}()
	}
	group.Wait()

	seen := map[string]int{}
	for i, url := range urls {
		if url == "" {
			t.Fatalf("server %d did not come up", i)
		}
		if first, repeated := seen[url]; repeated {
			t.Errorf("servers %d and %d were both handed %s", first, i, url)
		}
		seen[url] = i
	}

	// Reachable, not merely started: a server that lost its port and was
	// retried has a different address than the one it first asked for, and
	// only a request proves the address that came back is the live one.
	for i, url := range urls {
		resp, err := http.Get(url + "/healthz")
		if err != nil {
			t.Errorf("server %d at %s: %v", i, url, err)
			continue
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("server %d at %s answered %s", i, url, resp.Status)
		}
	}
}
