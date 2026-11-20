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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLauncher(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "launcher")
}

var _ = Describe("the launcher", func() {
	var (
		upstream *httptest.Server
		trusted  *x509.CertPool
	)

	// startProxy serves a plain listener that forwards to the upstream with cfg.
	startProxy := func(cfg *tls.Config) string {
		GinkgoHelper()
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		Expect(err).NotTo(HaveOccurred())
		DeferCleanup(ln.Close)
		go serve(ln, upstream.Listener.Addr().String(), cfg)
		return ln.Addr().String()
	}

	BeforeEach(func() {
		// Stands in for the emulator: TLS with a certificate of its own, echoing
		// what it received.
		upstream = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			w.Header().Set("X-Seen-Host", r.Host)
			_, _ = io.WriteString(w, r.Method+" "+r.URL.Path+" "+string(body))
		}))
		DeferCleanup(upstream.Close)
		trusted = x509.NewCertPool()
		trusted.AddCert(upstream.Certificate())
	})

	Describe("forwarding", func() {
		It("passes a plain HTTP request to the TLS upstream unchanged", func() {
			addr := startProxy(&tls.Config{RootCAs: trusted})

			req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/__admin/health", strings.NewReader("payload"))
			Expect(err).NotTo(HaveOccurred())
			req.Host = "aws-emulator.aws-emulator.svc.cluster.local:4566"
			resp, err := http.DefaultClient.Do(req)
			Expect(err).NotTo(HaveOccurred())
			DeferCleanup(resp.Body.Close)

			Expect(io.ReadAll(resp.Body)).To(BeEquivalentTo("POST /__admin/health payload"))
			// The Host header a client signed must reach the emulator as sent.
			Expect(resp.Header.Get("X-Seen-Host")).To(Equal(req.Host))
		})

		It("refuses an upstream whose certificate it does not trust", func() {
			addr := startProxy(&tls.Config{RootCAs: x509.NewCertPool()})

			client := &http.Client{Timeout: 3 * time.Second}
			resp, err := client.Get("http://" + addr + "/")
			if err == nil {
				_ = resp.Body.Close()
			}
			Expect(err).To(HaveOccurred(), "a request went through a proxy that should not trust the upstream")
		})
	})

	Describe("waiting for the emulator's certificate", func() {
		var path string

		BeforeEach(func() {
			path = filepath.Join(GinkgoT().TempDir(), "ca.pem")
		})

		It("reads the certificate once it appears", func(ctx SpecContext) {
			go func() {
				defer GinkgoRecover()
				time.Sleep(300 * time.Millisecond)
				block := &pem.Block{Type: "CERTIFICATE", Bytes: upstream.Certificate().Raw}
				Expect(os.WriteFile(path, pem.EncodeToMemory(block), 0o600)).To(Succeed())
			}()

			pool, err := waitForCA(ctx, path, 5*time.Second, make(chan error))
			Expect(err).NotTo(HaveOccurred())
			_, err = upstream.Certificate().Verify(x509.VerifyOptions{Roots: pool})
			Expect(err).NotTo(HaveOccurred(), "the pool holds the certificate that was written")
		}, SpecTimeout(10*time.Second))

		It("gives up when no certificate appears in time", func(ctx SpecContext) {
			_, err := waitForCA(ctx, path, 300*time.Millisecond, make(chan error))
			Expect(err).To(MatchError(ContainSubstring("no certificate")))
		}, SpecTimeout(10*time.Second))

		It("gives up at once when the emulator exits first", func(ctx SpecContext) {
			exited := make(chan error, 1)
			exited <- io.ErrUnexpectedEOF

			_, err := waitForCA(ctx, path, 5*time.Second, exited)
			Expect(err).To(MatchError(ContainSubstring("exited before")))
		}, SpecTimeout(3*time.Second))

		It("does not accept a file that holds no certificate", func(ctx SpecContext) {
			Expect(os.WriteFile(path, []byte("not a certificate"), 0o600)).To(Succeed())

			_, err := waitForCA(ctx, path, 300*time.Millisecond, make(chan error))
			Expect(err).To(HaveOccurred())
		}, SpecTimeout(10*time.Second))
	})
})
