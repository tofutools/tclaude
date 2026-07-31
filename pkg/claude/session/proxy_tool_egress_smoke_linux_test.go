//go:build linux

package session

import (
	"archive/zip"
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	clcommon "github.com/tofutools/tclaude/pkg/claude/common"
	"github.com/tofutools/tclaude/pkg/claude/common/sandboxpolicy"
)

// §8.1 test 5: representative tool binaries executed inside the proxy floor
// under the identical launch environment, proving that allowed destinations
// succeed through the proxy and denied ones fail closed.
//
// # Why the fixtures are host loopback rather than the runner's netns
//
// Every destination here is a server this test starts itself on host loopback
// and reaches through an authored `loopback` rule. That is deliberate: `git`
// over HTTPS and a `go` module fetch need a real TLS origin, and owning the
// origin is what lets this smoke prove the strongest property in the set —
// that the proxy does NOT intercept TLS. The CA trusted by the tools is the
// FIXTURE ORIGIN's own, minted here and handed to each tool through its own
// trust variable. tclaude installs no CA anywhere, and if the proxy were
// terminating TLS instead of tunnelling it, every one of these tools would
// fail to verify.
//
// The denied ports are LIVE servers, not closed ones. A refusal against a dead
// port would prove nothing: the failure has to be the policy answering.
//
// # Stated honesty limit (§8.1)
//
// This proves the environment tools inherit obeys the proxy. It does not prove
// that a model-driven tool call does. The floor is what makes that gap safe:
// a tool that ignores the proxy has no route at all, so the unproven part can
// only fail CLOSED. The capability detail says so rather than papering over it.
const (
	proxyEgressHelperEnv = "TCLAUDE_FILTERED_PROXY_EGRESS_HELPER"
	proxyEgressCAEnv     = "TCLAUDE_FILTERED_PROXY_EGRESS_CA"
	proxyEgressPortsEnv  = "TCLAUDE_FILTERED_PROXY_EGRESS_PORTS"

	// proxyEgressGoHost is why the go arm can work at all. cmd/go selects its
	// proxy through net/http's ProxyFromEnvironment, whose useProxy refuses
	// EVERY loopback destination before NO_PROXY is even consulted — verified:
	// https://127.0.0.1:PORT gets no proxy, https://<a name> does. So the go
	// arm addresses the fixture by NAME. The flow maps that name to 127.0.0.1
	// in the host resolver, which is where the proxy resolves it, and the
	// authored loopback row is what admits the result.
	proxyEgressGoHost = "egress.proxy.tclaude.test"

	proxyEgressModulePath    = "example.com/tclaudeproxysmoke"
	proxyEgressModuleVersion = "v1.0.0"
	proxyEgressBody          = "tclaude-proxy-egress-ok"
)

// proxyEgressPorts are the four host-loopback fixtures, passed to the
// in-sandbox helper as one env value so the two halves cannot disagree.
// The go arm gets its OWN pair of TLS ports rather than sharing git's. Sharing
// them meant a per-arm decision assertion could be satisfied by git alone, so
// the go arm's "denied" marker rested on an error that a proxy bypass would
// also produce — vacuous in exactly the direction this suite exists to prevent.
type proxyEgressPorts struct {
	AllowedHTTP  int
	DeniedHTTP   int
	AllowedTLS   int
	DeniedTLS    int
	AllowedGoTLS int
	DeniedGoTLS  int
}

func (p proxyEgressPorts) encode() string {
	return fmt.Sprintf("%d,%d,%d,%d,%d,%d",
		p.AllowedHTTP, p.DeniedHTTP, p.AllowedTLS, p.DeniedTLS,
		p.AllowedGoTLS, p.DeniedGoTLS)
}

