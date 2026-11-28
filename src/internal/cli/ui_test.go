// SPDX-License-Identifier: Apache-2.0

package cli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/snowops-oss/snowopslabs/internal/webui"
)

func ipNet(s string) *net.IPNet {
	ip, n, _ := net.ParseCIDR(s)
	n.IP = ip
	return n
}

// The machine's interfaces: loopback, a LAN address, link-local and a
// global IPv6 address.
var testAddrs = []net.Addr{ipNet("127.0.0.1/8"), ipNet("fe80::1/64"), ipNet("2001:db8::7/64"), ipNet("192.168.1.20/24")}

func TestPlanUI(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		opts      uiOptions
		auth      bool
		wantURLs  []string
		wantNotes []string
		wantTLS   bool
	}{
		{"the default, this machine only", uiOptions{port: "3939", bind: "127.0.0.1"}, false,
			[]string{"http://localhost:3939"}, nil, false},
		{"a network bind lists the LAN addresses and warns about plain HTTP", uiOptions{port: "3939", bind: "0.0.0.0"}, true,
			[]string{"http://192.168.1.20:3939", "http://[2001:db8::7]:3939", "http://localhost:3939"}, []string{"plain HTTP"}, false},
		{"one address", uiOptions{port: "8443", bind: "192.168.1.20", selfSigned: true}, true,
			[]string{"https://192.168.1.20:8443"}, []string{"self-signed", "fingerprint"}, true},
		{"a team server with a self-signed certificate", uiOptions{port: "3939", bind: "0.0.0.0", selfSigned: true}, true,
			[]string{"https://192.168.1.20:3939", "https://[2001:db8::7]:3939", "https://localhost:3939"}, []string{"SHA-256 fingerprint"}, true},
		{"the operator's own certificate", uiOptions{port: "3939", bind: "0.0.0.0", tlsCert: "c.pem", tlsKey: "k.pem"}, true,
			[]string{"https://192.168.1.20:3939", "https://[2001:db8::7]:3939", "https://localhost:3939"}, nil, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := t.TempDir()
			plan, err := planUI(tt.opts, tt.auth, state, "lab-host", testAddrs, now, webui.DistFS)
			if err != nil {
				t.Fatalf("planUI: %v", err)
			}
			if strings.Join(plan.urls, " ") != strings.Join(tt.wantURLs, " ") {
				t.Errorf("urls = %v, want %v", plan.urls, tt.wantURLs)
			}
			notes := strings.Join(plan.notes, "\n")
			for _, w := range tt.wantNotes {
				if !strings.Contains(notes, w) {
					t.Errorf("notes do not say %q:\n%s", w, notes)
				}
			}
			if len(tt.wantNotes) == 0 && notes != "" {
				t.Errorf("unexpected notes:\n%s", notes)
			}
			if (plan.certFile != "") != tt.wantTLS {
				t.Errorf("certFile = %q, want TLS %v", plan.certFile, tt.wantTLS)
			}
			if tt.opts.selfSigned && filepath.Dir(plan.certFile) != filepath.Join(state, "tls") {
				t.Errorf("self-signed certificate at %s, want it in the lab state", plan.certFile)
			}
		})
	}
}

// A self-signed certificate covers every URL the server prints, so a
// participant who checks the fingerprint gets no name mismatch.
func TestPlanUI_SelfSignedCoversEveryURL(t *testing.T) {
	plan, err := planUI(uiOptions{port: "3939", bind: "0.0.0.0", selfSigned: true}, true, t.TempDir(), "lab-host", testAddrs, time.Now(), webui.DistFS)
	if err != nil {
		t.Fatal(err)
	}
	cert := loadCert(t, plan.certFile, plan.keyFile)
	for _, u := range append(plan.urls, "https://lab-host:3939", "https://127.0.0.1:3939") {
		host, _, _ := net.SplitHostPort(strings.TrimPrefix(u, "https://"))
		if err := cert.VerifyHostname(host); err != nil {
			t.Errorf("certificate does not cover %s: %v", u, err)
		}
	}
}

func TestPlanUI_Failures(t *testing.T) {
	tests := []struct {
		name    string
		opts    uiOptions
		auth    bool
		distFS  fs.FS
		wantErr string
	}{
		{"a network bind without auth", uiOptions{bind: "0.0.0.0"}, false, webui.DistFS, "LABCTL_AUTH=true"},
		{"self-signed with the operator's certificate", uiOptions{bind: "127.0.0.1", selfSigned: true, tlsCert: "c.pem", tlsKey: "k.pem"}, false, webui.DistFS, "drop --tls-cert and --tls-key"},
		{"a certificate without its key", uiOptions{bind: "127.0.0.1", tlsCert: "c.pem"}, false, webui.DistFS, "must be provided together"},
		{"a broken embedded UI", uiOptions{bind: "127.0.0.1"}, false, brokenEmbedFS{}, "make cli-build"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := planUI(tt.opts, tt.auth, t.TempDir(), "h", testAddrs, time.Now(), tt.distFS)
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("planUI = %v, want it to say %q", err, tt.wantErr)
			}
		})
	}
}

// The ways `labctl ui` refuses to start, as a user sees them.
func TestUI_Failures(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"expose the API with auth off", []string{"ui", "--bind", "0.0.0.0"}, "refusing to bind 0.0.0.0 without authentication"},
		{"two certificates at once", []string{"ui", "--tls-self-signed", "--tls-cert", "c.pem", "--tls-key", "k.pem"}, "drop --tls-cert and --tls-key"},
		{"a key without its certificate", []string{"ui", "--tls-key", "k.pem"}, "must be provided together"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newTerminal(t)
			t.Setenv("LABCTL_AUTH", "")
			r := l.run(tt.args...)
			if r.exitCode() != 1 || !strings.Contains(r.stderr, tt.wantErr) {
				t.Errorf("exit %d, stderr %q; want 1 and %q", r.exitCode(), r.stderr, tt.wantErr)
			}
		})
	}
}

