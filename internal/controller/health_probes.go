/*
Copyright 2026 bitkaio LLC.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0
*/

package controller

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

// probeTimeout bounds every individual network probe. Health checks run on a
// 30s cadence — each backing service must respond within this window or it is
// reported NotConnected for the cycle.
const probeTimeout = 3 * time.Second

// probeResult is what every probe returns.
type probeResult struct {
	Connected bool
	Reason    string
	Message   string
}

// ─── PostgreSQL ─────────────────────────────────────────────────────────────

// probeDatabase TCP-dials the configured Postgres endpoint. It does not test
// authentication — port reachability is sufficient to distinguish "operator
// can't reach the DB" from "DB rejected the credentials" (which surfaces in
// the Langfuse pod logs instead).
func probeDatabase(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) probeResult {
	host, port, err := resolveDatabaseEndpoint(ctx, c, instance)
	if err != nil {
		return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Cannot resolve Postgres endpoint: %v", err)}
	}
	return tcpProbe(ctx, host, port, "PostgreSQL")
}

func resolveDatabaseEndpoint(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) (string, string, error) {
	if instance.Spec.Database == nil {
		return "", "", errors.New("no database configured")
	}
	db := instance.Spec.Database
	switch {
	case db.CloudNativePG != nil:
		// CNPG exposes a -rw service for the primary.
		return fmt.Sprintf("%s-rw.%s.svc", db.CloudNativePG.ClusterRef.Name, instance.Namespace), "5432", nil
	case db.Managed != nil:
		// Operator-managed Postgres is delivered through CNPG too; secret has the URL.
		raw, err := readSecretValue(ctx, c, instance.Namespace, instance.Name+"-generated-secrets", "database-url")
		if err != nil {
			return "", "", err
		}
		return parsePostgresURL(raw)
	case db.External != nil:
		key := db.External.SecretRef.Keys["url"]
		if key == "" {
			key = "database_url"
		}
		raw, err := readSecretValue(ctx, c, instance.Namespace, db.External.SecretRef.Name, key)
		if err != nil {
			return "", "", err
		}
		return parsePostgresURL(raw)
	}
	return "", "", errors.New("no database mode set")
}

// parsePostgresURL extracts host and port from a postgres:// or postgresql:// URL.
//
// It deliberately does not use net/url: that rejects a bare '@' in the userinfo
// per RFC 3986, but Prisma and the migration Job's init container both split on
// the last '@', so a password containing one connects fine. Failing here would
// report a healthy instance as unreachable.
func parsePostgresURL(raw string) (string, string, error) {
	rest := strings.TrimSpace(raw)
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}

	// Authority only — a '@' in the path or query is not a credential.
	authority := rest
	if i := strings.IndexAny(authority, "/?"); i >= 0 {
		authority = authority[:i]
	}
	if i := strings.LastIndex(authority, "@"); i >= 0 {
		authority = authority[i+1:]
	}
	if authority == "" {
		return "", "", errors.New("postgres URL has no host")
	}

	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		host, port = strings.Trim(authority, "[]"), "5432"
	}
	if host == "" {
		return "", "", errors.New("postgres URL has no host")
	}
	// A non-numeric port means the authority was mis-split, and only '/' or '?'
	// in the credentials can do that: both end the authority, hiding the host
	// from this parser and from Prisma alike. Name those two and nothing else —
	// '@' and ':' are handled above, and listing them here sends people off to
	// re-encode a password that was never the problem.
	if _, err := strconv.Atoi(port); err != nil {
		return "", "", fmt.Errorf("invalid port %q — percent-encode '/' as %%2F and '?' as %%3F "+
			"in the credentials; '@' and ':' need no encoding", port)
	}
	return host, port, nil
}

// ─── ClickHouse ─────────────────────────────────────────────────────────────

// probeClickHouse issues an HTTP GET to /ping. ClickHouse responds with
// "Ok.\n" and 200 when the server is up; /ping does not require auth.
func probeClickHouse(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) probeResult {
	endpoint, err := resolveClickHouseURL(ctx, c, instance)
	if err != nil {
		return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Cannot resolve ClickHouse URL: %v", err)}
	}

	// Probe the origin, not the configured string: a URL carrying a path or
	// query would make /ping 404 and report Unreachable, which reads as a
	// network problem rather than the misconfiguration it is.
	origin, err := clickHouseOrigin(endpoint)
	if err != nil {
		return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Invalid ClickHouse URL: %v", err)}
	}

	// An HTTPS endpoint secured by a private CA would fail x509 verification
	// against the system trust store, so trust the instance's configured CA.
	var tlsConfig *tls.Config
	if strings.HasPrefix(origin, "https://") {
		tlsConfig, err = instanceTLSConfig(ctx, c, instance)
		if err != nil {
			return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Cannot build ClickHouse TLS config: %v", err)}
		}
	}
	return httpProbeTLS(ctx, origin+"/ping", "ClickHouse", tlsConfig)
}

