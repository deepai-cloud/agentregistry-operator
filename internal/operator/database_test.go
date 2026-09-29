package operator

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
)

func managedFixture() Request {
	r := fixture()
	r.Parent.Spec.Database = Database{}
	delete(r.Related["Secret.v1"], "database")
	return r
}

func databaseChild(t *testing.T, r Response, kind, name string) Object {
	t.Helper()
	for _, child := range r.Children {
		if child["kind"] == kind && object(child["metadata"])["name"] == name {
			return asMap(t, child)
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return nil
}

func observeDatabaseChildren(t *testing.T, r Response) map[string]map[string]Object {
	t.Helper()
	observed := map[string]map[string]Object{}
	for _, child := range r.Children {
		key := child["kind"].(string) + "." + child["apiVersion"].(string)
		if observed[key] == nil {
			observed[key] = map[string]Object{}
		}
		m := object(child["metadata"])
		observed[key][m["namespace"].(string)+"/"+m["name"].(string)] = asMap(t, child)
	}
	return observed
}

func TestManagedDatabaseProvisionsCompleteInstallation(t *testing.T) {
	req := managedFixture()
	got, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Children) != 9 {
		t.Fatalf("want four application and five database children, got %d", len(got.Children))
	}
	name := databaseName(req.Parent)
	if name != "catalog-postgres" {
		t.Fatalf("unexpected database name %q", name)
	}
	secret := databaseChild(t, got, "Secret", name)
	if secret["immutable"] != true {
		t.Fatal("managed database credentials must be immutable")
	}
	for _, key := range []string{"url", "username", "password", "postgres-password", "database"} {
		if len(secretData(secret, key)) == 0 {
			t.Fatalf("managed database Secret has no %q", key)
		}
	}
	if len(secretData(secret, "password")) < 32 || len(secretData(secret, "postgres-password")) < 32 {
		t.Fatal("database passwords must be independently generated strong credentials")
	}
	if string(secretData(secret, "password")) == string(secretData(secret, "postgres-password")) {
		t.Fatal("application must not share the postgres administrator password")
	}
	u, err := url.Parse(string(secretData(secret, "url")))
	if err != nil || u.User == nil {
		t.Fatalf("invalid generated database URL: %v", err)
	}
	password, _ := u.User.Password()
	if u.Hostname() != name+".tenant-a.svc.cluster.local" || u.Port() != "5432" || u.User.Username() == "postgres" || u.User.Username() != string(secretData(secret, "username")) || password != string(secretData(secret, "password")) || strings.TrimPrefix(u.Path, "/") != string(secretData(secret, "database")) {
		t.Fatal("database URL must refer to the managed Service and dedicated application credentials")
	}
	service := databaseChild(t, got, "Service", name)
	if object(service["spec"])["clusterIP"] != "None" {
		t.Fatal("managed PostgreSQL requires a headless Service")
	}
	statefulSet := databaseChild(t, got, "StatefulSet", name)
	stsSpec := object(statefulSet["spec"])
	if stsSpec["serviceName"] != name || number(stsSpec["replicas"]) != 1 {
		t.Fatal("managed PostgreSQL must run one persistent instance behind its Service")
	}
	pod := object(object(stsSpec["template"])["spec"])
	containers := pod["containers"].([]any)
	if !strings.Contains(object(containers[0])["image"].(string), "pgvector") {
		t.Fatal("database image lacks required pgvector extension")
	}
	init := databaseChild(t, got, "ConfigMap", name)
	initJSON, _ := json.Marshal(init["data"])
	if !strings.Contains(strings.ToLower(string(initJSON)), "vector") || !strings.Contains(strings.ToUpper(string(initJSON)), "NOSUPERUSER") {
		t.Fatal("initialization must install vector and create an unprivileged application user")
	}
	app := databaseChild(t, got, "Deployment", "catalog")
	appPod := object(object(object(app["spec"])["template"])["spec"])
	appContainers := appPod["containers"].([]any)
	databaseURLFound := false
	for _, container := range appContainers {
		for _, env := range object(container)["env"].([]any) {
			e := object(env)
			if e["name"] == "AGENT_REGISTRY_DATABASE_URL" {
				ref := object(object(e["valueFrom"])["secretKeyRef"])
				databaseURLFound = ref["name"] == name && ref["key"] == "url"
			}
		}
	}
	if !databaseURLFound {
		t.Fatal("application database URL must use the generated dedicated credentials")
	}
	for _, volume := range appPod["volumes"].([]any) {
		secretVolume := object(object(volume)["secret"])
		if secretVolume["secretName"] != name {
			continue
		}
		items, ok := secretVolume["items"].([]any)
		if !ok || len(items) == 0 {
			t.Fatal("mounting the complete database Secret exposes the administrator password to the application")
		}
		for _, item := range items {
			if object(item)["key"] == "postgres-password" {
				t.Fatal("database administrator password must not be mounted in application pods")
			}
		}
	}
	status, _ := json.Marshal(got.Status)
	for _, key := range []string{"password", "postgres-password", "url"} {
		if strings.Contains(string(status), string(secretData(secret, key))) {
			t.Fatal("managed database credentials leaked into status")
		}
	}
	if condition(got, "Ready").Status != "False" {
		t.Fatal("creating database resources must not immediately imply readiness")
	}
}

func TestManagedDatabaseCredentialsSurviveReconciliation(t *testing.T) {
	req := managedFixture()
	first, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	name := databaseName(req.Parent)
	initialSecret := databaseChild(t, first, "Secret", name)
	req.Children = observeDatabaseChildren(t, first)
	before, _ := json.Marshal(req)
	second, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initialSecret["data"], databaseChild(t, second, "Secret", name)["data"]) {
		t.Fatal("reconciliation regenerated credentials for an initialized database")
	}
	after, _ := json.Marshal(req)
	if string(before) != string(after) {
		t.Fatal("reconciliation mutated the observed state")
	}
	// A controller restart has no in-memory state: the observed immutable Secret
	// is sufficient to reconstruct the same credentials and pod configuration.
	third, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(second, third) {
		t.Fatal("managed desired state changed with identical observations")
	}
	req.Parent.Spec.Resources = Resources{Requests: map[string]string{"cpu": "300m"}}
	reconfigured, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(initialSecret["data"], databaseChild(t, reconfigured, "Secret", name)["data"]) {
		t.Fatal("application configuration change rotated persistent database credentials")
	}
}

