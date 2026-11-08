package operator

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"maps"
	"math/big"
	"path"
	"reflect"
	"strings"
	"testing"
	"time"
)

func rotationCertificate(t *testing.T, isCA bool) Object {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), DNSNames: []string{"catalog.example.test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IsCA: isCA, BasicConstraintsValid: true}
	if isCA {
		template.KeyUsage |= x509.KeyUsageCertSign
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, pub, priv)
	if err != nil {
		t.Fatal(err)
	}
	key, err := x509.MarshalPKCS8PrivateKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	return Object{
		"tls.crt": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})),
		"tls.key": base64.StdEncoding.EncodeToString(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})),
	}
}

func rotationFixture(t *testing.T) Request {
	t.Helper()
	req := fixture()
	req.Parent.Spec.Exposure = &Exposure{Hostname: "catalog.example.test", TLSSecretName: "tls"}
	req.Related["Secret.v1"]["tls"] = Object{"apiVersion": "v1", "kind": "Secret",
		"metadata": Object{"name": "tls", "namespace": "tenant-a", "uid": "tls-uid", "resourceVersion": "1"},
		"data":     rotationCertificate(t, false)}
	db := object(req.Related["Secret.v1"]["database"]["data"])
	db["url"] = base64.StdEncoding.EncodeToString([]byte("postgres://alice:never-return-this@postgres.tenant-a.svc.cluster.local:5432/catalog?sslmode=verify-full&sslrootcert=/database/ca.crt"))
	db["ca.crt"] = rotationCertificate(t, true)["tls.crt"]
	return req
}

func TestSharedSecretExposesOnlyEachContainersFiles(t *testing.T) {
	for _, tc := range []struct {
		name string
		ca   bool
	}{{"with database CA", true}, {"without database CA", false}} {
		t.Run(tc.name, func(t *testing.T) {
			req := rotationFixture(t)
			secrets := req.Related["Secret.v1"]
			shared := object(secrets["auth"]["data"])
			maps.Copy(shared, object(secrets["database"]["data"]))
			maps.Copy(shared, object(secrets["tls"]["data"]))
			shared["backup-token"] = base64.StdEncoding.EncodeToString([]byte("unused"))
			delete(secrets, "database")
			delete(secrets, "tls")
			req.Parent.Spec.Database.SecretName = "auth"
			req.Parent.Spec.Exposure.TLSSecretName = "auth"
			want := map[string]map[string]string{
				"registry": {"/database/url": "url"},
				"gateway":  {"/auth/username": "username", "/auth/password": "password", "/tls/tls.crt": "tls.crt", "/tls/tls.key": "tls.key"},
			}
			if tc.ca {
				want["registry"]["/database/ca.crt"] = "ca.crt"
			} else {
				shared["url"] = object(fixture().Related["Secret.v1"]["database"]["data"])["url"]
				delete(shared, "ca.crt")
			}
			result, err := Sync(req, config())
			if err != nil || condition(result, "DependenciesReady").Status != "True" {
				t.Fatalf("valid shared Secret must resolve: %v %+v", err, result.Status)
			}
			deployment := asMap(t, find(t, result, "Deployment")["spec"])
			pod := object(object(deployment["template"])["spec"])
			volumes := map[string]Object{}
			for _, raw := range pod["volumes"].([]any) {
				v := object(raw)
				volumes[v["name"].(string)] = object(v["secret"])
			}
			got := map[string]map[string]string{}
			for _, raw := range pod["containers"].([]any) {
				container := object(raw)
				files := map[string]string{}
				got[container["name"].(string)] = files
				for _, rawMount := range container["volumeMounts"].([]any) {
					mount := object(rawMount)
					source := volumes[mount["name"].(string)]
					if source == nil {
						continue
					}
					dir := mount["mountPath"].(string)
					if source["secretName"] != "auth" || mount["readOnly"] != true {
						t.Fatalf("%s must mount the referenced Secret read-only", dir)
					}
					items, _ := source["items"].([]any)
					if len(items) == 0 { // Kubernetes mounts every Secret key by default.
						for key := range shared {
							files[path.Join(dir, key)] = key
						}
					}
					for _, rawItem := range items {
						item := object(rawItem)
						files[path.Join(dir, item["path"].(string))] = item["key"].(string)
					}
				}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("unexpected Secret files by container: got %v, want %v", got, want)
			}
		})
	}
}

