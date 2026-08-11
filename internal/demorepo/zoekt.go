package demorepo

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// StartZoekt runs a Zoekt web server over indexDir for the duration of the test
// and returns its base URL. -rpc turns on the JSON API the search adapter uses;
// the HTML interface is off because nothing here reads it.
//
// It lives here rather than in one test package because more than one of them
// needs a real engine: doc-1's release gate is about Zoekt and this adapter
// agreeing on how a branch is selected, which a fake engine cannot show.
func StartZoekt(t testing.TB, indexDir string) string {
	t.Helper()
	binary, err := exec.LookPath("zoekt-webserver")
	if err != nil {
		t.Skipf("zoekt-webserver is not on PATH, see CONTRIBUTING.md: %v", err)
	}

	// ponytail: the port is picked by closing a listener and handing the number
	// over, so another process could take it in between. Closing that window
	// needs Zoekt to accept an already listening socket.
	address := closedAddress(t)
	logPath := filepath.Join(t.TempDir(), "zoekt-webserver.log")
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating %s: %v", logPath, err)
	}

	cmd := exec.Command(binary, "-index", indexDir, "-listen", address, "-rpc", "-html=false")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting zoekt-webserver: %v", err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = logFile.Close()
	})

	url := "http://" + address
	waitReady(t, url, logPath)
	return url
}

// waitReady blocks until the server answers its health check. On timeout the
// server's own output is the diagnosis, so it is read back and reported.
func waitReady(t testing.TB, url, logPath string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/healthz", nil)
		if err != nil {
			t.Fatalf("building the health check request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
			err = fmt.Errorf("http %s", resp.Status)
		}
		if time.Now().After(deadline) {
			output, _ := os.ReadFile(logPath)
			t.Fatalf("zoekt-webserver at %s never became ready: %v\n%s", url, err, output)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// closedAddress returns a loopback address that was free and is now unbound: a
// port to start a server on, or one guaranteed to refuse a connection.
func closedAddress(t testing.TB) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("releasing %s: %v", address, err)
	}
	return address
}