func decodeProxyEgressPorts(t *testing.T, value string) proxyEgressPorts {
	t.Helper()
	fields := strings.Split(value, ",")
	require.Len(t, fields, 6, "malformed egress port fixture %q", value)
	numbers := make([]int, 6)
	for i, field := range fields {
		port, err := strconv.Atoi(field)
		require.NoError(t, err)
		numbers[i] = port
	}
	return proxyEgressPorts{
		AllowedHTTP: numbers[0], DeniedHTTP: numbers[1],
		AllowedTLS: numbers[2], DeniedTLS: numbers[3],
		AllowedGoTLS: numbers[4], DeniedGoTLS: numbers[5],
	}
}

// TestPinnedProxyToolEgress runs curl over both carriages, git over HTTPS, and
// a go module fetch inside one real proxy-engine launch.
func TestPinnedProxyToolEgress(t *testing.T) {
	if os.Getenv(proxySmokeEnv) != "1" {
		t.Skip("set TCLAUDE_FILTERED_PROXY_SMOKE=1 on the executing Linux CI boundary")
	}
	// The tools have to exist before a failure to run one can be read as a
	// policy result rather than a missing binary.
	for _, tool := range []string{"curl", "git", "go"} {
		_, err := exec.LookPath(tool)
		require.NoErrorf(t, err, "the tool-egress boundary requires %s on PATH", tool)
	}

	authority := newProxyEgressAuthority(t)
	repository := proxyEgressGitRepository(t)
	module := proxyEgressModuleZip(t)
	ports := proxyEgressPorts{
		AllowedHTTP:  proxyEgressHTTPServer(t, nil, repository, module),
		DeniedHTTP:   proxyEgressHTTPServer(t, nil, repository, module),
		AllowedTLS:   proxyEgressHTTPServer(t, authority, repository, module),
		DeniedTLS:    proxyEgressHTTPServer(t, authority, repository, module),
		AllowedGoTLS: proxyEgressHTTPServer(t, authority, repository, module),
		DeniedGoTLS:  proxyEgressHTTPServer(t, authority, repository, module),
	}

	helperBinary := "proxy-egress-helper"
	launch := runProxyEngineLaunch(t, proxyEngineLaunchInput{
		Rules: proxyEgressRules(ports),
		ExtraEnv: []string{
			proxyEgressHelperEnv + "=1",
			proxyEgressPortsEnv + "=" + ports.encode(),
		},
		WorkspaceBinaries: map[string]string{helperBinary: os.Args[0]},
		Timeout:           300 * time.Second,
		PrepareHome: func(t *testing.T, home, workspace string) map[string]string {
			t.Helper()
			// The CA is the FIXTURE ORIGIN's, and it is written inside the
			// sandbox home so the tools can read it. Handing it to the tools is
			// what makes "the proxy did not intercept" assertable: a MITM proxy
			// would present a certificate this CA did not sign.
			caPath := filepath.Join(workspace, "fixture-ca.pem")
			require.NoError(t, os.WriteFile(caPath, authority.caPEM, 0o600))
			cache := filepath.Join(workspace, "go-cache")
			modcache := filepath.Join(workspace, "go-mod")
			require.NoError(t, os.MkdirAll(cache, 0o700))
			require.NoError(t, os.MkdirAll(modcache, 0o700))
			return map[string]string{
				proxyEgressCAEnv: caPath,
				"GOCACHE":        cache,
				"GOMODCACHE":     modcache,
				"GOFLAGS":        "-mod=mod",
				// The checksum database is off because the fixture module is
				// not in it. GOPRIVATE/GONOPROXY are deliberately NOT set:
				// GOPRIVATE defaults GONOPROXY, and a GONOPROXY of "*" tells go
				// to bypass GOPROXY and resolve modules from version control —
				// which would take the go arm off the proxy path entirely and
				// make its denied case pass for the wrong reason.
				"GOSUMDB":   "off",
				"GONOSUMDB": "*",
				// A toolchain download would be a second, unauthored
				// destination and has nothing to do with what is under test.
				"GOTOOLCHAIN": "local",
			}
		},
		Command: func(workspace string) string {
			return clcommon.ShellQuoteArg(
				filepath.Join(workspace, helperBinary)) +
				" -test.run=^TestProxyToolEgressHelper$ -test.v"
		},
	})
	t.Logf("proxy tool-egress launch output:\n%s", launch.Output)

	for _, marker := range []string{
		"proxy-egress: curl/http-carriage/allowed: carried",
		"proxy-egress: curl/socks-carriage/allowed: carried",
		"proxy-egress: curl/http-carriage/denied: refused",
		"proxy-egress: curl/socks-carriage/denied: refused",
		"proxy-egress: git-https/allowed: carried",
		"proxy-egress: git-https/denied: refused",
		"proxy-egress: go-module/allowed: carried",
		"proxy-egress: go-module/denied: refused",
	} {
		assert.Contains(t, launch.Output, marker,
			"every tool must be observed on both the allowed and the denied side")
	}

	// The refusals must be the POLICY answering, which is only visible in the
	// proxy's own record. A curl that failed because the fixture was down would
	// satisfy the markers above and leave no refusal here.
	decisions := requireProxyDecisions(t, launch)

	// Each arm is proved at ITS OWN port. One aggregate counter would let a
	// single curl refusal stand in for the git and go arms, whose "denied"
	// markers rest on an error that a misconfigured fixture, a missing
	// endpoint, or a toolchain that bypassed the proxy would also produce.
	assert.Truef(t, proxyEgressCarried(decisions, ports.AllowedHTTP),
		"the authorized plain-HTTP destination was never carried; decisions:\n%s",
		formatProxyDecisions(decisions))
	assert.Truef(t, proxyEgressCarried(decisions, ports.AllowedTLS),
		"git's authorized TLS destination was never carried; decisions:\n%s",
		formatProxyDecisions(decisions))
	assert.Truef(t, proxyEgressCarried(decisions, ports.AllowedGoTLS),
		"the go module fetch never reached the proxy — check it is not bypassing it; decisions:\n%s",
		formatProxyDecisions(decisions))
	assert.Truef(t, proxyEgressRefused(decisions, ports.DeniedGoTLS),
		"the go denied arm produced no refusal at the proxy; it failed for another reason; decisions:\n%s",
		formatProxyDecisions(decisions))
	assert.Truef(t, proxyEgressRefused(decisions, ports.DeniedHTTP),
		"curl's denied arm produced no refusal at the proxy; it failed for another reason; decisions:\n%s",
		formatProxyDecisions(decisions))
	assert.Truef(t, proxyEgressRefused(decisions, ports.DeniedTLS),
		"git's denied arm produced no refusal at the proxy; it failed for another reason; decisions:\n%s",
		formatProxyDecisions(decisions))

	// Both carriages must appear, or "curl over both carriages" is unproven.
	carriages := map[string]bool{}
	for _, decision := range decisions {
		carriages[decision.Carriage] = true
	}
	assert.Truef(t, carriages["http"] && carriages["socks5"],
		"both carriages must have delivered traffic; decisions:\n%s",
		formatProxyDecisions(decisions))
}

