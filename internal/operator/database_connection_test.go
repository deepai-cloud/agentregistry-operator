package operator

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func TestDatabaseConnectionRequiresDriverURIScheme(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql", "Postgres", "POSTGRES", "PostgreSQL", "POSTGRESQL"} {
		t.Run(scheme, func(t *testing.T) {
			req := fixture()
			desired, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			db := object(req.Related["Secret.v1"]["database"]["data"])
			original := db["url"]
			connection := scheme + "://alice:never-return-this@postgres.tenant-a.svc.cluster.local:5432/catalog?sslmode=disable"
			db["url"] = base64.StdEncoding.EncodeToString([]byte(connection))
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			valid := scheme == "postgres" || scheme == "postgresql"
			wantStatus, wantReplicas := "False", float64(0)
			if valid {
				wantStatus, wantReplicas = "True", 1
			}
			dependency := condition(got, "DependenciesReady")
			if dependency.Status != wantStatus || number(object(find(t, got, "Deployment")["spec"])["replicas"]) != wantReplicas {
				t.Fatalf("URL scheme validation disagrees with the database driver: %+v", got.Status)
			}
			if !valid && (!strings.Contains(dependency.Message, "lowercase") || condition(got, "Ready").Reason != "DependencyNotReady") {
				t.Fatalf("missing actionable URL scheme failure: %+v", got.Status)
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "never-return-this") {
				t.Fatal("URL scheme validation leaked credentials")
			}
			if string(secretData(req.Related["Secret.v1"]["database"], "url")) != connection || len(got.Children) != len(desired.Children) {
				t.Fatal("URL scheme validation changed the supplied Secret or dropped managed children")
			}
			db["url"] = original
			repaired, err := Sync(req, config())
			if err != nil || condition(repaired, "DependenciesReady").Status != "True" || !reflect.DeepEqual(repaired.Children, desired.Children) {
				t.Fatal("restoring the valid URL must resume the original workload configuration")
			}
		})
	}
}

func TestDatabaseConnectionOptionsFailBeforeStartup(t *testing.T) {
	ca := rotationCertificate(t, true)["tls.crt"].(string)
	invalidCA := base64.StdEncoding.EncodeToString([]byte("not a certificate"))
	for _, tc := range []struct {
		name, query, ca, message string
	}{
		{"default", "sslmode=disable", "", ""},
		{"timeout-zero", "sslmode=disable&connect_timeout=0", "", ""},
		{"timeout-seconds", "sslmode=disable&connect_timeout=5", "", ""},
		{"timeout-leading-zeros", "sslmode=disable&connect_timeout=005", "", ""},
		{"timeout-plus", "sslmode=disable&connect_timeout=%2B5", "", ""},
		{"timeout-duration-boundary", "sslmode=disable&connect_timeout=9223372036", "", ""},
		{"timeout-empty", "sslmode=disable&connect_timeout=", "", "connect_timeout"},
		{"timeout-key-only", "sslmode=disable&connect_timeout", "", "connect_timeout"},
		{"timeout-negative", "sslmode=disable&connect_timeout=-1", "", "connect_timeout"},
		{"timeout-fractional", "sslmode=disable&connect_timeout=1.5", "", "connect_timeout"},
		{"timeout-units", "sslmode=disable&connect_timeout=5s", "", "connect_timeout"},
		{"timeout-text", "sslmode=disable&connect_timeout=never-return-this", "", "connect_timeout"},
		{"timeout-space", "sslmode=disable&connect_timeout=+5", "", "connect_timeout"},
		{"timeout-duration-overflow", "sslmode=disable&connect_timeout=9223372037", "", "connect_timeout"},
		{"timeout-integer-overflow", "sslmode=disable&connect_timeout=9223372036854775808", "", "connect_timeout"},
		{"tls-required-no-ca", "sslmode=require", "", ""},
		{"tls-required-ca", "sslmode=require&sslrootcert=/database/ca.crt", ca, ""},
		{"tls-required-missing-ca", "sslmode=require&sslrootcert=/database/ca.crt", "", "ca.crt"},
		{"tls-required-invalid-ca", "sslmode=require&sslrootcert=/database/ca.crt", invalidCA, "ca.crt"},
		{"tls-disabled-ca", "sslmode=disable&sslrootcert=/database/ca.crt", ca, ""},
		{"tls-disabled-missing-ca", "sslmode=disable&sslrootcert=/database/ca.crt", "", "ca.crt"},
		{"tls-disabled-invalid-ca", "sslmode=disable&sslrootcert=/database/ca.crt", invalidCA, "ca.crt"},
		{"verify-full-ca", "sslmode=verify-full&sslrootcert=/database/ca.crt", ca, ""},
		{"verify-full-missing-root", "sslmode=verify-full", ca, "sslrootcert"},
		{"verify-full-missing-ca", "sslmode=verify-full&sslrootcert=/database/ca.crt", "", "ca.crt"},
		{"verify-full-invalid-ca", "sslmode=verify-full&sslrootcert=/database/ca.crt", invalidCA, "ca.crt"},
		{"unreferenced-invalid-ca", "sslmode=disable", invalidCA, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixture()
			db := object(req.Related["Secret.v1"]["database"]["data"])
			db["url"] = base64.StdEncoding.EncodeToString([]byte("postgres://alice:never-return-this@postgres.tenant-a.svc.cluster.local:5432/catalog?" + tc.query))
			if tc.ca != "" {
				db["ca.crt"] = tc.ca
			}
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Children) != 4 {
				t.Fatal("invalid connection must not drop managed children")
			}
			wantStatus, wantReplicas := "True", float64(1)
			if tc.message != "" {
				wantStatus, wantReplicas = "False", 0
			}
			dependency := condition(got, "DependenciesReady")
			if dependency.Status != wantStatus || asMap(t, find(t, got, "Deployment")["spec"])["replicas"] != wantReplicas {
				t.Fatalf("incorrect connection validation: %+v", got.Status)
			}
			if tc.message != "" && (dependency.Reason != "InvalidOrMissingSecret" || !strings.Contains(dependency.Message, tc.message) || condition(got, "Ready").Reason != "DependencyNotReady") {
				t.Fatalf("missing actionable dependency failure: %+v", got.Status)
			}
			data, err := json.Marshal(got)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "never-return-this") {
				t.Fatal("connection validation leaked Secret contents")
			}
		})
	}
}

