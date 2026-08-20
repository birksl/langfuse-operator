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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/PalenaAI/langfuse-operator/api/v1alpha1"
)

func newFakeClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

// testSecret builds a fixture Secret matching the test instance's namespace
// and secret name. Probe-resolver tests all reference the same names.
func testSecret(data map[string]string) *corev1.Secret {
	bd := map[string][]byte{}
	for k, v := range data {
		bd[k] = []byte(v)
	}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
		Data:       bd,
	}
}

// ─── parsePostgresURL ───────────────────────────────────────────────────────

func TestParsePostgresURL(t *testing.T) {
	cases := []struct {
		name, in, host, port string
		wantErr              bool
	}{
		{"plain", "postgres://u:p@db:5432/x", "db", "5432", false},
		{"alt-scheme", "postgresql://u:p@db.example/x", "db.example", "5432", false},
		{"no-scheme", "u:p@db:6543/x", "db", "6543", false},
		{"no-port", "postgres://u:p@db/x", "db", "5432", false},
		{"only-host", "postgres://db", "db", "5432", false},
		{"empty", "", "", "", true},

		// net/url rejects a bare '@' in userinfo, but Prisma and the migration
		// Job's init container both split on the last '@', so these connect
		// fine — reporting them unreachable marks a healthy instance Degraded.
		{"unencoded @ in password", "postgresql://langfuse:secret@here@db-01-rw.database.svc/langfuse",
			"db-01-rw.database.svc", "5432", false},
		{"unencoded @ with port", "postgres://u:p@ss@db:6543/x", "db", "6543", false},
		{"percent-encoded @ still works", "postgres://u:p%40ss@db:5432/x", "db", "5432", false},
		{"colon in password", "postgres://u:p:ass@db:5432/x", "db", "5432", false},
		{"percent-encoded reserved chars are fine", "postgres://u:p%2Fa%23ss@db:5432/x", "db", "5432", false},

		// '/', '?' and '#' each end the authority under the WHATWG rules Prisma
		// follows, so the host after them is invisible to it — accepting these
		// would leave a green probe against a database Langfuse cannot reach.
		{"unencoded slash in password is rejected, not mis-parsed", "postgres://u:p/ass@db/x", "", "", true},
		{"unencoded question mark in password is rejected", "postgres://u:p?ass@db/x", "", "", true},
		{"unencoded hash in password is rejected", "postgres://u:p#ass@db/x", "", "", true},

		// Prisma percent-decodes the credentials, so a stray '%' does not mean
		// what the user typed. A well-formed escape is left alone.
		{"lone percent in password is rejected", "postgres://u:p%ss@db:5432/x", "", "", true},
		{"trailing percent in password is rejected", "postgres://u:pass%@db:5432/x", "", "", true},

		// A '@' after the authority belongs to the path or query, not the
		// credentials, so it must not be mistaken for a separator.
		{"@ in path only", "postgres://db:5432/tenant@a", "db", "5432", false},
		{"@ in query only", "postgres://db/x?opts=a@b", "db", "5432", false},

		{"ipv6 with port", "postgres://u:p@[fd00::1]:5432/x", "fd00::1", "5432", false},
		{"ipv6 without port", "postgres://u:p@[fd00::1]/x", "fd00::1", "5432", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := parsePostgresURL(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.host || port != tc.port {
				t.Fatalf("got host=%q port=%q, want host=%q port=%q", host, port, tc.host, tc.port)
			}
		})
	}
}

// The parser accepts '@' and ':', so an error that tells the user to encode
// them sends them to change a password that was never the problem — which is
// exactly how a real instance was misdiagnosed. The advice has to name only the
// characters that actually end the authority.
func TestParsePostgresURL_AdviceNamesOnlyTheCharactersThatBreak(t *testing.T) {
	_, _, err := parsePostgresURL("postgres://u:pa/ss@db:5432/x")
	if err == nil {
		t.Fatal("an unencoded '/' in the password should be an error")
	}
	for _, want := range []string{"%2F", "%3F", "'@' and ':' need no encoding"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should contain %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "encode any /, @") {
		t.Errorf("error %q still tells the user to encode '@'", err)
	}
}

// ─── clickHouseOrigin ───────────────────────────────────────────────────────