// proxyEgressRules authorizes three host-loopback ports plus the name the go
// arm addresses. Every other fixture port is live and unauthored, which is what
// makes its refusal a policy answer rather than nothing listening.
func proxyEgressRules(ports proxyEgressPorts) sandboxpolicy.NetworkRules {
	return sandboxpolicy.NetworkRules{
		Mode: sandboxpolicy.AccessModeList,
		Allow: []sandboxpolicy.NetworkAllowEntry{
			{
				Loopback: true,
				Ports: []int{
					ports.AllowedHTTP, ports.AllowedTLS, ports.AllowedGoTLS,
				},
			},
			// Two jobs, both real. It makes the policy DISCRIMINATING — a
			// loopback-only list is deliberately not (the floor expresses host
			// loopback natively, including authored ports), so without a
			// non-loopback selector no engine deploys and the launch falls back
			// to the packet gateway. And it is the row the go arm actually
			// travels: cmd/go will not proxy a loopback literal, so that arm
			// addresses the fixture by this name, which the flow maps to
			// 127.0.0.1 for the host-side resolver.
			//
			// Only the ALLOWED go port is authorized here, so the denied go port
			// is refused on the authored name+port pair rather than incidentally.
			{Host: proxyEgressGoHost, Ports: []int{ports.AllowedGoTLS}},
		},
		Engine: sandboxpolicy.NetworkEngineProxy,
	}
}

