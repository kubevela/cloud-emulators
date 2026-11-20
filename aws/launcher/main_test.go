/*
Copyright 2026 The KubeVela Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// startProxy serves a plain listener that forwards to upstream with cfg.
func startProxy(t *testing.T, upstream string, cfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go serve(ln, upstream, cfg)
	return ln.Addr().String()
}

func tlsUpstream(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Host", r.Host)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+string(body))
	}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func TestForwardsPlainHTTPToTheTLSUpstreamUnchanged(t *testing.T) {
	srv, pool := tlsUpstream(t)
	addr := startProxy(t, srv.Listener.Addr().String(), &tls.Config{RootCAs: pool})

	req, _ := http.NewRequest(http.MethodPost, "http://"+addr+"/__admin/health", strings.NewReader("payload"))
	req.Host = "aws-emulator.aws-emulator.svc.cluster.local:4566"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if got, want := string(body), "POST /__admin/health payload"; got != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
	// The Host header a client signed must reach the emulator as sent.
	if got := resp.Header.Get("X-Seen-Host"); got != req.Host {
		t.Fatalf("upstream saw Host %q, want %q", got, req.Host)
	}
}

func TestRefusesAnUpstreamWhoseCertificateItDoesNotTrust(t *testing.T) {
	srv, _ := tlsUpstream(t)
	addr := startProxy(t, srv.Listener.Addr().String(), &tls.Config{RootCAs: x509.NewCertPool()})

	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err == nil {
		resp.Body.Close()
		t.Fatalf("got status %d through a proxy that should not trust the upstream", resp.StatusCode)
	}
}

func TestWaitForCAReadsTheCertificateOnceItAppears(t *testing.T) {
	srv, _ := tlsUpstream(t)
	path := filepath.Join(t.TempDir(), "ca.pem")
	go func() {
		time.Sleep(300 * time.Millisecond)
		_ = os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw}), 0o600)
	}()
	pool, err := waitForCA(context.Background(), path, 5*time.Second, make(chan error))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Certificate().Verify(x509.VerifyOptions{Roots: pool}); err != nil {
		t.Fatalf("the pool does not hold the written certificate: %v", err)
	}
}

func TestWaitForCAGivesUp(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "ca.pem")

	if _, err := waitForCA(context.Background(), missing, 300*time.Millisecond, make(chan error)); err == nil || !strings.Contains(err.Error(), "no certificate") {
		t.Fatalf("after the wait ran out: %v", err)
	}

	exited := make(chan error, 1)
	exited <- io.ErrUnexpectedEOF
	if _, err := waitForCA(context.Background(), missing, 5*time.Second, exited); err == nil || !strings.Contains(err.Error(), "exited before") {
		t.Fatalf("after the emulator exited: %v", err)
	}

	// A file that is not a certificate is not accepted as one.
	garbage := filepath.Join(t.TempDir(), "ca.pem")
	_ = os.WriteFile(garbage, []byte("not a certificate"), 0o600)
	if _, err := waitForCA(context.Background(), garbage, 300*time.Millisecond, make(chan error)); err == nil {
		t.Fatal("accepted a file that holds no certificate")
	}
}