func TestManagedDatabaseDuplicateObservationKeepsRolloutStable(t *testing.T) {
	req := managedFixture()
	first, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	req.Children = observeDatabaseChildren(t, first)
	baseline, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	name := databaseName(req.Parent)
	related := databaseChild(t, first, "Secret", name)
	object(related["metadata"])["uid"] = "persisted-secret-uid"
	object(related["metadata"])["resourceVersion"] = "123"
	req.Related["Secret.v1"][name] = related
	for i := 0; i < 20; i++ {
		got, err := Sync(req, config())
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(databaseChild(t, baseline, "Deployment", "catalog"), databaseChild(t, got, "Deployment", "catalog")) {
			t.Fatal("observing the same managed Secret through both watches must not repeatedly roll application pods")
		}
	}
}

func TestManagedDatabaseInvalidCredentialsDoNotRotatePersistentPasswords(t *testing.T) {
	for _, changed := range []string{"username", "password", "database", "postgres-password"} {
		t.Run(changed, func(t *testing.T) {
			req := managedFixture()
			first, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			req.Children = observeDatabaseChildren(t, first)
			name := databaseName(req.Parent)
			secret := req.Children["Secret.v1"]["tenant-a/"+name]
			value := "wrong"
			if changed == "password" {
				value = strings.Repeat("b", 64) // Strong, but inconsistent with the URL.
			}
			object(secret["data"])[changed] = base64.StdEncoding.EncodeToString([]byte(value))
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(secret["data"], databaseChild(t, got, "Secret", name)["data"]) {
				t.Fatal("invalid credentials must be preserved for recovery, never replaced")
			}
			if condition(got, "DependenciesReady").Status != "False" || condition(got, "Ready").Status != "False" || number(object(databaseChild(t, got, "Deployment", "catalog")["spec"])["replicas"]) != 0 || number(object(databaseChild(t, got, "StatefulSet", name)["spec"])["replicas"]) != 0 {
				t.Fatal("inconsistent managed credentials must stop workloads until original credentials are restored")
			}
		})
	}
}