func TestTerminatingRelatedSecretStopsRegistry(t *testing.T) {
	for _, name := range []string{"auth", "database", "tls"} {
		t.Run(name, func(t *testing.T) {
			req := rotationFixture(t)
			before, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			dep := asMap(t, find(t, before, "Deployment"))
			object(dep["metadata"])["generation"] = 1
			dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
			req.Children = observedChildren(t, before, dep)
			ready, err := Sync(req, config())
			if err != nil || condition(ready, "Ready").Status != "True" {
				t.Fatal("expected a healthy registry before deletion")
			}

			secrets := req.Related["Secret.v1"]
			replacement := asMap(t, secrets[name])
			object(secrets[name]["metadata"])["deletionTimestamp"] = "2026-11-04T12:00:00Z"
			blocked, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			deps := condition(blocked, "DependenciesReady")
			if deps.Status != "False" || deps.Reason != "InvalidOrMissingSecret" || !strings.Contains(deps.Message, name) ||
				!strings.Contains(deps.Message, "terminating") || condition(blocked, "Ready").Reason != "DependencyNotReady" ||
				number(object(find(t, blocked, "Deployment")["spec"])["replicas"]) != 0 {
				t.Fatalf("deleting %s should stop the registry and identify the unavailable Secret: %+v", name, blocked.Status)
			}

			object(replacement["metadata"])["uid"] = "replacement-" + name
			secrets[name] = replacement
			resumed, err := Sync(req, config())
			if err != nil || condition(resumed, "DependenciesReady").Status != "True" || !reflect.DeepEqual(resumed.Children, before.Children) {
				t.Fatal("observing a valid replacement did not restore the desired registry")
			}
		})
	}
}

func TestSecretMetadataUpdatesDoNotRollPods(t *testing.T) {
	for _, name := range []string{"auth", "database", "tls"} {
		t.Run(name, func(t *testing.T) {
			req := rotationFixture(t)
			before, err := Sync(req, config())
			if err != nil || condition(before, "DependenciesReady").Status != "True" {
				t.Fatalf("invalid fixture: %v", err)
			}
			dep := asMap(t, find(t, before, "Deployment"))
			object(dep["metadata"])["generation"] = 1
			dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
			req.Children = observedChildren(t, before, dep)
			metadata := object(req.Related["Secret.v1"][name]["metadata"])
			metadata["labels"] = Object{"owner": "platform"}
			metadata["annotations"] = Object{"last-checked": "updated"}
			metadata["resourceVersion"] = "2"
			after, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(find(t, before, "Deployment"), find(t, after, "Deployment")) {
				t.Fatal("metadata-only update changed the Deployment")
			}
			if condition(after, "Ready").Status != "True" {
				t.Fatal("metadata-only update interrupted readiness")
			}
		})
	}
}

func TestSecretRotationRollsPods(t *testing.T) {
	for _, tc := range []struct {
		name, secret, key, value string
	}{
		{"username", "auth", "username", "bob"},
		{"password", "auth", "password", "rotated-password-long-enough"},
		{"JWT key", "auth", "jwt-key", strings.Repeat("b", 64)},
		{"database URL", "database", "url", "postgres://alice:rotated-database-password@postgres.tenant-a.svc.cluster.local:5432/catalog?sslmode=verify-full&sslrootcert=/database/ca.crt"},
		{"database CA", "database", "ca.crt", ""},
		{"TLS pair", "tls", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := rotationFixture(t)
			before, err := Sync(req, config())
			if err != nil || condition(before, "DependenciesReady").Status != "True" {
				t.Fatalf("invalid fixture: %v", err)
			}
			secret := req.Related["Secret.v1"][tc.secret]
			switch tc.key {
			case "":
				secret["data"] = rotationCertificate(t, false)
			case "ca.crt":
				object(secret["data"])[tc.key] = rotationCertificate(t, true)["tls.crt"]
			default:
				object(secret["data"])[tc.key] = base64.StdEncoding.EncodeToString([]byte(tc.value))
			}
			object(secret["metadata"])["resourceVersion"] = "2"
			after, err := Sync(req, config())
			if err != nil || condition(after, "DependenciesReady").Status != "True" {
				t.Fatalf("valid rotation rejected: %v", err)
			}
			beforeTemplate := object(find(t, before, "Deployment")["spec"])["template"]
			afterTemplate := object(find(t, after, "Deployment")["spec"])["template"]
			if reflect.DeepEqual(beforeTemplate, afterTemplate) {
				t.Fatal("Secret data change did not roll the pod template")
			}
		})
	}
}
