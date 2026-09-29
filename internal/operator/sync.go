package operator

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"reflect"
)

const instanceLabel = "registry.deepai.cloud/instance"
const fingerprintKey = "registry.deepai.cloud/config-hash"

var images = map[string]string{
	"0.3.2": "ghcr.io/agentregistry-dev/agentregistry/server:v0.3.2@sha256:5e5ef547d92f5c67f39989a3b904ed0bf7dbdf83bff1b31f2f3f73c15889d660",
	"0.3.3": "ghcr.io/agentregistry-dev/agentregistry/server:v0.3.3@sha256:2ec728d9f44a7b13f777219ce9404cd2250b3217098bb32192e6d8d52de8ee27",
}

func object(v any) Object { m, _ := v.(map[string]any); return m }
func hash(v any) string {
	b, _ := json.Marshal(v)
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}
func Sync(r Request, c Config) (Response, error) {
	if err := validate(r.Parent, c); err != nil {
		return Response{}, err
	}
	p := r.Parent
	managed := managedDatabase(p.Spec.Database)
	var dbSecret Object
	if managed {
		var err error
		dbSecret, err = databaseSecret(r)
		if err != nil {
			return Response{}, err
		}
		p.Spec.Database.Host = databaseHost(p)
		p.Spec.Database.SecretName = databaseName(p)
	}
	// Validate the generated connection the same way as an external connection,
	// without mutating the webhook's observed objects.
	dependencyRequest := r
	dependencyRequest.Parent = p
	dependencyRequest.Related = make(map[string]map[string]Object, len(r.Related)+1)
	for key, group := range r.Related {
		copyGroup := make(map[string]Object, len(group))
		for name, value := range group {
			if managed && value["kind"] == "Secret" && object(value["metadata"])["name"] == databaseName(p) {
				continue
			}
			copyGroup[name] = value
		}
		dependencyRequest.Related[key] = copyGroup
	}
	if dbSecret != nil {
		dependencySecret := Object{}
		for key, value := range dbSecret {
			dependencySecret[key] = value
		}
		// A content fingerprint is stable across observation through both caches.
		dependencySecret["metadata"] = Object{"name": databaseName(p), "namespace": p.Metadata.Namespace, "uid": hash(dbSecret["data"]), "resourceVersion": "managed"}
		dependencyRequest.Related["generated-database"] = map[string]Object{databaseName(p): dependencySecret}
	}
	depsOK, message, tokens := dependencies(dependencyRequest)
	if managed {
		if dbSecret == nil {
			depsOK, message = false, "Managed database credentials are missing for existing storage; restore the database Secret from backup"
		} else if !validDatabaseSecret(dbSecret, p) {
			depsOK, message = false, "Managed database Secret is invalid; restore its original credentials from backup"
		}
	}
	replicas := 1
	if !depsOK {
		replicas = 0
	}
	labels := Object{instanceLabel: p.Metadata.UID}
	selector := Object{"matchLabels": labels}
	makeObject := func(api, kind string, spec Object) Object {
		o := Object{"apiVersion": api, "kind": kind, "metadata": Object{"name": p.Metadata.Name, "namespace": p.Metadata.Namespace, "labels": labels}}
		if spec != nil {
			o["spec"] = spec
		}
		return o
	}
	sa := makeObject("v1", "ServiceAccount", nil)
	sa["automountServiceAccountToken"] = false
	security := Object{"runAsNonRoot": true, "runAsUser": 1001, "runAsGroup": 1001, "readOnlyRootFilesystem": true, "allowPrivilegeEscalation": false, "capabilities": Object{"drop": []string{"ALL"}}, "seccompProfile": Object{"type": "RuntimeDefault"}}
	env := func(name, value string) Object { return Object{"name": name, "value": value} }
	secretEnv := func(name, secret, key string) Object {
		return Object{"name": name, "valueFrom": Object{"secretKeyRef": Object{"name": secret, "key": key}}}
	}
	res, _ := resources(p.Spec.Resources)
	registry := Object{"name": "registry", "image": images[p.Spec.Version], "imagePullPolicy": "IfNotPresent", "securityContext": security, "resources": res, "env": []Object{
		env("AGENT_REGISTRY_SERVER_ADDRESS", "127.0.0.1:8080"), env("AGENT_REGISTRY_MCP_PORT", "31313"), env("AGENT_REGISTRY_ENABLE_ANONYMOUS_AUTH", "true"), env("AGENT_REGISTRY_ENABLE_REGISTRY_VALIDATION", "false"), env("AGENT_REGISTRY_DISABLE_BUILTIN_SEED", "true"), env("AGENT_REGISTRY_DATABASE_VECTOR_ENABLED", "false"), env("AGENT_REGISTRY_EMBEDDINGS_ENABLED", "false"), secretEnv("AGENT_REGISTRY_DATABASE_URL", p.Spec.Database.SecretName, "url"), secretEnv("AGENT_REGISTRY_JWT_PRIVATE_KEY", p.Spec.AuthSecretName, "jwt-key")}, "volumeMounts": []Object{{"name": "tmp", "mountPath": "/tmp"}, {"name": "database", "mountPath": "/database", "readOnly": true}}}
	gatewayEnv := []Object{env("AUTH_DIR", "/auth"), env("LISTEN_ADDRESS", ":8443"), env("HEALTH_ADDRESS", ":9090")}
	mounts := []Object{{"name": "auth", "mountPath": "/auth", "readOnly": true}}
	volumes := []Object{{"name": "tmp", "emptyDir": Object{"sizeLimit": "64Mi"}}, {"name": "database", "secret": Object{"secretName": p.Spec.Database.SecretName}}, {"name": "auth", "secret": Object{"secretName": p.Spec.AuthSecretName}}}
	if managed {
		// The bootstrap superuser password must never be mounted into the app.
		volumes[1]["secret"] = Object{"secretName": p.Spec.Database.SecretName, "items": []Object{{"key": "url", "path": "url"}}}
	}
	serviceType, port, scheme, host := "ClusterIP", 8080, "http", p.Metadata.Name+"."+p.Metadata.Namespace+".svc.cluster.local:8080"
	if p.Spec.Exposure != nil {
		serviceType, port, scheme, host = "LoadBalancer", 443, "https", p.Spec.Exposure.Hostname
		gatewayEnv = append(gatewayEnv, env("TLS_CERT_FILE", "/tls/tls.crt"), env("TLS_KEY_FILE", "/tls/tls.key"))
		mounts = append(mounts, Object{"name": "tls", "mountPath": "/tls", "readOnly": true})
		volumes = append(volumes, Object{"name": "tls", "secret": Object{"secretName": p.Spec.Exposure.TLSSecretName}})
	}
	probe := func(path string) Object {
		return Object{"httpGet": Object{"path": path, "port": 9090}, "periodSeconds": 5, "timeoutSeconds": 4, "failureThreshold": 2}
	}
	gateway := Object{"name": "gateway", "image": c.GatewayImage, "imagePullPolicy": "IfNotPresent", "command": []string{"/gateway"}, "securityContext": security, "resources": Resources{Requests: map[string]string{"cpu": "50m", "memory": "32Mi"}, Limits: map[string]string{"cpu": "250m", "memory": "128Mi"}}, "env": gatewayEnv, "ports": []Object{{"name": "gateway", "containerPort": 8443}}, "volumeMounts": mounts, "readinessProbe": probe("/readyz"), "livenessProbe": probe("/healthz")}
	template := Object{"metadata": Object{"labels": labels, "annotations": Object{fingerprintKey: hash([]any{p.Spec, tokens, c.GatewayImage})}}, "spec": Object{"serviceAccountName": p.Metadata.Name, "automountServiceAccountToken": false, "securityContext": Object{"fsGroup": 1001, "seccompProfile": Object{"type": "RuntimeDefault"}}, "containers": []Object{registry, gateway}, "volumes": volumes}}
	deployment := makeObject("apps/v1", "Deployment", Object{"replicas": replicas, "revisionHistoryLimit": 2, "strategy": Object{"type": "Recreate"}, "selector": selector, "template": template})
	service := makeObject("v1", "Service", Object{"type": serviceType, "selector": labels, "ports": []Object{{"name": "gateway", "port": port, "targetPort": 8443, "protocol": "TCP"}}})
	network := makeObject("networking.k8s.io/v1", "NetworkPolicy", networkPolicy(p, selector))
	children := []Object{sa, network, service, deployment}
	if managed {
		children = append(children, databaseChildren(r.Parent, dbSecret)...)
	}
	ready := depsOK && observedCurrent(r, children) && observedHealthy(r, deployment) && (!managed || databaseHealthy(r))
	readyCondition := Condition{Type: "Ready", Status: "False", Reason: "Progressing", Message: "Waiting for current managed resources and registry rollout with database/MCP readiness; inspect children and pod events", ObservedGeneration: p.Metadata.Generation}
	if !depsOK {
		readyCondition.Reason = "DependencyNotReady"
		readyCondition.Message = message
	}
	installed := p.Status.InstalledVersion
	if ready {
		readyCondition.Status = "True"
		readyCondition.Reason = "Available"
		readyCondition.Message = "Current application rollout passed database and MCP readiness"
		installed = p.Spec.Version
	}
	dc := Condition{Type: "DependenciesReady", Status: "False", Reason: "InvalidOrMissingSecret", Message: message, ObservedGeneration: p.Metadata.Generation}
	if depsOK {
		dc.Status = "True"
		dc.Reason = "Resolved"
	}
	base := scheme + "://" + host
	return Response{Children: children, Status: Status{ObservedGeneration: p.Metadata.Generation, InstalledVersion: installed, Endpoints: map[string]string{"ui": base + "/", "api": base + "/v0", "mcp": base + "/mcp"}, Conditions: []Condition{dc, readyCondition}}, ResyncAfterSeconds: 10}, nil
}
func networkPolicy(p Parent, selector Object) Object {
	// A podSelector without namespaceSelector is scoped to this policy's namespace.
	ingress := Object{"ports": []Object{{"protocol": "TCP", "port": 8443}}, "from": []Object{{"podSelector": Object{}}}}
	if p.Spec.Exposure != nil {
		delete(ingress, "from")
	}
	peer := Object{"podSelector": Object{"matchLabels": Object{"registry.deepai.cloud/database": p.Metadata.Name}}}
	if ip := net.ParseIP(p.Spec.Database.Host); ip != nil {
		bits := 128
		if ip.To4() != nil {
			bits = 32
		}
		peer = Object{"ipBlock": Object{"cidr": fmt.Sprintf("%s/%d", ip.String(), bits)}}
	}
	return Object{"podSelector": selector, "policyTypes": []string{"Ingress", "Egress"}, "ingress": []Object{ingress}, "egress": []Object{
		{"to": []Object{peer}, "ports": []Object{{"protocol": "TCP", "port": 5432}}},
		{"to": []Object{{"namespaceSelector": Object{"matchLabels": Object{"kubernetes.io/metadata.name": "kube-system"}}, "podSelector": Object{"matchLabels": Object{"k8s-app": "kube-dns"}}}}, "ports": []Object{{"protocol": "UDP", "port": 53}, {"protocol": "TCP", "port": 53}}},
	}}
}