// TestProxyToolEgressHelper runs INSIDE the sandbox and drives the real tools.
func TestProxyToolEgressHelper(t *testing.T) {
	if os.Getenv(proxyEgressHelperEnv) != "1" {
		t.Skip("proxy tool-egress smoke helper")
	}
	ports := decodeProxyEgressPorts(t, os.Getenv(proxyEgressPortsEnv))
	endpoint := proxySmokeProxyEndpoint(t)
	httpProxy := "http://" + endpoint
	socksProxy := "socks5h://" + endpoint

	// curl, over each carriage, against a live allowed port and a live denied
	// one. The proxy is named explicitly rather than inherited so the carriage
	// under test is unambiguous.
	for _, carriage := range []struct {
		name  string
		proxy string
	}{
		{"http-carriage", httpProxy},
		{"socks-carriage", socksProxy},
	} {
		body, err := proxyEgressCurl(carriage.proxy, ports.AllowedHTTP)
		require.NoErrorf(t, err, "curl over %s must reach the authorized port",
			carriage.name)
		require.Contains(t, body, proxyEgressBody)
		fmt.Printf("proxy-egress: curl/%s/allowed: carried\n", carriage.name)

		_, err = proxyEgressCurl(carriage.proxy, ports.DeniedHTTP)
		require.Errorf(t, err,
			"curl over %s reached an unauthorized port that is LIVE on the host",
			carriage.name)
		fmt.Printf("proxy-egress: curl/%s/denied: refused (%v)\n",
			carriage.name, err)
	}

	// git over HTTPS. The clone succeeding proves the CONNECT tunnel carried a
	// TLS session the fixture's own CA validated end to end — no interception,
	// and no tclaude-installed trust anywhere.
	require.NoError(t, proxyEgressGitClone(t, ports.AllowedTLS, httpProxy))
	fmt.Println("proxy-egress: git-https/allowed: carried")
	require.Error(t, proxyEgressGitClone(t, ports.DeniedTLS, httpProxy),
		"git reached an unauthorized origin that is LIVE on the host")
	fmt.Println("proxy-egress: git-https/denied: refused")

	// go module fetch, the third proxy-honoring toolchain in the set.
	require.NoError(t, proxyEgressGoDownload(t, ports.AllowedGoTLS, httpProxy))
	fmt.Println("proxy-egress: go-module/allowed: carried")
	require.Error(t, proxyEgressGoDownload(t, ports.DeniedGoTLS, httpProxy),
		"go reached an unauthorized module origin that is LIVE on the host")
	fmt.Println("proxy-egress: go-module/denied: refused")
}

func proxyEgressCurl(proxy string, port int) (string, error) {
	output, err := exec.Command("curl",
		"--silent", "--show-error", "--fail", "--max-time", "20",
		"--proxy", proxy,
		fmt.Sprintf("http://127.0.0.1:%d/ok", port),
	).CombinedOutput()
	return string(output), err
}