func TestClickHouseOrigin(t *testing.T) {
	cases := []struct {
		name, in, want string
		wantErr        bool
		errContains    string
	}{
		{name: "plain", in: "http://ch:8123", want: "http://ch:8123"},
		{name: "trailing slash is harmless", in: "http://ch:8123/", want: "http://ch:8123"},
		{name: "https", in: "https://ch.example.com:8443", want: "https://ch.example.com:8443"},
		{name: "no port", in: "http://ch", want: "http://ch"},

		// The reported failure: a database name in the path made /ping 404, which
		// the probe reported as Unreachable — indistinguishable from a network
		// outage. It must name the misconfiguration instead.
		{name: "database in path", in: "http://ch:8123/langfuse",
			wantErr: true, errContains: "must not contain a path"},
		{name: "database as query", in: "http://ch:8123/?database=langfuse",
			wantErr: true, errContains: "must not contain a query"},
		{name: "no host", in: "not-a-url", wantErr: true, errContains: "no host"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := clickHouseOrigin(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got origin %q", got)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q should mention %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// ─── parseHTTPEndpoint ──────────────────────────────────────────────────────

func TestParseHTTPEndpoint(t *testing.T) {
	cases := []struct {
		in, host, port string
		wantErr        bool
	}{
		{"http://minio:9000", "minio", "9000", false},
		{"https://s3.eu-central-1.amazonaws.com", "s3.eu-central-1.amazonaws.com", "443", false},
		{"http://host", "host", "80", false},
		{"https://s3.shared-storage.svc.cluster.local:3900", "s3.shared-storage.svc.cluster.local", "3900", false},
	}
	for _, tc := range cases {
		host, port, err := parseHTTPEndpoint(tc.in)
		if tc.wantErr && err == nil {
			t.Fatalf("want error, got none for %q", tc.in)
		}
		if !tc.wantErr && (host != tc.host || port != tc.port) {
			t.Fatalf("%q: got host=%q port=%q, want host=%q port=%q", tc.in, host, port, tc.host, tc.port)
		}
	}
}

// ─── resolveBlobStorageEndpoint ─────────────────────────────────────────────

func TestResolveBlobStorageEndpoint(t *testing.T) {
	cases := []struct {
		name, host, port string
		instance         *v1alpha1.LangfuseInstance
		wantErr          bool
	}{
		{
			name: "s3-with-endpoint",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				BlobStorage: &v1alpha1.BlobStorageSpec{
					Provider: "s3",
					S3:       &v1alpha1.S3Spec{Endpoint: "http://minio:9000", Bucket: "x"},
				},
			}},
			host: "minio", port: "9000",
		},
		{
			name: "s3-aws-default-region",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				BlobStorage: &v1alpha1.BlobStorageSpec{
					Provider: "s3",
					S3:       &v1alpha1.S3Spec{Bucket: "x"},
				},
			}},
			host: "s3.us-east-1.amazonaws.com", port: "443",
		},
		{
			name: "s3-aws-explicit-region",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				BlobStorage: &v1alpha1.BlobStorageSpec{
					Provider: "s3",
					S3:       &v1alpha1.S3Spec{Region: "eu-west-2", Bucket: "x"},
				},
			}},
			host: "s3.eu-west-2.amazonaws.com", port: "443",
		},
		{
			name: "azure",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				BlobStorage: &v1alpha1.BlobStorageSpec{
					Provider: "azure",
					Azure:    &v1alpha1.AzureBlobSpec{StorageAccountName: "acme", ContainerName: "c"},
				},
			}},
			host: "acme.blob.core.windows.net", port: "443",
		},
		{
			name: "gcs",
			instance: &v1alpha1.LangfuseInstance{Spec: v1alpha1.LangfuseInstanceSpec{
				BlobStorage: &v1alpha1.BlobStorageSpec{Provider: "gcs"},
			}},
			host: "storage.googleapis.com", port: "443",
		},
		{
			name:     "no-blob",
			instance: &v1alpha1.LangfuseInstance{},
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			host, port, err := resolveBlobStorageEndpoint(tc.instance)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want error, got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if host != tc.host || port != tc.port {
				t.Fatalf("got %s:%s, want %s:%s", host, port, tc.host, tc.port)
			}
		})
	}
}

// ─── resolveDatabaseEndpoint ────────────────────────────────────────────────

func TestResolveDatabaseEndpoint_CNPG(t *testing.T) {
	c := newFakeClient(t)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Database: &v1alpha1.DatabaseSpec{
				CloudNativePG: &v1alpha1.CloudNativePGSpec{
					ClusterRef: v1alpha1.ObjectReference{Name: "pg-cluster"},
				},
			},
		},
	}
	host, port, err := resolveDatabaseEndpoint(context.Background(), c, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "pg-cluster-rw.ns.svc" || port != "5432" {
		t.Fatalf("got %s:%s", host, port)
	}
}

func TestResolveDatabaseEndpoint_External(t *testing.T) {
	c := newFakeClient(t,
		testSecret(map[string]string{"database-url": "postgres://u:p@pg.example:6543/db"}),
	)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Database: &v1alpha1.DatabaseSpec{
				External: &v1alpha1.ExternalDatabaseSpec{
					SecretRef: v1alpha1.SecretKeysRef{
						Name: "creds",
						Keys: map[string]string{"url": "database-url"},
					},
				},
			},
		},
	}
	host, port, err := resolveDatabaseEndpoint(context.Background(), c, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "pg.example" || port != "6543" {
		t.Fatalf("got %s:%s", host, port)
	}
}

