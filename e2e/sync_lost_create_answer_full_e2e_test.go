//go:build e2e && e2e_full

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// A create whose answer is lost on the wire: the CLI's HTTPS goes through a
// forward proxy the test starts, which terminates TLS with a certificate
// authority the CLI is told to trust (SSL_CERT_FILE), forwards every request
// to OneDrive as it is, and for the first create of one file name lets the
// create reach OneDrive and then closes the CLI's connection instead of
// answering. Nothing in the product is built for the test: the proxy is the
// standard HTTPS_PROXY route Go's transport already takes.
// ---------------------------------------------------------------------------

const (
	// faultProxyCertLifetime bounds the test authority and its leaves to the
	// run that made them.
	faultProxyCertLifetime = 2 * time.Hour
	// faultProxyReadHeaderTimeout bounds how long the proxy waits for a
	// CONNECT request's headers.
	faultProxyReadHeaderTimeout = 30 * time.Second
	// faultProxySerialBits is the size of a certificate's random serial.
	faultProxySerialBits = 128
	// lostCreateFileName is the name whose first create loses its answer.
	lostCreateFileName = "lost-create-answer.txt"
)

// faultProxy is a TLS-terminating forward proxy for the CLI subprocess. It
// owns its listener, the connections it accepted and their goroutines; the
// cleanup it registers closes them and waits for every goroutine to end.
type faultProxy struct {
	t        *testing.T
	listener net.Listener
	caPath   string
	ca       *x509.Certificate
	caKey    *ecdsa.PrivateKey
	upstream *http.Transport
	// ctx is the test's context: the forwarded requests run under it, so
	// they end with the test.
	ctx context.Context //nolint:containedctx // the proxy lives exactly as long as the test that owns it
	// dropAnswer reports a request whose answer the proxy drops after
	// OneDrive has answered it; it sees each request once, in order, and
	// may keep state.
	dropAnswer func(req *http.Request, body []byte) bool
	dropped    atomic.Int32

	mu sync.Mutex
	// leaves are the certificates made so far, by host; conns the client
	// connections open now, nil once the proxy stopped. Both are guarded by
	// mu, and so is every wg.Add of a tunnel's goroutine (track).
	leaves map[string]*tls.Certificate
	conns  map[net.Conn]struct{}
	wg     sync.WaitGroup
}

// startFaultProxy starts a proxy on a loopback port that drops the answers
// dropAnswer selects, and stops it when the test ends.
func startFaultProxy(t *testing.T, dropAnswer func(req *http.Request, body []byte) bool) *faultProxy {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber:          faultProxySerial(t),
		Subject:               pkix.Name{CommonName: "onedrive-go e2e fault proxy"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(faultProxyCertLifetime),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &caKey.PublicKey, caKey)
	require.NoError(t, err)
	ca, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	caPath := filepath.Join(t.TempDir(), "fault-proxy-ca.pem")
	require.NoError(t, os.WriteFile(caPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))

	listener, err := (&net.ListenConfig{}).Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	require.True(t, ok, "http.DefaultTransport is an *http.Transport")
	upstream := defaultTransport.Clone()
	// The CLI's own Accept-Encoding goes to OneDrive and its answer comes
	// back as sent. With compression on, the transport asked for gzip itself
	// and handed back the body decompressed with no length, which the
	// tunnel then wrote unframed (writeAnswer).
	upstream.DisableCompression = true
	proxy := &faultProxy{
		t: t, listener: listener, caPath: caPath, ca: ca, caKey: caKey,
		upstream: upstream, ctx: t.Context(), dropAnswer: dropAnswer,
		leaves: map[string]*tls.Certificate{}, conns: map[net.Conn]struct{}{},
	}
	server := &http.Server{Handler: http.HandlerFunc(proxy.serveConnect), ReadHeaderTimeout: faultProxyReadHeaderTimeout}
	proxy.wg.Go(func() {
		if serveErr := server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			t.Logf("fault proxy: serve ended: %v", serveErr)
		}
	})
	t.Cleanup(func() {
		assert.NoError(t, server.Close())
		proxy.mu.Lock()
		for conn := range proxy.conns {
			_ = conn.Close() //nolint:errcheck // ending the tunnel is the point; its error says nothing
		}
		proxy.conns = nil
		proxy.mu.Unlock()
		proxy.wg.Wait()
		proxy.upstream.CloseIdleConnections()
	})

	return proxy
}

func faultProxySerial(t *testing.T) *big.Int {
	t.Helper()

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), faultProxySerialBits))
	require.NoError(t, err)

	return serial
}