// clickHouseOrigin reduces a configured ClickHouse URL to scheme://host:port,
// rejecting one that carries a path or query string. ClickHouse's HTTP
// interface selects a database with the CLICKHOUSE_DB env var (or a ?database=
// parameter), never a path segment — so a path breaks every application query
// too, not just this probe, and is worth reporting as a config error.
func clickHouseOrigin(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse URL: %w", err)
	}
	if u.Host == "" {
		return "", errors.New("no host")
	}
	if path := strings.Trim(u.Path, "/"); path != "" {
		return "", fmt.Errorf("must not contain a path (got %q) — "+
			"select the database with CLICKHOUSE_DB, not a URL path", u.Path)
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("must not contain a query string (got %q)", u.RawQuery)
	}
	return u.Scheme + "://" + u.Host, nil
}

func resolveClickHouseURL(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) (string, error) {
	if instance.Spec.ClickHouse == nil {
		return "", errors.New("no clickhouse configured")
	}
	ch := instance.Spec.ClickHouse
	switch {
	case ch.Managed != nil:
		return fmt.Sprintf("http://%s-clickhouse.%s.svc:8123", instance.Name, instance.Namespace), nil
	case ch.External != nil:
		key := ch.External.SecretRef.Keys["url"]
		if key == "" {
			key = "url"
		}
		raw, err := readSecretValue(ctx, c, instance.Namespace, ch.External.SecretRef.Name, key)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(raw, "/"), nil
	}
	return "", errors.New("no clickhouse mode set")
}

// ─── Redis ──────────────────────────────────────────────────────────────────

// probeRedis TCP-dials Redis and sends a PING. Verifying PONG is a strict
// liveness check — a half-open socket on the load balancer wouldn't pass.
func probeRedis(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) probeResult {
	host, port, err := resolveRedisEndpoint(ctx, c, instance)
	if err != nil {
		return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Cannot resolve Redis endpoint: %v", err)}
	}

	dialer := &net.Dialer{Timeout: probeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("Redis dial failed: %v", err)}
	}
	defer func() { _ = conn.Close() }()
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > probeTimeout {
		deadline = time.Now().Add(probeTimeout)
	}
	_ = conn.SetDeadline(deadline)

	if _, err := conn.Write([]byte("*1\r\n$4\r\nPING\r\n")); err != nil {
		return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("Redis PING write failed: %v", err)}
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil {
		return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("Redis PING read failed: %v", err)}
	}
	resp := string(buf[:n])
	// "+PONG\r\n" on success, "-NOAUTH …" if the server demands AUTH but the
	// socket is healthy. Both prove the listener is responsive.
	if strings.HasPrefix(resp, "+PONG") || strings.HasPrefix(resp, "-NOAUTH") {
		return probeResult{Connected: true, Reason: "Connected", Message: "Redis responded to PING"}
	}
	return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("Redis returned unexpected response: %q", resp)}
}

func resolveRedisEndpoint(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) (string, string, error) {
	if instance.Spec.Redis == nil {
		return "", "", errors.New("no redis configured")
	}
	r := instance.Spec.Redis
	switch {
	case r.Managed != nil:
		return fmt.Sprintf("%s-redis.%s.svc", instance.Name, instance.Namespace), "6379", nil
	case r.External != nil:
		hostKey := r.External.SecretRef.Keys["host"]
		if hostKey == "" {
			hostKey = "host"
		}
		portKey := r.External.SecretRef.Keys["port"]
		if portKey == "" {
			portKey = "port"
		}
		host, err := readSecretValue(ctx, c, instance.Namespace, r.External.SecretRef.Name, hostKey)
		if err != nil {
			return "", "", err
		}
		port, err := readSecretValue(ctx, c, instance.Namespace, r.External.SecretRef.Name, portKey)
		if err != nil {
			// Port secret can be omitted; default to 6379.
			port = "6379"
		}
		return host, port, nil
	}
	return "", "", errors.New("no redis mode set")
}

// ─── Blob Storage ───────────────────────────────────────────────────────────