func TestManagedDatabaseMissingCredentialsNeverReinitializeExistingStorage(t *testing.T) {
	for _, retained := range []string{"StatefulSet", "PersistentVolumeClaim"} {
		t.Run(retained, func(t *testing.T) {
			req := managedFixture()
			first, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			name := databaseName(req.Parent)
			if retained == "StatefulSet" {
				req.Children = map[string]map[string]Object{"StatefulSet.apps/v1": {"tenant-a/" + name: databaseChild(t, first, retained, name)}}
			} else {
				req.Related["PersistentVolumeClaim.v1"] = map[string]Object{"tenant-a/data-" + name + "-0": {"apiVersion": "v1", "kind": "PersistentVolumeClaim", "metadata": Object{"name": "data-" + name + "-0", "namespace": "tenant-a"}}}
			}
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			for _, child := range got.Children {
				if child["kind"] == "Secret" && object(child["metadata"])["name"] == name {
					t.Fatal("lost credentials must not be silently replaced while database storage may exist")
				}
			}
			if condition(got, "DependenciesReady").Status != "False" || condition(got, "Ready").Status != "False" {
				t.Fatal("missing persistent database credentials must block readiness")
			}
			if number(object(databaseChild(t, got, "Deployment", "catalog")["spec"])["replicas"]) != 0 {
				t.Fatal("application must stop until original database credentials are restored")
			}
			databaseChild(t, got, "StatefulSet", name)
			// Restoration may be an unowned Secret seen through related resources.
			req.Related["Secret.v1"][name] = databaseChild(t, first, "Secret", name)
			restored, err := Sync(req, config())
			if err != nil || condition(restored, "DependenciesReady").Status != "True" {
				t.Fatalf("restoring original credentials must unblock the installation: %v %+v", err, restored.Status)
			}
			if !reflect.DeepEqual(databaseChild(t, first, "Secret", name)["data"], databaseChild(t, restored, "Secret", name)["data"]) {
				t.Fatal("restored database credentials were overwritten")
			}
		})
	}
}

func TestManagedDatabaseStorageAndLocalDependencies(t *testing.T) {
	for _, tc := range []struct {
		name, storage string
		class         *string
	}{
		{name: "default", storage: "1Gi"},
		{name: "explicit", storage: "5Gi", class: stringPointer("fast-local")},
		{name: "no-storage-class", storage: "1Gi", class: stringPointer("")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := managedFixture()
			if tc.name == "explicit" {
				req.Parent.Spec.Database.Storage = tc.storage
			}
			req.Parent.Spec.Database.StorageClassName = tc.class
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			sts := databaseChild(t, got, "StatefulSet", databaseName(req.Parent))
			spec := object(sts["spec"])
			retention := object(spec["persistentVolumeClaimRetentionPolicy"])
			if retention["whenDeleted"] != "Retain" || retention["whenScaled"] != "Retain" {
				t.Fatal("database data must survive deletion and scaling")
			}
			claims := spec["volumeClaimTemplates"].([]any)
			if len(claims) != 1 || object(object(claims[0])["metadata"])["name"] != "data" {
				t.Fatal("database needs a stable data volume claim")
			}
			claimSpec := object(object(claims[0])["spec"])
			if object(object(claimSpec["resources"])["requests"])["storage"] != tc.storage {
				t.Fatal("database storage request not applied")
			}
			class, present := claimSpec["storageClassName"]
			if tc.class == nil && present || tc.class != nil && class != *tc.class {
				t.Fatal("must preserve the distinction between default and explicitly empty storageClassName")
			}
		})
	}
	req := managedFixture()
	req.Parent.Spec.Exposure = &Exposure{Hostname: "catalog.example.test", TLSSecretName: "tls"}
	customize, err := Customize(req.Parent)
	if err != nil {
		t.Fatal(err)
	}
	if len(customize.RelatedResources) != 2 {
		t.Fatalf("expected exact Secret and retained PVC watches: %+v", customize)
	}
	for _, rule := range customize.RelatedResources {
		if rule.APIVersion != "v1" || rule.Namespace != "tenant-a" {
			t.Fatalf("cross-namespace database dependency: %+v", rule)
		}
		switch rule.Resource {
		case "secrets":
			if !reflect.DeepEqual(rule.Names, []string{"auth", "catalog-postgres", "tls"}) {
				t.Fatalf("unexpected managed Secret dependencies: %+v", rule)
			}
		case "persistentvolumeclaims":
			if !reflect.DeepEqual(rule.Names, []string{"data-catalog-postgres-0"}) {
				t.Fatalf("retained database PVC watch must be exact: %+v", rule)
			}
		default:
			t.Fatalf("unexpected related resource: %+v", rule)
		}
	}
}