// env is base with the CLI subprocess routed through the proxy and trusting
// its authority. NO_PROXY is cleared so no host bypasses it.
func (p *faultProxy) env(base map[string]string) map[string]string {
	env := make(map[string]string, len(base))
	for key, value := range base {
		env[key] = value
	}
	proxyURL := "http://" + p.listener.Addr().String()
	env["HTTPS_PROXY"], env["https_proxy"] = proxyURL, proxyURL
	env["NO_PROXY"], env["no_proxy"] = "", ""
	env["SSL_CERT_FILE"] = p.caPath

	return env
}

// serveConnect takes a CONNECT, answers it, and serves the tunnel's requests
// over TLS with a leaf for the host the client asked for.
func (p *faultProxy) serveConnect(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodConnect {
		http.Error(w, "the fault proxy forwards HTTPS only", http.StatusMethodNotAllowed)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "cannot hijack the connection", http.StatusInternalServerError)
		return
	}
	conn, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	if !p.track(conn) {
		_ = conn.Close() //nolint:errcheck // the proxy is stopping; the close's error says nothing
		return
	}
	host := r.Host
	go func() {
		defer p.wg.Done()
		defer p.untrack(conn)
		p.serveTunnel(p.ctx, conn, host)
	}()
}

// track records conn and counts the goroutine that will serve it, under mu,
// so the cleanup's Wait never races an Add: once the cleanup has stopped
// the proxy (conns is nil), it refuses.
func (p *faultProxy) track(conn net.Conn) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.conns == nil {
		return false
	}
	p.conns[conn] = struct{}{}
	p.wg.Add(1)

	return true
}

func (p *faultProxy) untrack(conn net.Conn) {
	p.mu.Lock()
	delete(p.conns, conn)
	p.mu.Unlock()
	_ = conn.Close() //nolint:errcheck // the tunnel is over either way
}

// serveTunnel answers the client's requests in one tunnel, one at a time,
// until the client closes it or an answer is dropped.
func (p *faultProxy) serveTunnel(ctx context.Context, conn net.Conn, connectHost string) {
	if _, err := io.WriteString(conn, "HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{
		MinVersion:     tls.VersionTLS12,
		NextProtos:     []string{"http/1.1"},
		GetCertificate: p.leafFor,
	})
	reader := bufio.NewReader(tlsConn)
	for {
		req, err := http.ReadRequest(reader)
		if err != nil {
			return
		}
		if !p.forward(ctx, tlsConn, req, connectHost) {
			return
		}
	}
}

// forward sends req to OneDrive and writes the answer back, or, for a
// request dropAnswer selects, waits for OneDrive's answer and closes the
// tunnel without writing it. It reports whether the tunnel stays open.
func (p *faultProxy) forward(ctx context.Context, client net.Conn, req *http.Request, connectHost string) bool {
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close() //nolint:errcheck // fully read; nothing more to report
	if err != nil {
		return false
	}

	//nolint:gosec // a test proxy: it forwards to the host the CLI subprocess asked it to tunnel to
	outbound, err := http.NewRequestWithContext(ctx, req.Method, "https://"+connectHost+req.URL.RequestURI(),
		bytes.NewReader(body))
	if err != nil {
		return false
	}
	outbound.Header = req.Header.Clone()
	outbound.ContentLength = int64(len(body))
	drop := p.dropAnswer(req, body)

	resp, err := p.upstream.RoundTrip(outbound)
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }() // a read-only answer; its close reports nothing

	if drop {
		// OneDrive has answered: the create happened. The client hears
		// nothing but a closed connection.
		_, _ = io.Copy(io.Discard, resp.Body) //nolint:errcheck // the answer is dropped on purpose
		p.dropped.Add(1)

		return false
	}

	if err := writeAnswer(client, resp); err != nil {
		return false
	}

	return !req.Close && !resp.Close
}

// writeAnswer writes OneDrive's answer to the tunnel as HTTP/1.1, the
// protocol the tunnel speaks, framing a body of unknown length in chunks.
// OneDrive answers over HTTP/2, where a body need not state its length, and
// Response.Write sends such a body unframed, to be ended by closing the
// connection, which a tunnel kept open for the next request never does: the
// CLI read a created folder's answer until its stall watchdog gave up after
// two minutes, and the create the test is about was never sent.
func writeAnswer(client net.Conn, resp *http.Response) error {
	resp.Proto, resp.ProtoMajor, resp.ProtoMinor = "HTTP/1.1", 1, 1
	if resp.ContentLength < 0 && resp.Body != nil && resp.Body != http.NoBody {
		resp.TransferEncoding = []string{"chunked"}
	}

	return resp.Write(client) //nolint:wrapcheck // the tunnel's caller only needs to know it ended
}