func TestResolveDatabaseEndpoint_ExternalMissingSecret(t *testing.T) {
	c := newFakeClient(t)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Database: &v1alpha1.DatabaseSpec{
				External: &v1alpha1.ExternalDatabaseSpec{
					SecretRef: v1alpha1.SecretKeysRef{Name: "creds"},
				},
			},
		},
	}
	_, _, err := resolveDatabaseEndpoint(context.Background(), c, inst)
	if err == nil {
		t.Fatal("expected error for missing secret")
	}
}

// ─── resolveClickHouseURL ───────────────────────────────────────────────────

func TestResolveClickHouseURL_Managed(t *testing.T) {
	c := newFakeClient(t)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			ClickHouse: &v1alpha1.ClickHouseSpec{Managed: &v1alpha1.ManagedClickHouseSpec{}},
		},
	}
	got, err := resolveClickHouseURL(context.Background(), c, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://lf-clickhouse.ns.svc:8123" {
		t.Fatalf("got %q", got)
	}
}

func TestResolveClickHouseURL_External(t *testing.T) {
	c := newFakeClient(t,
		testSecret(map[string]string{"url": "http://ch.example:8123/"}),
	)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			ClickHouse: &v1alpha1.ClickHouseSpec{
				External: &v1alpha1.ExternalClickHouseSpec{
					SecretRef: v1alpha1.SecretKeysRef{Name: "creds", Keys: map[string]string{}},
				},
			},
		},
	}
	got, err := resolveClickHouseURL(context.Background(), c, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "http://ch.example:8123" {
		t.Fatalf("got %q (want trailing slash trimmed)", got)
	}
}

// ─── resolveRedisEndpoint ───────────────────────────────────────────────────

func TestResolveRedisEndpoint_External(t *testing.T) {
	c := newFakeClient(t,
		testSecret(map[string]string{"host": "redis.example", "port": "6380"}),
	)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Redis: &v1alpha1.RedisSpec{
				External: &v1alpha1.ExternalRedisSpec{
					SecretRef: v1alpha1.SecretKeysRef{Name: "creds", Keys: map[string]string{}},
				},
			},
		},
	}
	host, port, err := resolveRedisEndpoint(context.Background(), c, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "redis.example" || port != "6380" {
		t.Fatalf("got %s:%s", host, port)
	}
}

func TestResolveRedisEndpoint_ExternalMissingPortDefaultsTo6379(t *testing.T) {
	c := newFakeClient(t,
		testSecret(map[string]string{"host": "redis.example"}),
	)
	inst := &v1alpha1.LangfuseInstance{
		ObjectMeta: metav1.ObjectMeta{Name: "lf", Namespace: "ns"},
		Spec: v1alpha1.LangfuseInstanceSpec{
			Redis: &v1alpha1.RedisSpec{
				External: &v1alpha1.ExternalRedisSpec{
					SecretRef: v1alpha1.SecretKeysRef{Name: "creds", Keys: map[string]string{}},
				},
			},
		},
	}
	_, port, err := resolveRedisEndpoint(context.Background(), c, inst)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if port != "6379" {
		t.Fatalf("got port=%q, want 6379", port)
	}
}

// ─── Live probes (loopback) ─────────────────────────────────────────────────

// TestTCPProbe_Reachable starts a real listener so DialContext can succeed and
// proves Connected==true on success. Uses a free OS-allocated port.
func TestTCPProbe_Reachable(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	host, port, _ := net.SplitHostPort(l.Addr().String())

	res := tcpProbe(context.Background(), host, port, "test")
	if !res.Connected || res.Reason != "Connected" {
		t.Fatalf("got %+v", res)
	}
}

func TestTCPProbe_Unreachable(t *testing.T) {
	// Port 1 is reserved and never listens.
	res := tcpProbe(context.Background(), "127.0.0.1", "1", "test")
	if res.Connected || res.Reason != "Unreachable" {
		t.Fatalf("got %+v", res)
	}
}

func TestHTTPProbe_OK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("Ok.\n"))
	}))
	defer srv.Close()

	res := httpProbe(context.Background(), srv.URL+"/ping", "test")
	if !res.Connected {
		t.Fatalf("got %+v", res)
	}
}

func TestHTTPProbe_5xxReportsUnreachable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	res := httpProbe(context.Background(), srv.URL+"/ping", "test")
	if res.Connected || !strings.Contains(res.Message, "HTTP 500") {
		t.Fatalf("got %+v", res)
	}
}
