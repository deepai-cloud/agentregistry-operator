package operator

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// This is the PostgreSQL/pgvector image used by the local acceptance installation.
const postgresImage = "pgvector/pgvector:0.8.2-pg17@sha256:feb68f4f15446397d8cac7f4fe48fe4586de83160d1fc48b46283312d1a33966"
const databaseLabel = "registry.deepai.cloud/database"

func managedDatabase(d Database) bool { return d.Host == "" && d.SecretName == "" }

func databaseStorage(storage string) (string, error) {
	if storage == "" {
		storage = "1Gi"
	}
	if len(storage) <= 16 && memoryQuantity.MatchString(storage) {
		bytes, err := resourceUnits("memory", storage)
		if err == nil {
			// Match Kubernetes quantity canonicalization in volumeClaimTemplates.
			return canonicalMemory(bytes), nil
		}
	}
	return "", errors.New("database storage must be a positive quantity in Mi or Gi, at most 1Pi")
}

func databaseName(p Parent) string {
	// The StatefulSet appends -0 and uses the result as a pod hostname, which
	// must still fit a 63-character DNS label. Reserve both suffixes here.
	return databaseResourceName(p.Metadata.Name, 52)
}

// Keep the previous identity available for discovering retained credentials and
// storage. Renaming a StatefulSet also changes its generated PVC names.
func legacyDatabaseName(p Parent) string {
	return databaseResourceName(p.Metadata.Name, 54)
}

func databaseResourceName(name string, maxPrefix int) string {
	if len(name) > maxPrefix {
		name = name[:maxPrefix-11] + "-" + hash(name)[:10]
	}
	return name + "-postgres"
}

func databaseHost(p Parent) string {
	return databaseName(p) + "." + p.Metadata.Namespace + ".svc.cluster.local"
}

func databaseSecretURL(secret Object) *url.URL {
	raw := string(secretData(secret, "url"))
	// The pinned pgx driver recognizes only these exact URL prefixes. net/url
	// lowercases schemes, but pgx treats other casing as keyword/value settings.
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.Fragment != "" {
		return nil
	}
	return u
}

func validDatabaseSecret(secret Object, p Parent) bool {
	password := string(secretData(secret, "password"))
	adminPassword := string(secretData(secret, "postgres-password"))
	// Both passwords are passed to PostgreSQL through the pod environment.
	if strings.ContainsRune(password, '\x00') || strings.ContainsRune(adminPassword, '\x00') {
		return false
	}
	u := databaseSecretURL(secret)
	if u == nil {
		return false
	}
	urlPassword, _ := u.User.Password()
	return len(password) >= 32 && len(adminPassword) >= 32 && password != adminPassword &&
		string(secretData(secret, "username")) == "registry" && string(secretData(secret, "database")) == "registry" &&
		u.User.Username() == "registry" && urlPassword == password && u.Path == "/registry" &&
		u.Host == databaseHost(p)+":5432" && u.RawQuery == "sslmode=disable"
}

func observedObject(groups map[string]map[string]Object, kind, name, namespace string) Object {
	for _, group := range groups {
		for _, o := range group {
			m := object(o["metadata"])
			if o["kind"] == kind && m["name"] == name && m["namespace"] == namespace {
				return o
			}
		}
	}
	return nil
}

