package operator

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
)

// This is the PostgreSQL/pgvector image used by the local acceptance installation.
const postgresImage = "pgvector/pgvector:0.8.2-pg17@sha256:feb68f4f15446397d8cac7f4fe48fe4586de83160d1fc48b46283312d1a33966"
const databaseLabel = "registry.deepai.cloud/database"

func managedDatabase(d Database) bool { return d.Host == "" && d.SecretName == "" }

func databaseName(p Parent) string {
	name := p.Metadata.Name
	if len(name) > 54 {
		name = name[:43] + "-" + hash(name)[:10]
	}
	return name + "-postgres"
}

func databaseHost(p Parent) string {
	return databaseName(p) + "." + p.Metadata.Namespace + ".svc.cluster.local"
}

func validDatabaseSecret(secret Object, p Parent) bool {
	password := string(secretData(secret, "password"))
	u, err := url.Parse(string(secretData(secret, "url")))
	if err != nil || u == nil || u.User == nil {
		return false
	}
	urlPassword, _ := u.User.Password()
	return len(password) >= 32 && len(secretData(secret, "postgres-password")) >= 32 &&
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
func databaseSecret(r Request) (Object, error) {
	p := r.Parent
	name := databaseName(p)
	previous := observedObject(r.Children, "Secret", name, p.Metadata.Namespace)
	if previous == nil {
		previous = relatedSecret(r, name)
	}
	if previous != nil {
		return Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": name, "namespace": p.Metadata.Namespace}, "type": "Opaque", "immutable": true, "data": previous["data"]}, nil
	}
	if observedObject(r.Children, "StatefulSet", name, p.Metadata.Namespace) != nil ||
		observedObject(r.Related, "PersistentVolumeClaim", "data-"+name+"-0", p.Metadata.Namespace) != nil {
		return nil, nil
	}
	random := make([]byte, 64)
	if _, err := rand.Read(random); err != nil {
		return nil, fmt.Errorf("generate database credentials: %w", err)
	}
	password, adminPassword := hex.EncodeToString(random[:32]), hex.EncodeToString(random[32:])
	u := url.URL{Scheme: "postgres", User: url.UserPassword("registry", password), Host: databaseHost(p) + ":5432", Path: "/registry", RawQuery: "sslmode=disable"}
	data := Object{}
	for key, value := range map[string]string{"username": "registry", "password": password, "postgres-password": adminPassword, "database": "registry", "url": u.String()} {
		data[key] = base64.StdEncoding.EncodeToString([]byte(value))
	}
	return Object{"apiVersion": "v1", "kind": "Secret", "metadata": Object{"name": name, "namespace": p.Metadata.Namespace}, "type": "Opaque", "immutable": true, "data": data}, nil
}

// psql quotes the variable as an SQL literal. The app owns only its database and
// never receives the bootstrap superuser credential. Extensions are installed first.
const databaseInit = `#!/bin/bash
set -euo pipefail
psql --username postgres --dbname postgres --no-password --set ON_ERROR_STOP=1 <<'SQL'
\getenv registry_password REGISTRY_PASSWORD
CREATE ROLE registry LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD :'registry_password';
CREATE DATABASE registry OWNER registry;
REVOKE ALL ON DATABASE registry FROM PUBLIC;
\connect registry
CREATE EXTENSION IF NOT EXISTS pg_trgm;
CREATE EXTENSION IF NOT EXISTS vector;
SQL
`

func databaseChildren(p Parent, secret Object) []Object {
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
	init["data"] = Object{"01-registry.sh": databaseInit}
	service := makeObject("v1", "Service", Object{"clusterIP": "None", "selector": labels, "ports": []Object{{"name": "postgres", "port": 5432, "targetPort": 5432, "protocol": "TCP"}}})
	// An Egress policy with no rules denies all egress. The API omits empty lists,
	// so leave the field absent to keep desired-state readiness comparisons stable.
	network := makeObject("networking.k8s.io/v1", "NetworkPolicy", Object{"podSelector": selector, "policyTypes": []string{"Ingress", "Egress"}, "ingress": []Object{{"from": []Object{{"podSelector": Object{"matchLabels": Object{instanceLabel: p.Metadata.UID}}}}, "ports": []Object{{"protocol": "TCP", "port": 5432}}}}})
	storage := p.Spec.Database.Storage
	if storage == "" {
		storage = "1Gi"
	}
	// Kubernetes canonicalizes quantities in volumeClaimTemplates on admission.
	bytes := int64(quantity(storage))
	if bytes%(1<<30) == 0 {
		storage = strconv.FormatInt(bytes/(1<<30), 10) + "Gi"
	} else {
		storage = strconv.FormatInt(bytes/(1<<20), 10) + "Mi"
	}
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
	sts := makeObject("apps/v1", "StatefulSet", Object{
		"serviceName": name, "replicas": replicas, "selector": selector,
		"persistentVolumeClaimRetentionPolicy": Object{"whenDeleted": "Retain", "whenScaled": "Retain"},
		"volumeClaimTemplates":                 []Object{{"metadata": Object{"name": "data"}, "spec": claim}},
		"template": Object{"metadata": Object{"labels": labels}, "spec": Object{
			"serviceAccountName": p.Metadata.Name, "automountServiceAccountToken": false,
			"securityContext": Object{"fsGroup": 999, "fsGroupChangePolicy": "OnRootMismatch", "seccompProfile": Object{"type": "RuntimeDefault"}},
			"containers":      []Object{container},
			"volumes":         []Object{{"name": "init", "configMap": Object{"name": name, "defaultMode": 0444}}, {"name": "run", "emptyDir": Object{}}, {"name": "tmp", "emptyDir": Object{"sizeLimit": "64Mi"}}},
		}},
	})
	children := []Object{init, network, service, sts}
	if secret != nil {
		children = append([]Object{secret}, children...)
	}
	return children
}

func databaseHealthy(r Request) bool {
	o := observedObject(r.Children, "StatefulSet", databaseName(r.Parent), r.Parent.Metadata.Namespace)
	st, metadata := object(o["status"]), object(o["metadata"])
	return number(metadata["generation"]) >= 1 && number(st["observedGeneration"]) >= number(metadata["generation"]) &&
		number(st["replicas"]) == 1 && number(st["readyReplicas"]) == 1 && number(st["updatedReplicas"]) == 1 &&
		st["currentRevision"] != nil && st["currentRevision"] == st["updateRevision"]
}