// probeBlobStorage TCP-dials the storage endpoint. For self-hosted MinIO this
// means the configured endpoint; for managed cloud providers it uses each
// provider's well-known endpoint hostname.
func probeBlobStorage(ctx context.Context, c client.Client, instance *v1alpha1.LangfuseInstance) probeResult {
	host, port, err := resolveBlobStorageEndpoint(instance)
	if err != nil {
		return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Cannot resolve blob storage endpoint: %v", err)}
	}
	// Suppress the unused client warning — c is part of the signature to keep
	// every probe shaped the same way, in case future probes need k8s reads.
	_ = c
	return tcpProbe(ctx, host, port, "Blob storage")
}

func resolveBlobStorageEndpoint(instance *v1alpha1.LangfuseInstance) (string, string, error) {
	if instance.Spec.BlobStorage == nil {
		return "", "", errors.New("no blob storage configured")
	}
	bs := instance.Spec.BlobStorage
	switch bs.Provider {
	case "s3":
		if bs.S3 == nil {
			return "", "", errors.New("s3 spec is empty")
		}
		if bs.S3.Endpoint != "" {
			return parseHTTPEndpoint(bs.S3.Endpoint)
		}
		// AWS S3 with no endpoint — use the regional public endpoint.
		region := bs.S3.Region
		if region == "" {
			region = "us-east-1"
		}
		return fmt.Sprintf("s3.%s.amazonaws.com", region), "443", nil
	case "azure":
		if bs.Azure == nil || bs.Azure.StorageAccountName == "" {
			return "", "", errors.New("azure spec missing storageAccountName")
		}
		return fmt.Sprintf("%s.blob.core.windows.net", bs.Azure.StorageAccountName), "443", nil
	case "gcs":
		return "storage.googleapis.com", "443", nil
	}
	return "", "", fmt.Errorf("unsupported blob storage provider %q", bs.Provider)
}

// parseHTTPEndpoint accepts an http[s]://host[:port] URL and returns (host, port).
// If port is omitted, defaults to 80 for http and 443 for https.
func parseHTTPEndpoint(raw string) (string, string, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", fmt.Errorf("parse endpoint URL: %w", err)
	}
	host := u.Hostname()
	if host == "" {
		return "", "", errors.New("endpoint URL has no host")
	}
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return host, port, nil
}

// ─── Shared probe helpers ───────────────────────────────────────────────────

// tcpProbe attempts a TCP connection within probeTimeout. Returns Connected
// when the dial succeeds.
func tcpProbe(ctx context.Context, host, port, label string) probeResult {
	dialer := &net.Dialer{Timeout: probeTimeout}
	conn, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort(host, port))
	if err != nil {
		return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("%s dial failed: %v", label, err)}
	}
	_ = conn.Close()
	return probeResult{Connected: true, Reason: "Connected", Message: fmt.Sprintf("%s is reachable", label)}
}

// httpProbe issues a GET against the URL with probeTimeout, using the system
// trust store for TLS.
func httpProbe(ctx context.Context, target, label string) probeResult {
	return httpProbeTLS(ctx, target, label, nil)
}

// httpProbeTLS issues a GET against the URL with probeTimeout. Returns
// Connected for any 2xx response. A non-nil tlsConfig overrides the trust store,
// which is required for endpoints secured by a private CA.
func httpProbeTLS(ctx context.Context, target, label string, tlsConfig *tls.Config) probeResult {
	reqCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, target, nil)
	if err != nil {
		return probeResult{Reason: "ConfigError", Message: fmt.Sprintf("Cannot build request: %v", err)}
	}
	httpClient := &http.Client{Timeout: probeTimeout}
	if tlsConfig != nil {
		httpClient.Transport = &http.Transport{TLSClientConfig: tlsConfig}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("%s request failed: %v", label, err)}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return probeResult{Reason: "Unreachable", Message: fmt.Sprintf("%s returned HTTP %d", label, resp.StatusCode)}
	}
	// Drain a few bytes so the keep-alive socket can be reused.
	_, _ = io.CopyN(io.Discard, resp.Body, 64)
	return probeResult{Connected: true, Reason: "Connected", Message: fmt.Sprintf("%s responded HTTP %d", label, resp.StatusCode)}
}

// readSecretValue fetches a single key from a Secret in the target namespace.
func readSecretValue(ctx context.Context, c client.Client, namespace, name, key string) (string, error) {
	secret := &corev1.Secret{}
	err := c.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, secret)
	if err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("secret %s/%s not found", namespace, name)
		}
		return "", fmt.Errorf("get secret %s/%s: %w", namespace, name, err)
	}
	v, ok := secret.Data[key]
	if !ok {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, name, key)
	}
	return strings.TrimSpace(string(v)), nil
}