// leafFor makes, once per host, a certificate for the host the client
// names, signed by the proxy's authority.
func (p *faultProxy) leafFor(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	host := hello.ServerName
	p.mu.Lock()
	defer p.mu.Unlock()

	if leaf, made := p.leaves[host]; made {
		return leaf, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("generate leaf key: %w", err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), faultProxySerialBits))
	if err != nil {
		return nil, fmt.Errorf("generate leaf serial: %w", err)
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		DNSNames:     []string{host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(faultProxyCertLifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.ca, &key.PublicKey, p.caKey)
	if err != nil {
		return nil, fmt.Errorf("sign leaf: %w", err)
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
	p.leaves[host] = leaf

	return leaf, nil
}

// dropFirstCreateAnswer selects the first file create for name: a POST to
// a folder's children whose body names it. Every later request, a retried
// create of the same name among them, is answered.
func dropFirstCreateAnswer(name string) func(req *http.Request, body []byte) bool {
	var once atomic.Bool

	return func(req *http.Request, body []byte) bool {
		if req.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/children") {
			return false
		}
		var create struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(body, &create) != nil || create.Name != name {
			return false
		}

		return once.CompareAndSwap(false, true)
	}
}

// Validates: R-5.6.15
//
// A file create reaches OneDrive but its answer is lost on the wire: the
// sync never learns the new item's ID. The pass that lost it, or the next
// one, finishes the upload into the file that create made (the claim rule):
// OneDrive ends with one file of that name holding the user's content, no
// conflict copy on either side, and a further pass has nothing to do. This
// is the live proof of what the chaos fault create:response-lost and
// create_answer_lost_test.go prove against fakes; it runs only in the
// nightly full tier.
func TestE2E_Sync_CreateWhoseAnswerWasLostFinishesIntoItsOwnFile(t *testing.T) {
	if runtime.GOOS != "linux" {
		// Go reads SSL_CERT_FILE only on Linux and the other Unix systems
		// without a platform verifier; on macOS the CLI would not trust the
		// proxy's authority.
		t.Skip("the fault proxy's authority is trusted through SSL_CERT_FILE, which Go reads on Linux")
	}
	registerLogDump(t)

	syncDir := t.TempDir()
	opsCfgPath := writeMinimalConfig(t)
	testFolder := fmt.Sprintf("e2e-lost-create-%d", time.Now().UnixNano())
	t.Cleanup(func() { cleanupRemoteFolder(t, testFolder) })
	cfgPath, env := writeSyncConfigForFolder(t, syncDir, testFolder)

	localDir := filepath.Join(syncDir, testFolder)
	require.NoError(t, os.MkdirAll(localDir, 0o700))
	content := []byte(fmt.Sprintf("the user's content for %s\n", testFolder))
	require.NoError(t, os.WriteFile(filepath.Join(localDir, lostCreateFileName), content, 0o600))

	proxy := startFaultProxy(t, dropFirstCreateAnswer(lostCreateFileName))
	// Single pass by design: this is the pass whose create loses its answer;
	// it may finish the upload itself or leave it for the next pass.
	_, stderr, syncErr := runCLIWithConfigAllowError(t, cfgPath, proxy.env(env), "sync", "--upload-only")
	require.Equal(t, int32(1), proxy.dropped.Load(), "the proxy dropped the create's answer once")
	t.Logf("the pass that lost the answer ended with error %v: %s", syncErr, printable(stderr))

	runSyncThroughTransientFailures(t, cfgPath, env, "--upload-only")

	remotePath := "/" + testFolder + "/" + lostCreateFileName
	waitForRemoteStatSize(t, opsCfgPath, nil, remotePath, int64(len(content)), pollTimeout)
	assertRemoteContentEquals(t, opsCfgPath, remotePath, content)
	assert.Equal(t, []string{lostCreateFileName}, printableNames(remoteFileNames(t, opsCfgPath, nil, "/"+testFolder)),
		"one file of that name on OneDrive, no second copy")

	conflicts, err := filepath.Glob(filepath.Join(localDir, "*conflict*"))
	require.NoError(t, err)
	assert.Empty(t, conflicts, "no conflict copy locally")
	entries, err := os.ReadDir(localDir)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "the local folder holds the user's file alone")

	// Single pass by design: the pass after convergence must find nothing.
	out, _ := runSyncPass(t, cfgPath, env, "--upload-only")
	assertOutputContains(t, out, "Up to date", "the recovered create leaves nothing for the next sync")
}