func observedCurrent(r Request, desired []Object) bool {
	for _, child := range desired {
		found := false
		for _, group := range r.Children {
			for _, observed := range group {
				if object(observed["metadata"])["deletionTimestamp"] == nil && contains(observed, child) {
					found = true
				}
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func observedHealthy(r Request, desired Object) bool {
	for _, group := range r.Children {
		for _, o := range group {
			m := object(o["metadata"])
			if o["kind"] != "Deployment" || m["name"] != r.Parent.Metadata.Name || m["namespace"] != r.Parent.Metadata.Namespace {
				continue
			}
			st := object(o["status"])
			if number(st["observedGeneration"]) < number(m["generation"]) || number(m["generation"]) < 1 {
				continue
			}
			healthy := true
			for _, key := range []string{"replicas", "readyReplicas", "availableReplicas", "updatedReplicas"} {
				if number(st[key]) != 1 {
					healthy = false
				}
			}
			if number(object(o["spec"])["replicas"]) != 1 {
				healthy = false
			}
			if healthy && contains(object(o["spec"])["template"], object(desired["spec"])["template"]) {
				return true
			}
		}
	}
	return false
}
func number(v any) float64 {
	b, _ := json.Marshal(v)
	var n float64
	_ = json.Unmarshal(b, &n)
	return n
}

// Compare desired fields while allowing API-defaulted fields in the observed object.
func contains(observed, desired any) bool {
	a, _ := json.Marshal(observed)
	b, _ := json.Marshal(desired)
	var x, y any
	_ = json.Unmarshal(a, &x)
	_ = json.Unmarshal(b, &y)
	return subset(x, y)
}
func subset(observed, desired any) bool {
	switch d := desired.(type) {
	case map[string]any:
		o, ok := observed.(map[string]any)
		if !ok {
			return false
		}
		for k, v := range d {
			if !subset(o[k], v) {
				return false
			}
		}
		return true
	case []any:
		o, ok := observed.([]any)
		if !ok || len(o) != len(d) {
			return false
		}
		for i := range d {
			if !subset(o[i], d[i]) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(observed, desired)
	}
}