func stringPointer(s string) *string { return &s }

func labelsMatch(selector, labels Object) bool {
	for key, value := range selector {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func TestManagedDatabaseNetworkSelectorsSeparateApplicationAndDatabase(t *testing.T) {
	req := managedFixture()
	got, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	name := databaseName(req.Parent)
	appSpec := object(databaseChild(t, got, "Deployment", "catalog")["spec"])
	dbSpec := object(databaseChild(t, got, "StatefulSet", name)["spec"])
	appLabels := object(object(object(appSpec["template"])["metadata"])["labels"])
	dbLabels := object(object(object(dbSpec["template"])["metadata"])["labels"])
	appSelector := object(object(appSpec["selector"])["matchLabels"])
	serviceSelector := object(object(databaseChild(t, got, "Service", "catalog")["spec"])["selector"])
	for _, selector := range []Object{appSelector, serviceSelector} {
		if !labelsMatch(selector, appLabels) || labelsMatch(selector, dbLabels) {
			t.Fatal("application controller and Service selectors must exclude database pods")
		}
	}
	dbServiceSelector := object(object(databaseChild(t, got, "Service", name)["spec"])["selector"])
	if !labelsMatch(dbServiceSelector, dbLabels) || labelsMatch(dbServiceSelector, appLabels) {
		t.Fatal("database Service must only select database pods")
	}
	dbPolicy := object(databaseChild(t, got, "NetworkPolicy", name)["spec"])
	if !labelsMatch(object(object(dbPolicy["podSelector"])["matchLabels"]), dbLabels) {
		t.Fatal("database NetworkPolicy does not select its database")
	}
	ingress := dbPolicy["ingress"].([]any)
	if len(ingress) != 1 {
		t.Fatal("database must have one scoped ingress rule")
	}
	peers := object(ingress[0])["from"].([]any)
	if len(peers) != 1 {
		t.Fatal("database ingress must only allow its application")
	}
	peer := object(peers[0])
	selector := object(object(peer["podSelector"])["matchLabels"])
	if _, allNamespaces := peer["namespaceSelector"]; allNamespaces || !labelsMatch(selector, appLabels) || labelsMatch(selector, dbLabels) || len(selector) == 0 {
		t.Fatal("database ingress must select only namespace-local application pods")
	}
}

func TestManagedDatabaseAuthFailureKeepsDatabase(t *testing.T) {
	req := managedFixture()
	first, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	req.Children = observeDatabaseChildren(t, first)
	delete(req.Related["Secret.v1"], "auth")
	got, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Children) != len(first.Children) {
		t.Fatal("missing auth deleted database resources")
	}
	if number(object(databaseChild(t, got, "Deployment", "catalog")["spec"])["replicas"]) != 0 || number(object(databaseChild(t, got, "StatefulSet", databaseName(req.Parent))["spec"])["replicas"]) != 1 {
		t.Fatal("auth failure should stop the application while keeping PostgreSQL running")
	}
	if !reflect.DeepEqual(databaseChild(t, first, "Secret", databaseName(req.Parent))["data"], databaseChild(t, got, "Secret", databaseName(req.Parent))["data"]) {
		t.Fatal("auth failure rotated database credentials")
	}
}

func TestManagedDatabaseReadinessRequiresHealthyStatefulSet(t *testing.T) {
	req := managedFixture()
	req.Parent.Spec.Database.Storage = "1024Mi"
	desired, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	req.Children = observeDatabaseChildren(t, desired)
	// Reconcile once with the generated Secret now observed before completing
	// both rollouts, so the app uses the current dependency fingerprint.
	desired, err = Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	req.Children = observeDatabaseChildren(t, desired)
	for _, group := range req.Children {
		for _, child := range group {
			switch child["kind"] {
			case "NetworkPolicy":
				// The API omits empty rule arrays while policyTypes still denies egress.
				if object(child["metadata"])["name"] == databaseName(req.Parent) {
					delete(object(child["spec"]), "egress")
				}
			case "Deployment", "StatefulSet":
				object(child["metadata"])["generation"] = 1
				child["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "availableReplicas": 1, "updatedReplicas": 1, "currentReplicas": 1, "currentRevision": "revision-1", "updateRevision": "revision-1"}
				if child["kind"] == "StatefulSet" {
					// Kubernetes canonicalizes equivalent quantities on persistence.
					claims := object(child["spec"])["volumeClaimTemplates"].([]any)
					object(object(object(object(claims[0])["spec"])["resources"])["requests"])["storage"] = "1Gi"
				}
			}
		}
	}
	ready, err := Sync(req, config())
	if err != nil || condition(ready, "Ready").Status != "True" {
		t.Fatalf("complete observed managed installation should be ready: %v %+v", err, ready.Status)
	}
	for _, group := range req.Children {
		for _, child := range group {
			if child["kind"] == "StatefulSet" {
				object(child["status"])["readyReplicas"] = 0
			}
		}
	}
	unready, err := Sync(req, config())
	if err != nil || condition(unready, "Ready").Status != "False" {
		t.Fatalf("healthy application must not hide a database outage: %v %+v", err, unready.Status)
	}
}

func TestDatabaseModeValidationAndExternalCompatibility(t *testing.T) {
	for name, database := range map[string]Database{
		"host-only":   {Host: "postgres.tenant-a.svc.cluster.local"},
		"secret-only": {SecretName: "database"},
		"zero-volume": {Storage: "0Gi"},
		"bad-volume":  {Storage: "unlimited"},
	} {
		t.Run(name, func(t *testing.T) {
			req := managedFixture()
			req.Parent.Spec.Database = database
			if _, err := Sync(req, config()); err == nil {
				t.Fatal("invalid or incomplete database settings were accepted")
			}
		})
	}
	req := fixture()
	got, err := Sync(req, config())
	if err != nil || len(got.Children) != 4 {
		t.Fatalf("external database compatibility lost: %v %+v", err, got)
	}
	for _, child := range got.Children {
		if child["kind"] == "Secret" || child["kind"] == "StatefulSet" {
			t.Fatal("external mode must not provision a second database")
		}
	}
}

func TestManagedDatabaseNamesSupportFullLengthRegistryNames(t *testing.T) {
	seen := map[string]bool{}
	for _, parentName := range []string{strings.Repeat("a", 54), strings.Repeat("a", 63), strings.Repeat("a", 62) + "b"} {
		req := managedFixture()
		req.Parent.Metadata.Name = parentName
		name := databaseName(req.Parent)
		if !localName(name) || !serviceLabel.MatchString(name) || seen[name] {
			t.Fatalf("full-length parent needs a valid, distinct database Service name, got %q", name)
		}
		seen[name] = true
		got, err := Sync(req, config())
		if err != nil {
			t.Fatal(err)
		}
		databaseChild(t, got, "Service", name)
		customize, err := Customize(req.Parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range customize.RelatedResources {
			if rule.Resource == "persistentvolumeclaims" && !reflect.DeepEqual(rule.Names, []string{"data-" + name + "-0"}) {
				t.Fatal("retained PVC watch disagrees with the shortened database name")
			}
		}
	}
}