// Credentials are created only for a new installation and reused verbatim thereafter.
// Losing a Secret must never silently replace a password stored on an existing disk.
// pending is true while a fresh installation must defer StatefulSet creation.
func databaseSecret(r Request) (secret Object, pending bool, err error) {
	p := r.Parent
	name := databaseName(p)
	if legacy := legacyDatabaseName(p); legacy != name {
		for _, kind := range []string{"Secret", "StatefulSet", "PersistentVolumeClaim"} {
			oldName := legacy
			if kind == "PersistentVolumeClaim" {
				oldName = "data-" + legacy + "-0"
			}
			if observedObject(r.Children, kind, oldName, p.Metadata.Namespace) != nil ||
				observedObject(r.Related, kind, oldName, p.Metadata.Namespace) != nil {
				return nil, false, fmt.Errorf("legacy managed database %s %s requires manual recovery before using the corrected name %s; preserve its credentials and storage and follow the long-name recovery guide", kind, oldName, name)
			}
		}
	}
	previous := observedObject(r.Children, "Secret", name, p.Metadata.Namespace)
	related := relatedSecret(r, name)
	relatedMeta := object(related["metadata"])
	childUID, _ := object(previous["metadata"])["uid"].(string)
	// The related watch may see deletion before the child watch. A deletion
	// timestamp is irreversible for that UID, but does not apply to a replacement.
	if previous == nil || (childUID != "" && relatedMeta["uid"] == childUID && relatedMeta["deletionTimestamp"] != nil) {
		previous = related
	}
	if previous != nil && object(previous["metadata"])["deletionTimestamp"] == nil {
		return Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": name, "namespace": p.Metadata.Namespace}, "type": "Opaque", "immutable": true, "data": previous["data"]}, false, nil
	}
	if observedObject(r.Children, "StatefulSet", name, p.Metadata.Namespace) != nil ||
		observedObject(r.Related, "PersistentVolumeClaim", "data-"+name+"-0", p.Metadata.Namespace) != nil {
		return nil, false, nil
	}
	if previous != nil {
		// Wait for the terminating Secret to disappear before generating a new one.
		return nil, true, nil
	}
	random := make([]byte, 64)
	if _, err := rand.Read(random); err != nil {
		return nil, false, fmt.Errorf("generate database credentials: %w", err)
	}
	password, adminPassword := hex.EncodeToString(random[:32]), hex.EncodeToString(random[32:])
	u := url.URL{Scheme: "postgres", User: url.UserPassword("registry", password), Host: databaseHost(p) + ":5432", Path: "/registry", RawQuery: "sslmode=disable"}
	data := Object{}
	for key, value := range map[string]string{"username": "registry", "password": password, "postgres-password": adminPassword, "database": "registry", "url": u.String()} {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": name, "namespace": p.Metadata.Namespace}, "type": "Opaque", "immutable": true, "data": data}, true, nil
}

// psql quotes the variable as an SQL literal. The app owns only its database and
// never receives the bootstrap superuser credential. Each committed step is safe
// to repeat if startup is interrupted, without resetting existing credentials.
const databaseInit = `#!/bin/bash
set -euo pipefail
psql --username postgres --dbname postgres --no-password --no-psqlrc --set ON_ERROR_STOP=1 <<'SQL'
\getenv registry_password REGISTRY_PASSWORD
SELECT format('CREATE ROLE registry LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD %L', :'registry_password')
WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'registry') \gexec
SELECT 'CREATE DATABASE registry OWNER registry'
WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname = 'registry') \gexec
REVOKE ALL ON DATABASE registry FROM PUBLIC;
\connect registry
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS vector;
SQL
touch "$PGDATA/.registry-initialized"
`

// The pinned image only runs init hooks on an empty PGDATA. Resume incomplete
// registry setup on a socket-only temporary server before opening the TCP port.
// Completed volumes keep the ordinary image startup path. The ConfigMap key has
// no .sh suffix so the image does not source this wrapper as another init hook.
const databaseStart = `#!/bin/bash
set -euo pipefail
source /usr/local/bin/docker-entrypoint.sh
docker_setup_env
if [ -n "$DATABASE_ALREADY_EXISTS" ] && [ ! -e "$PGDATA/.registry-initialized" ]; then
    docker_create_db_directories
    docker_temp_server_start "$@"
    trap docker_temp_server_stop EXIT
    bash /docker-entrypoint-initdb.d/01-registry.sh
    docker_temp_server_stop
    trap - EXIT
fi
exec /usr/local/bin/docker-entrypoint.sh "$@"
`

func databaseChildren(p Parent, secret Object, pending bool) []Object {
	name := databaseName(p)
	labels := Object{databaseLabel: p.Metadata.Name}
	selector := Object{"matchLabels": labels}
	makeObject := func(api, kind string, spec Object) Object {
		o := Object{"apiVersion": api, "kind": kind, "metadata": Object{"name": name, "namespace": p.Metadata.Namespace, "labels": labels}}
		if spec != nil {
			o["spec"] = spec
		}
		return o
	}
	init := makeObject("v1", "ConfigMap", nil)
	init["data"] = Object{"01-registry.sh": databaseInit, "start-postgres": databaseStart}
	service := makeObject("v1", "Service", Object{"clusterIP": "None", "selector": labels, "ports": []Object{{"name": "postgres", "port": 5432, "targetPort": 5432, "protocol": "TCP"}}})
	// An Egress policy with no rules denies all egress. The API omits empty lists,
	// so leave the field absent to keep desired-state readiness comparisons stable.
	network := makeObject("networking.k8s.io/v1", "NetworkPolicy", Object{"podSelector": selector, "policyTypes": []string{"Ingress", "Egress"}, "ingress": []Object{{"from": []Object{{"podSelector": Object{"matchLabels": Object{instanceLabel: p.Metadata.UID}}}}, "ports": []Object{{"protocol": "TCP", "port": 5432}}}}})
	children := []Object{init, network, service}
	if secret != nil {
		children = append([]Object{secret}, children...)
	}
	// Child creation is independent: a failed Secret create must not leave a
	// StatefulSet/PVC that would then prevent safe credential generation on retry.
	if pending {
		return children
	}
	storage, _ := databaseStorage(p.Spec.Database.Storage) // Checked before rendering children.
	claim := Object{"accessModes": []string{"ReadWriteOnce"}, "resources": Object{"requests": Object{"storage": storage}}}
	if p.Spec.Database.StorageClassName != nil {
		claim["storageClassName"] = *p.Spec.Database.StorageClassName
	}
	secretEnv := func(env, key string) Object {
		return Object{"name": env, "valueFrom": Object{"secretKeyRef": Object{"name": name, "key": key}}}
	}
	probe := Object{"exec": Object{"command": []string{"pg_isready", "-h", "127.0.0.1", "-U", "postgres", "-d", "postgres"}}, "periodSeconds": 5, "timeoutSeconds": 3}
	container := Object{
		"name": "postgres", "image": postgresImage, "imagePullPolicy": "IfNotPresent",
		"command": []string{"/bin/bash", "/docker-entrypoint-initdb.d/start-postgres"}, "args": []string{"postgres"},
		"securityContext": Object{"runAsNonRoot": true, "runAsUser": 999, "runAsGroup": 999, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": Object{"drop": []string{"ALL"}}},
		"resources":       Resources{Requests: map[string]string{"cpu": "100m", "memory": "128Mi"}, Limits: map[string]string{"cpu": "1", "memory": "512Mi"}},
		"env":             []Object{{"name": "PGDATA", "value": "/var/lib/postgresql/data/pgdata"}, {"name": "POSTGRES_USER", "value": "postgres"}, {"name": "POSTGRES_DB", "value": "postgres"}, {"name": "POSTGRES_INITDB_ARGS", "value": "--auth-host=scram-sha-256"}, secretEnv("POSTGRES_PASSWORD", "postgres-password"), secretEnv("REGISTRY_PASSWORD", "password")},
		"ports":           []Object{{"name": "postgres", "containerPort": 5432}},
		"volumeMounts":    []Object{{"name": "data", "mountPath": "/var/lib/postgresql/data"}, {"name": "run", "mountPath": "/var/run/postgresql"}, {"name": "tmp", "mountPath": "/tmp"}, {"name": "init", "mountPath": "/docker-entrypoint-initdb.d", "readOnly": true}},
		"readinessProbe":  probe, "livenessProbe": probe,
		"startupProbe": Object{"exec": probe["exec"], "periodSeconds": 5, "timeoutSeconds": 3, "failureThreshold": 60},
	}
	replicas := 1
	if !validDatabaseSecret(secret, p) {
		replicas = 0
	}
	// The image processes every init hook on a fresh volume, including
	// non-executable shell and SQL files. Mount only the two scripts we own.
	initVolume := Object{"name": "init", "configMap": Object{
		"name": name, "defaultMode": 0444,
		"items": []Object{
			{"key": "start-postgres", "path": "start-postgres"},
			{"key": "01-registry.sh", "path": "01-registry.sh"},
		},
	}}
	sts := makeObject("apps/v1", "StatefulSet", Object{
		"serviceName": name, "replicas": replicas, "selector": selector,
		"persistentVolumeClaimRetentionPolicy": Object{"whenDeleted": "Retain", "whenScaled": "Retain"},
		"volumeClaimTemplates":                 []Object{{"metadata": Object{"name": "data"}, "spec": claim}},
		"template": Object{"metadata": Object{"labels": labels, "annotations": Object{fingerprintKey: hash(init["data"])}}, "spec": Object{
			"serviceAccountName": p.Metadata.Name, "automountServiceAccountToken": false,
			"securityContext": Object{"fsGroup": 999, "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": Object{"type": "RuntimeDefault"}},
			"containers":      []Object{container},
			"volumes":         []Object{initVolume, {"name": "run", "emptyDir": Object{}}, {"name": "tmp", "emptyDir": Object{"sizeLimit": "64Mi"}}},
		}},
	})
	return append(children, sts)
}

func databaseHealthy(r Request) bool {
	o := observedObject(r.Children, "StatefulSet", databaseName(r.Parent), r.Parent.Metadata.Namespace)
	st, metadata := object(o["status"]), object(o["metadata"])
	return number(metadata["generation"]) >= 1 && number(st["observedGeneration"]) >= number(metadata["generation"]) &&
		number(st["replicas"]) == 1 && number(st["readyReplicas"]) == 1 && number(st["updatedReplicas"]) == 1 &&
		st["currentRevision"] != nil && st["currentRevision"] == st["updateRevision"]
}