func TestDatabaseConnectionRequiresExplicitDatabase(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		for _, tc := range []struct {
			name, path string
			valid      bool
		}{
			{"absent", "", false},
			{"root", "/", false},
			{"double-slash", "//", false},
			{"triple-slash", "///", false},
			{"encoded-slash", "/%2F", false},
			{"mixed-encoded-slashes", "/%2f%2F", false},
			{"named", "/catalog", true},
			{"leading-slashes", "//catalog", true},
			{"encoded-name", "/%63atalog", true},
			{"name-containing-slash", "/catalog%2Farchive", true},
		} {
			t.Run(scheme+"/"+tc.name, func(t *testing.T) {
				req := fixture()
				db := object(req.Related["Secret.v1"]["database"]["data"])
				db["url"] = base64.StdEncoding.EncodeToString([]byte(scheme + "://alice:never-return-this@postgres.tenant-a.svc.cluster.local:5432" + tc.path + "?sslmode=disable"))
				got, err := Sync(req, config())
				if err != nil {
					t.Fatal(err)
				}
				wantStatus, wantReplicas := "False", float64(0)
				if tc.valid {
					wantStatus, wantReplicas = "True", 1
				}
				dependency := condition(got, "DependenciesReady")
				if dependency.Status != wantStatus || asMap(t, find(t, got, "Deployment")["spec"])["replicas"] != wantReplicas {
					t.Fatalf("incorrect database name validation: %+v", got.Status)
				}
				if !tc.valid && (dependency.Reason != "InvalidOrMissingSecret" || !strings.Contains(dependency.Message, "database") || condition(got, "Ready").Reason != "DependencyNotReady") {
					t.Fatalf("missing actionable dependency failure: %+v", got.Status)
				}
				if len(got.Children) != 4 {
					t.Fatal("invalid connection must not drop managed children")
				}
				data, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "never-return-this") {
					t.Fatal("database name validation leaked Secret contents")
				}
			})
		}
	}
}