func proxyEgressGitClone(t *testing.T, port int, proxy string) error {
	t.Helper()
	destination := filepath.Join(t.TempDir(), "clone")
	command := exec.Command("git",
		"-c", "http.proxy="+proxy,
		"-c", "http.sslCAInfo="+os.Getenv(proxyEgressCAEnv),
		"clone", "--quiet",
		fmt.Sprintf("https://127.0.0.1:%d/repo.git", port), destination,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git clone: %w: %s", err, output)
	}
	return nil
}

func proxyEgressGoDownload(t *testing.T, port int, proxy string) error {
	t.Helper()
	module := t.TempDir()
	// A FRESH module cache per attempt. Without it the denied fetch is served
	// from what the allowed fetch already downloaded, succeeds without touching
	// the network at all, and the denied assertion silently stops testing the
	// boundary — the module cache answering instead of the policy.
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(module, "go.mod"),
		[]byte("module tclaude.test/egress\n\ngo 1.21\n"), 0o600); err != nil {
		return err
	}
	command := exec.Command("go", "mod", "download", "-x",
		proxyEgressModulePath+"@"+proxyEgressModuleVersion)
	command.Dir = module
	command.Env = append(os.Environ(),
		"GOMODCACHE="+cache,
		// By NAME, never by literal: cmd/go refuses to proxy any loopback
		// destination regardless of NO_PROXY, so a 127.0.0.1 GOPROXY would
		// bypass the boundary entirely and the denied case would "pass" on a
		// connection error rather than on a policy refusal.
		"GOPROXY=https://"+proxyEgressGoHost+":"+strconv.Itoa(port)+"/mod",
		"HTTPS_PROXY="+proxy,
		"HTTP_PROXY="+proxy,
		// Scoped to this command rather than set on the launch: the launch
		// environment is the host supervisor's too, and narrowing its Go root
		// pool to a throwaway fixture CA is a side effect on a host process
		// that has no business trusting it.
		"SSL_CERT_FILE="+os.Getenv(proxyEgressCAEnv),
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf("go mod download: %w: %s", err, output)
	}
	return nil
}

// proxyEgressAuthority is a throwaway CA and a leaf for 127.0.0.1. It belongs
// to the FIXTURE ORIGIN, never to the proxy: nothing in tclaude signs, holds or
// installs it.
type proxyEgressAuthority struct {
	caPEM       []byte
	certificate tls.Certificate
}

func newProxyEgressAuthority(t *testing.T) *proxyEgressAuthority {
	t.Helper()
	caKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "tclaude proxy egress fixture CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(
		rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	caCert, err := x509.ParseCertificate(caDER)
	require.NoError(t, err)

	leafKey, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	leafTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		// git reaches the fixture by literal, the go arm by name; the leaf has
		// to satisfy both or a failure is TLS verification rather than policy.
		DNSNames: []string{proxyEgressGoHost},
	}
	leafDER, err := x509.CreateCertificate(
		rand.Reader, leafTemplate, caCert, &leafKey.PublicKey, caKey)
	require.NoError(t, err)

	return &proxyEgressAuthority{
		caPEM: pem.EncodeToMemory(
			&pem.Block{Type: "CERTIFICATE", Bytes: caDER}),
		certificate: tls.Certificate{
			Certificate: [][]byte{leafDER, caDER},
			PrivateKey:  leafKey,
		},
	}
}

// proxyEgressHTTPServer starts one fixture origin on host loopback. A nil
// authority serves plain HTTP; otherwise it serves TLS with the fixture leaf.
// Every origin serves all three surfaces, so the allowed and denied sides
// differ only in what the policy authorizes.
func proxyEgressHTTPServer(
	t *testing.T,
	authority *proxyEgressAuthority,
	repository map[string][]byte,
	module map[string][]byte,
) int {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(proxyEgressBody))
	})
	serveTree := func(prefix string, files map[string][]byte) {
		mux.HandleFunc(prefix, func(w http.ResponseWriter, r *http.Request) {
			body, ok := files[strings.TrimPrefix(r.URL.Path, prefix)]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_, _ = w.Write(body)
		})
	}
	serveTree("/repo.git/", repository)
	serveTree("/mod/", module)

	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if authority != nil {
		server.TLSConfig = &tls.Config{
			Certificates: []tls.Certificate{authority.certificate},
			MinVersion:   tls.VersionTLS12,
		}
		listener = tls.NewListener(listener, server.TLSConfig)
	}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	address, ok := listener.Addr().(*net.TCPAddr)
	require.True(t, ok)
	return address.Port
}