// uiDistFS succeeds against the real embed, and wraps a broken one's error
// with a next step.
func TestUIDistFS(t *testing.T) {
	if _, err := uiDistFS(webui.DistFS); err != nil {
		t.Fatalf("uiDistFS(webui.DistFS) = %v, want no error", err)
	}

	_, err := uiDistFS(brokenEmbedFS{})
	if err == nil || !strings.Contains(err.Error(), "make cli-build") {
		t.Errorf("uiDistFS(brokenEmbedFS{}) = %v, want it to say to run `make cli-build`", err)
	}
}

// brokenEmbedFS stands in for an embed whose dist/ subtree failed to load,
// something the real embed.FS cannot do once it compiles.
type brokenEmbedFS struct{ fstest.MapFS }

func (brokenEmbedFS) Sub(string) (fs.FS, error) { return nil, errors.New("boom") }

func loadCert(t *testing.T, certFile, keyFile string) *x509.Certificate {
	t.Helper()
	kp, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(kp.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

// The browser opens only once the port is bound: a page opened early fails to
// load. The fake opener records whether the port accepts connections at the
// moment it is asked to open the page.
func TestUI_OpensBrowserOnlyAfterBind(t *testing.T) {
	tests := []struct {
		name string
		args []string
	}{
		{"plain HTTP", nil},
		{"HTTPS", []string{"--tls-self-signed"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			l := newTerminal(t)
			port := freeTCPPort(t)
			marker := installFakeBrowser(t, port)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go func() {
				waitForFile(marker, 10*time.Second)
				cancel()
			}()

			t.Setenv("LABCTL_AUTH", "")
			r := l.runCtx(ctx, append([]string{"ui", "--port", port}, tt.args...)...)
			got, err := os.ReadFile(marker)
			if err != nil {
				t.Fatalf("the browser never opened; exit %d, stdout %q, stderr %q", r.exitCode(), r.stdout, r.stderr)
			}
			if strings.TrimSpace(string(got)) != "up" {
				t.Error("browser opened before the port accepted connections")
			}
		})
	}
}

// A port held by another process is reported, and no browser opens.
func TestUI_BusyPortOpensNoBrowser(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	l := newTerminal(t)
	marker := installFakeBrowser(t, port)
	t.Setenv("LABCTL_AUTH", "")
	r := l.run("ui", "--port", port)
	if r.exitCode() != 1 || !strings.Contains(r.stderr, "Another `labctl ui` may already be running") {
		t.Errorf("exit %d, stderr %q; want 1 and the already-running message", r.exitCode(), r.stderr)
	}
	// The opener runs asynchronously, so give a late launch time to show up.
	waitForFile(marker, 2*time.Second)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a browser opened for a port that is taken")
	}
}

func TestUI_InvalidTLSCertDoesNotOpenBrowserOrHoldPort(t *testing.T) {
	port := freeTCPPort(t)
	marker := installFakeBrowser(t, port)
	l := newTerminal(t)
	t.Setenv("LABCTL_AUTH", "")
	missing := filepath.Join(t.TempDir(), "missing.pem")
	r := l.run("ui", "--port", port, "--tls-cert", missing, "--tls-key", missing)
	if r.exitCode() != 1 || !strings.Contains(r.stderr, "loading TLS certificate") {
		t.Fatalf("exit %d, stderr %q; want a TLS certificate error", r.exitCode(), r.stderr)
	}

	waitForFile(marker, 100*time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Error("a browser opened after TLS certificate loading failed")
	}

	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("the UI port is still held after TLS certificate loading failed: %v", err)
	}
	_ = ln.Close()
}

// installFakeBrowser puts a shell script named like every browser opener on
// PATH. Each script writes "up" to the returned marker file if port accepts
// connections when it runs, and "down" otherwise.
func installFakeBrowser(t *testing.T, port string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the fake browser opener is a POSIX shell script")
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "opened")
	testBinary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SNOWOPS_FAKE_BROWSER_MARKER", marker)
	t.Setenv("SNOWOPS_FAKE_BROWSER_ADDR", net.JoinHostPort("127.0.0.1", port))
	script := "#!/bin/sh\nexec " + shellQuote(testBinary) + " -test.run='^TestFakeBrowserProbe$'\n"
	for _, name := range []string{"wslview", "powershell.exe", "cmd.exe", "xdg-open", "open"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return marker
}

// TestFakeBrowserProbe runs in a subprocess launched by the fake browser
// command and records whether the server accepts connections.
func TestFakeBrowserProbe(t *testing.T) {
	marker := os.Getenv("SNOWOPS_FAKE_BROWSER_MARKER")
	if marker == "" {
		return
	}
	conn, err := net.DialTimeout("tcp", os.Getenv("SNOWOPS_FAKE_BROWSER_ADDR"), time.Second)
	state := "down"
	if err == nil {
		state = "up"
		_ = conn.Close()
	}
	if err := os.WriteFile(marker, []byte(state), 0o600); err != nil {
		t.Fatal(err)
	}
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
}

// waitForFile polls until path exists or d has passed.
func waitForFile(path string, d time.Duration) {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// freeTCPPort returns a loopback port that nothing listens on right now.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_ = ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return port
}