func TestDatabaseConnectionRejectsNULAndRecovers(t *testing.T) {
	for _, scheme := range []string{"postgres", "postgresql"} {
		for _, tc := range []struct {
			name, username, password, database string
			valid                              bool
		}{
			{"nul-username", "\x00", "never-return-this", "catalog", false},
			{"nul-password", "alice", "\x00", "catalog", false},
			{"nul-database", "alice", "never-return-this", "\x00", false},
			{"username-startup-fields", "alice\x00application_name\x00never-return-this", "never-return-this", "catalog", false},
			{"database-startup-fields", "alice", "never-return-this", "catalog\x00application_name\x00never-return-this", false},
			{"trailing-nul-password", "alice", "never-return-this\x00", "catalog", false},
			{"embedded-nul-password", "alice", "never-return-this\x00suffix", "catalog", false},
			{"literal-percent-sequence", "alice%00", "never-return-this%00", "catalog%00", true},
			{"escaped-and-unicode", "élise@tenant", "never-return-this:/?@%", "catalog/équipe", true},
		} {
			t.Run(scheme+"/"+tc.name, func(t *testing.T) {
				req := fixture()
				desired, err := Sync(req, config())
				if err != nil {
					t.Fatal(err)
				}
				dep := find(t, desired, "Deployment")
				object(dep["metadata"])["generation"] = 1
				dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
				req.Children = observedChildren(t, desired, dep)
				req.Parent.Status.InstalledVersion = "0.3.3"
				db := object(req.Related["Secret.v1"]["database"]["data"])
				original := db["url"]
				connection := url.URL{Scheme: scheme, User: url.UserPassword(tc.username, tc.password), Host: "postgres.tenant-a.svc.cluster.local:5432", Path: "/" + tc.database, RawQuery: "sslmode=disable"}
				db["url"] = base64.StdEncoding.EncodeToString([]byte(connection.String()))
				got, err := Sync(req, config())
				if err != nil {
					t.Fatal(err)
				}
				wantStatus, wantReplicas := "False", float64(0)
				if tc.valid {
					wantStatus, wantReplicas = "True", 1
				}
				dependency := condition(got, "DependenciesReady")
				if dependency.Status != wantStatus || asMap(t, find(t, got, "Deployment")["spec"])["replicas"] != wantReplicas {
					t.Fatalf("incorrect NUL validation: %+v", got.Status)
				}
				if !tc.valid && (dependency.Reason != "InvalidOrMissingSecret" || !strings.Contains(dependency.Message, "NUL") || condition(got, "Ready").Reason != "DependencyNotReady") {
					t.Fatalf("missing actionable dependency failure: %+v", got.Status)
				}
				if got.Status.InstalledVersion != "0.3.3" || len(got.Children) != len(desired.Children) {
					t.Fatal("connection validation lost installed version or children")
				}
				data, err := json.Marshal(got)
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), "never-return-this") {
					t.Fatal("connection validation leaked Secret contents")
				}
				db["url"] = original
				repaired, err := Sync(req, config())
				if err != nil || condition(repaired, "DependenciesReady").Status != "True" || condition(repaired, "Ready").Status != "True" {
					t.Fatalf("restoring valid credentials did not recover: %v %+v", err, repaired.Status)
				}
			})
		}
	}
}

func TestDatabaseConnectionRepairResumesExistingDeployment(t *testing.T) {
	for _, connection := range []string{
		"/catalog?sslmode=disable&connect_timeout=5s",
		"/catalog?sslmode=require&sslrootcert=/database/ca.crt",
		"//?sslmode=disable",
		"/%2F?sslmode=disable",
	} {
		t.Run(connection, func(t *testing.T) {
			req := fixture()
			desired, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			dep := find(t, desired, "Deployment")
			object(dep["metadata"])["generation"] = 1
			dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
			req.Children = observedChildren(t, desired, dep)
			req.Parent.Status.InstalledVersion = "0.3.3"
			db := object(req.Related["Secret.v1"]["database"]["data"])
			original := db["url"]
			db["url"] = base64.StdEncoding.EncodeToString([]byte("postgres://alice:never-return-this@postgres.tenant-a.svc.cluster.local:5432" + connection))
			failed, err := Sync(req, config())
			if err != nil || condition(failed, "Ready").Reason != "DependencyNotReady" || asMap(t, find(t, failed, "Deployment")["spec"])["replicas"] != float64(0) {
				t.Fatalf("invalid connection did not stop an existing deployment: %v %+v", err, failed.Status)
			}
			if failed.Status.InstalledVersion != "0.3.3" || len(failed.Children) != len(desired.Children) {
				t.Fatal("connection failure lost installed version or children")
			}
			db["url"] = original
			repaired, err := Sync(req, config())
			if err != nil || condition(repaired, "DependenciesReady").Status != "True" || condition(repaired, "Ready").Status != "True" {
				t.Fatalf("restoring valid connection settings did not recover: %v %+v", err, repaired.Status)
			}
		})
	}
}
