package operator

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture() Request {
	p := Parent{APIVersion: "registry.deepai.cloud/v1alpha1", Kind: "AgentRegistry", Metadata: Metadata{Name: "catalog", Namespace: "tenant-a", UID: "parent-uid", Generation: 1}, Spec: Spec{Version: "0.3.3", Database: Database{SecretName: "database", Host: "postgres.tenant-a.svc.cluster.local"}, AuthSecretName: "auth"}}
	secret := func(name string, data map[string]string) Object {
		encoded := map[string]any{}
		for k, v := range data {
			encoded[k] = base64.StdEncoding.EncodeToString([]byte(v))
		}
		return Object{"apiVersion": "v1", "kind": "Secret", "metadata": map[string]any{"name": name, "namespace": "tenant-a", "uid": name + "-uid", "resourceVersion": "1"}, "data": encoded}
	}
	return Request{Parent: p, Related: map[string]map[string]Object{"Secret.v1": {"database": secret("database", map[string]string{"url": "postgres://alice:never-return-this@postgres.tenant-a.svc.cluster.local:5432/catalog?sslmode=disable"}), "auth": secret("auth", map[string]string{"username": "alice", "password": "tenant-password-long-enough", "jwt-key": strings.Repeat("a", 64)})}}}
}
func config() Config { return Config{GatewayImage: "agentregistry-operator:test"} }
func find(t *testing.T, r Response, kind string) Object {
	t.Helper()
	for _, o := range r.Children {
		if o["kind"] == kind {
			return o
		}
	}
	t.Fatalf("missing %s", kind)
	return nil
}
func asMap(t *testing.T, o any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(o)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
func condition(r Response, typ string) Condition {
	for _, c := range r.Status.Conditions {
		if c.Type == typ {
			return c
		}
	}
	return Condition{}
}
func TestCompleteDeterministicDesiredState(t *testing.T) {
	req := fixture()
	before, _ := json.Marshal(req)
	a, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	b, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("sync is nondeterministic")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("mutated observed state")
	}
	if len(a.Children) != 4 {
		t.Fatalf("want all four children, got %d", len(a.Children))
	}
	for _, kind := range []string{"Deployment", "Service", "ServiceAccount", "NetworkPolicy"} {
		find(t, a, kind)
	}
	out, _ := json.Marshal(a)
	for _, s := range []string{"never-return-this", "tenant-password-long-enough", strings.Repeat("a", 64)} {
		if strings.Contains(string(out), s) {
			t.Fatalf("Secret contents leaked: %s", s)
		}
	}
	if condition(a, "Ready").Status != "False" || a.Status.InstalledVersion != "" {
		t.Fatal("creation must not imply health")
	}
	dep := find(t, a, "Deployment")
	pod := asMap(t, asMap(t, asMap(t, dep["spec"])["template"])["spec"])
	if pod["automountServiceAccountToken"] != false {
		t.Fatal("token mounted")
	}
	if strings.Contains(string(out), "ClusterRole") {
		t.Fatal("runtime permissions")
	}
	svc := asMap(t, find(t, a, "Service")["spec"])
	if svc["type"] != "ClusterIP" {
		t.Fatal("must be private")
	}
}
func TestMissingOrInvalidDependencyNeverDropsChildren(t *testing.T) {
	for _, tc := range []string{"missing-db", "wrong-ns", "invalid-url", "missing-auth"} {
		t.Run(tc, func(t *testing.T) {
			req := fixture()
			base, _ := Sync(req, config())
			req.Children = map[string]map[string]Object{}
			for _, o := range base.Children {
				req.Children[o["kind"].(string)] = map[string]Object{"catalog": o}
			}
			switch tc {
			case "missing-db":
				delete(req.Related["Secret.v1"], "database")
			case "missing-auth":
				delete(req.Related["Secret.v1"], "auth")
			case "wrong-ns":
				req.Related["Secret.v1"]["database"]["metadata"].(map[string]any)["namespace"] = "tenant-b"
			case "invalid-url":
				req.Related["Secret.v1"]["database"]["data"].(map[string]any)["url"] = base64.StdEncoding.EncodeToString([]byte("postgres://u:p@other.tenant-b.svc.cluster.local/db?sslmode=disable"))
			}
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Children) != len(base.Children) {
				t.Fatal("dependency failure drops owned children")
			}
			if condition(got, "DependenciesReady").Status != "False" || condition(got, "Ready").Status != "False" {
				t.Fatal("unhealthy dependencies not reported")
			}
			if asMap(t, find(t, got, "Deployment")["spec"])["replicas"] != float64(0) {
				t.Fatal("invalid dependency must stop unsafe startup")
			}
		})
	}
}
func TestRelatedSelectionIsExactAndNamespaceLocal(t *testing.T) {
	req := fixture()
	got, err := Customize(req.Parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RelatedResources) != 1 {
		t.Fatal(got)
	}
	r := got.RelatedResources[0]
	if r.Namespace != "tenant-a" || r.Resource != "secrets" || !reflect.DeepEqual(r.Names, []string{"auth", "database"}) {
		t.Fatalf("unbounded dependency selector: %+v", r)
	}
}
func TestRejectsUnsafeSpecWithoutSyncResponse(t *testing.T) {
	cases := map[string]func(*Request){"version": func(r *Request) { r.Parent.Spec.Version = "0.4.0" }, "cross-namespace": func(r *Request) { r.Parent.Spec.Database.SecretName = "tenant-b/secret" }, "db-cross-namespace": func(r *Request) { r.Parent.Spec.Database.Host = "postgres.tenant-b.svc.cluster.local" }, "downgrade": func(r *Request) { r.Parent.Status.InstalledVersion = "0.3.3"; r.Parent.Spec.Version = "0.3.2" }, "name-length": func(r *Request) { r.Parent.Metadata.Name = strings.Repeat("a", 64) }, "host-injection": func(r *Request) { r.Parent.Spec.Database.Host = "127.0.0.1;bad" }, "invalid-resources": func(r *Request) {
		r.Parent.Spec.Resources = Resources{Requests: map[string]string{"cpu": "2"}, Limits: map[string]string{"cpu": "1"}}
	}}
	for name, f := range cases {
		t.Run(name, func(t *testing.T) {
			r := fixture()
			f(&r)
			if _, err := Sync(r, config()); err == nil {
				t.Fatal("accepted unsafe parent")
			}
			b, _ := json.Marshal(r)
			w := httptest.NewRecorder()
			NewHandler(config()).ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/sync", strings.NewReader(string(b))))
			if w.Code != 422 || strings.Contains(w.Body.String(), "children") {
				t.Fatalf("unsafe failure response: %d %s", w.Code, w.Body.String())
			}
		})
	}
}
func TestParentNameProducesValidServiceName(t *testing.T) {
	for _, name := range []string{"1catalog", "123"} {
		r := fixture()
		r.Parent.Metadata.Name = name
		if _, err := Sync(r, config()); err == nil {
			t.Errorf("accepted parent %q whose Service cannot be created", name)
		}
	}
	r := fixture()
	r.Parent.Metadata.Name = "catalog-1"
	if _, err := Sync(r, config()); err != nil {
		t.Fatal(err)
	}
	if !localName("1tenant") || !secretName("1secret") {
		t.Fatal("namespace and Secret names may legitimately start with digits")
	}
}
func TestObservedHealthAndConfigRollout(t *testing.T) {
	req := fixture()
	desired, _ := Sync(req, config())
	dep := find(t, desired, "Deployment")
	dep["metadata"].(map[string]any)["generation"] = float64(5)
	dep["status"] = map[string]any{"observedGeneration": float64(5), "replicas": float64(1), "readyReplicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1)}
	req.Children = observedChildren(t, desired, dep)
	ready, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if condition(ready, "Ready").Status != "True" || ready.Status.InstalledVersion != "0.3.3" {
		t.Fatalf("observed healthy rollout rejected: %+v", ready.Status)
	}
	req.Parent.Metadata.Generation++
	req.Parent.Spec.Resources = Resources{Requests: map[string]string{"cpu": "300m"}}
	changed, _ := Sync(req, config())
	if condition(changed, "Ready").Status != "False" {
		t.Fatal("stale health accepted for changed config")
	}
	req = fixture()
	req.Children = observedChildren(t, desired, dep)
	dep["status"].(map[string]any)["readyReplicas"] = float64(0)
	failed, _ := Sync(req, config())
	if condition(failed, "Ready").Status != "False" {
		t.Fatal("outage reported ready")
	}
}
func TestSecretRotationRollsPods(t *testing.T) {
	req := fixture()
	old, _ := Sync(req, config())
	req.Related["Secret.v1"]["database"]["metadata"].(map[string]any)["resourceVersion"] = "2"
	rotated, _ := Sync(req, config())
	if reflect.DeepEqual(find(t, old, "Deployment"), find(t, rotated, "Deployment")) {
		t.Fatal("rotation does not roll deployment")
	}
}
func TestHTTPRejectsInvalidEnvelope(t *testing.T) {
	for _, body := range []string{`{`, `{} {}`, `{"parent":{"spec":{"version":"0.3.3","privileged":true}}}`} {
		w := httptest.NewRecorder()
		NewHandler(config()).ServeHTTP(w, httptest.NewRequest("POST", "/sync", strings.NewReader(body)))
		if w.Code < 400 {
			t.Fatalf("accepted malformed request %s", body)
		}
	}
	w := httptest.NewRecorder()
	NewHandler(config()).ServeHTTP(w, httptest.NewRequest("GET", "/sync", nil))
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}

func TestKubernetesQuantityNormalizationDoesNotBlockReadiness(t *testing.T) {
	req := fixture()
	req.Parent.Spec.Resources = Resources{Requests: map[string]string{"cpu": "0.5", "memory": "1024Mi"}, Limits: map[string]string{"cpu": "1", "memory": "1Gi"}}
	desired, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	dep := asMap(t, find(t, desired, "Deployment"))
	dep["metadata"].(map[string]any)["generation"] = float64(1)
	dep["status"] = map[string]any{"observedGeneration": float64(1), "replicas": float64(1), "readyReplicas": float64(1), "updatedReplicas": float64(1), "availableReplicas": float64(1)}
	containers := dep["spec"].(map[string]any)["template"].(map[string]any)["spec"].(map[string]any)["containers"].([]any)
	requests := containers[0].(map[string]any)["resources"].(map[string]any)["requests"].(map[string]any)
	requests["cpu"] = "500m"
	requests["memory"] = "1Gi"
	req.Children = observedChildren(t, desired, dep)
	got, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if condition(got, "Ready").Status != "True" {
		t.Fatalf("Kubernetes canonical quantities must not block readiness: %+v", got.Status)
	}
}

func observedChildren(t *testing.T, desired Response, deployment Object) map[string]map[string]Object {
	t.Helper()
	children := map[string]map[string]Object{}
	for _, child := range desired.Children {
		kind := child["kind"].(string)
		children[kind] = map[string]Object{"catalog": asMap(t, child)}
	}
	children["Deployment"]["catalog"] = deployment
	return children
}

func TestReadinessRequiresEveryCurrentChild(t *testing.T) {
	for _, kind := range []string{"Service", "ServiceAccount", "NetworkPolicy", "Deployment"} {
		for _, failure := range []string{"missing", "terminating", "drifted"} {
			t.Run(kind+"/"+failure, func(t *testing.T) {
				req := fixture()
				desired, _ := Sync(req, config())
				dep := asMap(t, find(t, desired, "Deployment"))
				dep["metadata"].(map[string]any)["generation"] = 1
				dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "availableReplicas": 1, "updatedReplicas": 1}
				req.Children = observedChildren(t, desired, dep)
				before, _ := Sync(req, config())
				if condition(before, "Ready").Status != "True" {
					t.Fatal("complete healthy fixture is not Ready")
				}
				child := req.Children[kind]["catalog"]
				switch failure {
				case "missing":
					delete(req.Children, kind)
				case "terminating":
					child["metadata"].(map[string]any)["deletionTimestamp"] = "2026-09-28T00:00:00Z"
				case "drifted":
					switch kind {
					case "Service":
						child["spec"].(map[string]any)["selector"] = Object{"wrong": "selector"}
					case "NetworkPolicy":
						child["spec"].(map[string]any)["ingress"] = []any{}
					case "ServiceAccount":
						child["automountServiceAccountToken"] = true
					case "Deployment":
						child["spec"].(map[string]any)["selector"] = Object{"matchLabels": Object{"wrong": "selector"}}
					}
				}
				got, err := Sync(req, config())
				if err != nil || len(got.Children) != 4 {
					t.Fatalf("repair must retain every desired child: %v %+v", err, got)
				}
				if condition(got, "Ready").Status != "False" {
					t.Fatalf("Ready ignores %s %s", failure, kind)
				}
			})
		}
	}
}

