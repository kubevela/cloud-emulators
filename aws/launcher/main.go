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

// Command launcher runs AWS Emulator and offers it as plain HTTP.
//
// The emulator serves HTTPS only, on 127.0.0.1:9297, with a certificate it makes
// anew at every start, and by default accepts only the access keys it generated
// for that run. A controller in another pod can use neither. The launcher starts
// the emulator with request signing checks off, waits for the certificate of this
// run, then accepts plain TCP connections and forwards each one to the emulator
// over TLS, verifying that certificate. It adds nothing to and removes nothing
// from the bytes it carries.
//
// The launcher exits when the emulator does, so the container restarts as a whole
// and never forwards with a certificate from an earlier start.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"
)

const (
	upstreamAddr = "127.0.0.1:9297"
	caWait       = 60 * time.Second
)

func main() {
	log.SetFlags(0)
	log.SetPrefix("launcher: ")
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	bin := env("EMULATOR_BIN", "/opt/aws-emulator/bin/aws-emulator")
	port := env("EMULATOR_HTTP_PORT", "4566")
	auth := env("EMULATOR_AUTH", "off")
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("no home directory for the emulator's files: %w", err)
	}
	state := filepath.Join(home, ".aws", "emulator", "9297")
	// Files of an earlier start must not be mistaken for this one's.
	if err := os.RemoveAll(state); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()

	cmd := exec.Command(bin, append([]string{"--auth", auth}, os.Args[1:]...)...) // #nosec G204 -- the binary is fixed by the image
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start %s: %w", bin, err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	pool, err := waitForCA(ctx, filepath.Join(state, "ca.pem"), caWait, exited)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		_ = cmd.Process.Kill()
		return err
	}
	log.Printf("plain HTTP on :%s -> AWS Emulator on %s (auth %s, telemetry %s)", port, upstreamAddr, auth, env("EMULATOR_TELEMETRY", "on"))
	go serve(ln, upstreamAddr, &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12})

	select {
	case err := <-exited:
		return fmt.Errorf("the emulator exited: %v", err)
	case <-ctx.Done():
		_ = ln.Close()
		_ = cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-exited:
		case <-time.After(10 * time.Second):
			_ = cmd.Process.Kill()
		}
		return nil
	}
}

func env(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

// waitForCA returns the certificate pool the emulator wrote for this run. It
// gives up when the emulator exits, the context ends, or the wait runs out.
func waitForCA(ctx context.Context, path string, wait time.Duration, exited <-chan error) (*x509.CertPool, error) {
	deadline := time.After(wait)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if pem, err := os.ReadFile(path); err == nil && len(pem) > 0 { // #nosec G304 -- path is under the emulator's own state directory
			pool := x509.NewCertPool()
			if pool.AppendCertsFromPEM(pem) {
				return pool, nil
			}
		}
		select {
		case err := <-exited:
			return nil, fmt.Errorf("the emulator exited before it wrote %s: %v", path, err)
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline:
			return nil, fmt.Errorf("no certificate at %s after %s", path, wait)
		case <-tick.C:
		}
	}
}

// serve forwards every connection accepted on ln to upstream over TLS.
func serve(ln net.Listener, upstream string, cfg *tls.Config) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("accept: %v", err)
			continue
		}
		go forward(conn, upstream, cfg)
	}
}

// forward joins one client connection to one TLS connection to upstream, and
// closes each side's writing half when the other side has nothing more to send.
func forward(client net.Conn, upstream string, cfg *tls.Config) {
	defer client.Close()
	dialer := &tls.Dialer{NetDialer: &net.Dialer{Timeout: 5 * time.Second}, Config: cfg}
	up, err := dialer.Dial("tcp", upstream)
	if err != nil {
		log.Printf("connect to the emulator: %v", err)
		return
	}
	defer up.Close()
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(up, client)
		if c, ok := up.(*tls.Conn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(client, up)
		if c, ok := client.(*net.TCPConn); ok {
			_ = c.CloseWrite()
		}
		done <- struct{}{}
	}()
	<-done
	<-done
}
