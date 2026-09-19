package demorepo

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

	// A port is chosen by binding one and letting it go, so between that and
	// Zoekt binding it there is a window where anything else can take it.
	// Zoekt cannot be handed a listening socket, so the window cannot be
	// closed — but it can be survived, and survived precisely: a server that
	// lost the port exits rather than running without one.
	//
	//	ListenAndServe: listen tcp 127.0.0.1:64625: bind: address already in use
	//
	// So the answer to "did this attempt work" is not a timeout, it is whether
	// the process is still alive when it answers. A collision costs another
	// attempt on another port; anything else still fails with the server's own
	// output, as it did before.
	//
	// It matters more than it used to: several of these run at once now, which
	// turns a window that used to be theoretical into one that is merely rare.
	for attempt := 1; ; attempt++ {
		url, started := startZoektOnce(t, binary, indexDir)
		if started {
			return url
		}
		if attempt == zoektStartAttempts {
			t.Fatalf("zoekt-webserver lost the port it was given %d times running", attempt)
		}
	}
}

// zoektStartAttempts bounds how many ports one server may lose before the test
// gives up. Losing one is rare; losing several in a row is not a busy machine,
// it is something else wrong, and waiting longer will not diagnose it.
const zoektStartAttempts = 5

// startZoektOnce runs one attempt, reporting whether the server came up. A
// server that did is left running with its cleanup registered; one that died
// on the way is fully cleaned up before this returns, so an attempt leaves
// nothing behind for the next one to trip over.
func startZoektOnce(t testing.TB, binary, indexDir string) (string, bool) {
	t.Helper()

	address := closedAddress(t)
	logPath := filepath.Join(t.TempDir(), fmt.Sprintf("zoekt-webserver-%s.log", strings.ReplaceAll(address, ":", "-")))
	logFile, err := os.Create(logPath)
	if err != nil {
		t.Fatalf("creating %s: %v", logPath, err)
	}

	cmd := exec.Command(binary, "-index", indexDir, "-listen", address, "-rpc", "-html=false")
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting zoekt-webserver: %v", err)
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	url := "http://" + address
	if waitReady(t, url, logPath, exited) {
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			<-exited
			_ = logFile.Close()
		})
		return url, true
	}

	// It died. Nothing to kill, but the file it was writing to is this
	// attempt's and closing it here is what keeps a retry from accumulating
	// open files.
	_ = logFile.Close()
	return "", false
}

// waitReady blocks until the server answers its health check, reporting
// whether it did. It returns false, without failing the test, for the one
// outcome that is worth another attempt: the process exited on its own, which
// is what losing the port looks like. Every other way of not being ready is
// still fatal, with the server's own output as the diagnosis.
func waitReady(t testing.TB, url, logPath string, exited <-chan error) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		select {
		case <-exited:
			return false
		default:
		}

		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url+"/healthz", nil)
		if err != nil {
			t.Fatalf("building the health check request: %v", err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
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