func TestURLQueryCannotOverrideValidatedDatabaseHost(t *testing.T) {
	for _, query := range []string{"sslmode=disable&host=other.tenant-b.svc.cluster.local", "sslmode=disable&port=443", "sslmode=disable&sslmode=verify-full", "sslmode=require&sslrootcert=/auth/password"} {
		req := fixture()
		req.Related["Secret.v1"]["database"]["data"].(map[string]any)["url"] = base64.StdEncoding.EncodeToString([]byte("postgres://u:p@postgres.tenant-a.svc.cluster.local:5432/catalog?" + query))
		got, err := Sync(req, config())
		if err != nil {
			t.Fatal(err)
		}
		if condition(got, "DependenciesReady").Status != "False" {
			t.Fatalf("accepted connection override: %s", query)
		}
		if len(got.Children) != 4 {
			t.Fatal("lost children on invalid Secret")
		}
	}
}

func TestTLSDependencyRequiresServerCertificatePurpose(t *testing.T) {
	for _, tc := range []struct {
		name      string
		usage     []x509.ExtKeyUsage
		unknown   []asn1.ObjectIdentifier
		valid     bool
		extension *pkix.Extension
	}{
		{"client-only", []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, nil, false, nil},
		{"unknown-only", nil, []asn1.ObjectIdentifier{{1, 2, 3, 4}}, false, nil},
		{"server", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, true, nil},
		{"any", []x509.ExtKeyUsage{x509.ExtKeyUsageAny}, nil, true, nil},
		{"unrestricted", nil, nil, true, nil},
		{"unknown-critical", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, false,
			&pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}},
		{"unknown-noncritical", []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, nil, true,
			&pkix.Extension{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Value: []byte{5, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixture()
			req.Parent.Spec.Exposure = &Exposure{Hostname: "catalog.example.test", TLSSecretName: "tls"}
			pub, priv, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"catalog.example.test"},
				NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
				KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: tc.usage, UnknownExtKeyUsage: tc.unknown}
			if tc.extension != nil {
				template.ExtraExtensions = []pkix.Extension{*tc.extension}
			}
			der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
			if err != nil {
				t.Fatal(err)
			}
			key, err := x509.MarshalPKCS8PrivateKey(priv)
			if err != nil {
				t.Fatal(err)
			}
			req.Related["Secret.v1"]["tls"] = Object{"apiVersion": "v1", "kind": "Secret",
				"metadata": Object{"name": "tls", "namespace": "tenant-a", "uid": "tls-uid", "resourceVersion": "1"},
				"data": Object{"tls.crt": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
					"tls.key": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key}))}}
			got, err := Sync(req, config())
			if err != nil || len(got.Children) != 4 {
				t.Fatalf("TLS dependency must retain complete desired children: %v", err)
			}
			wantStatus, wantReplicas := "False", float64(0)
			if tc.valid {
				wantStatus, wantReplicas = "True", 1
			}
			if condition(got, "DependenciesReady").Status != wantStatus || asMap(t, find(t, got, "Deployment")["spec"])["replicas"] != wantReplicas {
				t.Fatalf("incorrect TLS purpose validation: %+v", got.Status)
			}
		})
	}
}
