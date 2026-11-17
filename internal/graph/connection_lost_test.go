package graph

import (
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/tonimelisma/onedrive-go/internal/driveid"
)

// The HTTP/2 framing the unanswering server speaks (RFC 9113).
const (
	http2PrefaceLen     = 24 // "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
	http2FrameHeaderLen = 9
	http2FrameHeaders   = 0x1
	http2FramePing      = 0x6
	http2FlagAck        = 0x1
	http2PingPayloadLen = 8
	// quietBeforePing is how long the test transport lets a connection go
	// without a frame before it pings.
	quietBeforePing = 5 * time.Millisecond
	// unansweredPingTimeout is how long the test transport waits for a
	// ping's answer before it closes the connection. It is the margin a
	// server that does answer pings has for its answer.
	unansweredPingTimeout = 100 * time.Millisecond
	// unansweringServerSafetyNet ends the server's reads should the client
	// never close the connection.
	unansweringServerSafetyNet = 10 * time.Second
)

// newUnansweringHTTP2Server serves HTTP/2 over TLS on loopback: it reads the
// client's preface, sends its SETTINGS and then only reads, answering no
// ping. With answerHeaders it answers pings until it has answered the first
// request's headers, so the connection is lost only after the answer has
// begun, then sends no body and answers nothing more. It returns the server's
// URL and a client whose transport pings a connection that has been quiet for
// quietBeforePing and closes it when the ping goes unanswered for
// unansweredPingTimeout, as the production transports do after their own
// bounds.
func newUnansweringHTTP2Server(t *testing.T, answerHeaders bool) (string, *http.Client) {
	t.Helper()

	certSource := httptest.NewUnstartedServer(http.NotFoundHandler())
	certSource.StartTLS()
	t.Cleanup(certSource.Close)
	roots := x509.NewCertPool()
	roots.AddCert(certSource.Certificate())

	serverTLS := &tls.Config{
		Certificates: certSource.TLS.Certificates, NextProtos: []string{"h2"}, MinVersion: tls.VersionTLS12,
	}
	var listenConfig net.ListenConfig
	listener, err := listenConfig.Listen(t.Context(), "tcp", "127.0.0.1:0")
	require.NoError(t, err)

	served := make(chan struct{})
	go func() {
		defer close(served)
		conn, acceptErr := listener.Accept()
		if acceptErr != nil {
			return
		}
		defer func() { assert.NoError(t, conn.Close()) }()
		if !assert.NoError(t, conn.SetReadDeadline(time.Now().Add(unansweringServerSafetyNet))) {
			return
		}
		serveWithoutAnsweringPings(tls.Server(conn, serverTLS), answerHeaders)
	}()
	t.Cleanup(func() {
		assert.NoError(t, listener.Close())
		<-served
	})

	transport := &http.Transport{
		TLSClientConfig:   &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
		ForceAttemptHTTP2: true,
		HTTP2:             &http.HTTP2Config{SendPingTimeout: quietBeforePing, PingTimeout: unansweredPingTimeout},
	}
	t.Cleanup(transport.CloseIdleConnections)

	return "https://" + listener.Addr().String(), &http.Client{Transport: transport}
}

// serveWithoutAnsweringPings reads frames until the client closes the
// connection or the connection's read deadline passes. With answerHeaders it
// answers the first request's headers, and the pings that come before them.
func serveWithoutAnsweringPings(conn net.Conn, answerHeaders bool) {
	if _, err := io.ReadFull(conn, make([]byte, http2PrefaceLen)); err != nil {
		return
	}
	// A SETTINGS frame with no settings: the server's half of the preface.
	emptySettings := []byte{0, 0, 0, 0x4, 0, 0, 0, 0, 0}
	if _, err := conn.Write(emptySettings); err != nil {
		return
	}
	// The answer to stream 1: ":status: 200" (HPACK static index 8) with
	// END_HEADERS and without END_STREAM, so a body is to follow.
	statusOK := []byte{0, 0, 1, http2FrameHeaders, 0x4, 0, 0, 0, 1, 0x88}

	header := make([]byte, http2FrameHeaderLen)
	for {
		if _, err := io.ReadFull(conn, header); err != nil {
			return
		}
		length := int64(header[0])<<16 | int64(header[1])<<8 | int64(header[2])
		isPing := header[3] == http2FramePing && header[4]&http2FlagAck == 0 && length == http2PingPayloadLen
		if isPing && answerHeaders {
			// The ACK of a PING carries the PING's payload back.
			ack := []byte{0, 0, http2PingPayloadLen, http2FramePing, http2FlagAck, 0, 0, 0, 0}
			ack = append(ack, make([]byte, http2PingPayloadLen)...)
			if _, err := io.ReadFull(conn, ack[http2FrameHeaderLen:]); err != nil {
				return
			}
			if _, err := conn.Write(ack); err != nil {
				return
			}

			continue
		}
		if _, err := io.CopyN(io.Discard, conn, length); err != nil {
			return
		}
		if header[3] == http2FrameHeaders && answerHeaders {
			answerHeaders = false
			if _, err := conn.Write(statusOK); err != nil {
				return
			}
		}
	}
}

// unansweringServerClient is a graph client of newUnansweringHTTP2Server.
func unansweringServerClient(t *testing.T, answerHeaders bool) *Client {
	t.Helper()

	serverURL, httpClient := newUnansweringHTTP2Server(t, answerHeaders)

	return MustNewClient(serverURL, httpClient, staticToken("t"), slog.New(slog.DiscardHandler), "test-agent")
}

// A request on an HTTP/2 connection the health check closed before the answer
// began carries ErrConnectionLost. The error comes from the real HTTP/2
// client, so this test also pins the standard library's text that graph
// recognizes the closed connection by: a Go release that rewords it fails
// here.
//
// Validates: R-6.8.20
func TestGetItem_AConnectionLostBeforeTheAnswerIsTyped(t *testing.T) {
	t.Parallel()

	_, err := unansweringServerClient(t, false).GetItem(t.Context(), driveid.New("d"), "item-on-a-lost-connection")

	require.ErrorIs(t, err, ErrConnectionLost)
	var urlErr *url.Error
	require.ErrorAs(t, err, &urlErr, "the request failed before its answer")
}

// The same closed connection reaches a body being read outside any
// *url.Error, and carries ErrConnectionLost there too.
//
// Validates: R-6.8.20
func TestGetItem_AConnectionLostDuringTheBodyIsTyped(t *testing.T) {
	t.Parallel()

	_, err := unansweringServerClient(t, true).GetItem(t.Context(), driveid.New("d"), "item-on-a-lost-connection")

	require.ErrorIs(t, err, ErrConnectionLost)
	var readErr *ResponseReadError
	require.ErrorAs(t, err, &readErr, "the answer had begun")
}