// proxyEgressGitRepository builds a dumb-HTTP-servable bare repository. Dumb
// HTTP is chosen deliberately: it needs no CGI on the fixture side, so what the
// clone exercises is the transport rather than the server.
func proxyEgressGitRepository(t *testing.T) map[string][]byte {
	t.Helper()
	work := t.TempDir()
	source := filepath.Join(work, "source")
	bare := filepath.Join(work, "repo.git")
	require.NoError(t, os.MkdirAll(source, 0o700))
	require.NoError(t, os.WriteFile(
		filepath.Join(source, "README"), []byte(proxyEgressBody), 0o600))
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=main", source},
		{"-C", source, "config", "user.email", "smoke@tclaude.test"},
		{"-C", source, "config", "user.name", "tclaude smoke"},
		{"-C", source, "add", "README"},
		{"-C", source, "commit", "--quiet", "-m", "fixture"},
		{"clone", "--quiet", "--bare", source, bare},
		{"-C", bare, "update-server-info"},
	} {
		output, err := exec.Command("git", args...).CombinedOutput()
		require.NoErrorf(t, err, "git %v: %s", args, output)
	}
	return proxyEgressReadTree(t, bare)
}

func proxyEgressReadTree(t *testing.T, root string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	require.NoError(t, filepath.Walk(root,
		func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return relErr
			}
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			files[relative] = body
			return nil
		}))
	return files
}

// proxyEgressModuleZip builds the minimum GOPROXY surface for one module
// version: the list, the info, the go.mod, and the zip.
func proxyEgressModuleZip(t *testing.T) map[string][]byte {
	t.Helper()
	goMod := "module " + proxyEgressModulePath + "\n\ngo 1.21\n"
	archive := &bytes.Buffer{}
	writer := zip.NewWriter(archive)
	prefix := proxyEgressModulePath + "@" + proxyEgressModuleVersion + "/"
	for name, body := range map[string]string{
		"go.mod":   goMod,
		"smoke.go": "package tclaudeproxysmoke\n\n// " + proxyEgressBody + "\n",
	} {
		entry, err := writer.Create(prefix + name)
		require.NoError(t, err)
		_, err = entry.Write([]byte(body))
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())

	base := proxyEgressModulePath + "/@v/"
	return map[string][]byte{
		base + "list": []byte(proxyEgressModuleVersion + "\n"),
		base + proxyEgressModuleVersion + ".info": []byte(fmt.Sprintf(
			`{"Version":%q,"Time":"2020-01-01T00:00:00Z"}`,
			proxyEgressModuleVersion)),
		base + proxyEgressModuleVersion + ".mod": []byte(goMod),
		base + proxyEgressModuleVersion + ".zip": archive.Bytes(),
	}
}

// proxyEgressCarried reports whether the proxy actually carried a connection to
// this fixture port.
func proxyEgressCarried(decisions []proxyDecisionRecord, port int) bool {
	for _, decision := range decisions {
		if decision.Port == port && decision.Verdict == "allowed" {
			return true
		}
	}
	return false
}

// proxyEgressRefused reports whether the POLICY refused this fixture port. The
// port is live, so a refusal recorded here is the engine answering rather than
// a connection that failed on its own.
func proxyEgressRefused(decisions []proxyDecisionRecord, port int) bool {
	for _, decision := range decisions {
		if decision.Port == port && decision.Verdict != "allowed" {
			return true
		}
	}
	return false
}
