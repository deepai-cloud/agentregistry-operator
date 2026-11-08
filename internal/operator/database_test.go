package operator

import (
	"encoding/base64"
	"encoding/json"
	"net/url"
	"os/exec"
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

func observeDatabaseChildren(t testing.TB, r Response) map[string]map[string]Object {
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

// Complete the credential-persistence phase before inspecting a provisioned database.
func provisionManagedDatabase(t testing.TB, req Request) Response {
	t.Helper()
	initial, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	req.Children = observeDatabaseChildren(t, initial)
	got, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// Measure the recurring readiness comparison with all nine managed children
// present in the same JSON representation delivered by the webhook.
func BenchmarkManagedChildrenCurrent(b *testing.B) {
	req := managedFixture()
	desired := provisionManagedDatabase(b, req)
	req.Children = observeDatabaseChildren(b, desired)
	b.ReportAllocs()
	for b.Loop() {
		if !observedCurrent(req, desired.Children) {
			b.Fatal("matching children reported as stale")
		}
	}
}

func TestManagedDatabaseWaitsForPersistedCredentials(t *testing.T) {
	for _, watch := range []string{"children", "related"} {
		t.Run(watch, func(t *testing.T) {
			req := managedFixture()
			name := databaseName(req.Parent)
			var secret Object
			for attempt := 0; attempt < 3; attempt++ {
				got, err := Sync(req, config())
				if err != nil {
					t.Fatal(err)
				}
				secret = databaseChild(t, got, "Secret", name)
				for _, child := range got.Children {
					if child["kind"] == "StatefulSet" {
						t.Fatal("must persist credentials before creating a StatefulSet that could provision storage")
					}
				}
				if number(object(databaseChild(t, got, "Deployment", "catalog")["spec"])["replicas"]) != 0 {
					t.Fatal("application must wait for persisted database credentials")
				}
				if condition(got, "Ready").Status != "False" || condition(got, "DependenciesReady").Status != "False" ||
					condition(got, "DependenciesReady").Reason != "Provisioning" || got.ResyncAfterSeconds <= 0 {
					t.Fatal("pending credentials must report provisioning and retry")
				}
				// Metacontroller may create every other child even if Secret creation fails.
				req.Children = observeDatabaseChildren(t, got)
				delete(req.Children, "Secret.v1")
			}
			if watch == "children" {
				req.Children["Secret.v1"] = map[string]Object{"tenant-a/" + name: secret}
			} else {
				req.Related["Secret.v1"][name] = secret
			}
			started, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(secret["data"], databaseChild(t, started, "Secret", name)["data"]) {
				t.Fatal("startup must reuse the credentials that were successfully persisted")
			}
			if condition(started, "DependenciesReady").Status != "True" ||
				number(object(databaseChild(t, started, "StatefulSet", name)["spec"])["replicas"]) != 1 ||
				number(object(databaseChild(t, started, "Deployment", "catalog")["spec"])["replicas"]) != 1 {
				t.Fatal("observing persisted credentials must unblock database and application startup")
			}
			req.Children = observeDatabaseChildren(t, started)
			again, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(started.Children, again.Children) {
				t.Fatal("a controller restart after credential persistence must preserve the desired resources")
			}
		})
	}
}

func TestManagedDatabaseWaitsForDeletingCredentials(t *testing.T) {
	for _, watch := range []string{"child", "related", "related-ahead-of-child"} {
		t.Run(watch, func(t *testing.T) {
			req := managedFixture()
			name := databaseName(req.Parent)
			initial, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			req.Children = observeDatabaseChildren(t, initial)
			secret := req.Children["Secret.v1"][req.Parent.Metadata.Namespace+"/"+name]
			object(secret["metadata"])["uid"] = "deleting-secret"
			deleting := asMap(t, secret)
			object(deleting["metadata"])["deletionTimestamp"] = "2026-11-04T12:00:00Z"
			switch watch {
			case "child":
				req.Children["Secret.v1"][req.Parent.Metadata.Namespace+"/"+name] = deleting
			case "related":
				delete(req.Children, "Secret.v1")
				req.Related["Secret.v1"][name] = deleting
			case "related-ahead-of-child":
				req.Related["Secret.v1"][name] = deleting
			}
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if condition(got, "DependenciesReady").Reason != "Provisioning" ||
				number(object(databaseChild(t, got, "Deployment", req.Parent.Metadata.Name)["spec"])["replicas"]) != 0 {
				t.Fatal("a terminating database Secret must not start a fresh installation")
			}
			for _, child := range got.Children {
				if child["kind"] == "StatefulSet" || child["kind"] == "Secret" {
					t.Fatal("must wait for Secret deletion before replacing credentials or provisioning storage")
				}
			}
			if watch == "related-ahead-of-child" {
				// Once the child watch observes a replacement, the old UID's
				// deletion timestamp must not withhold the replacement Secret.
				object(secret["metadata"])["uid"] = "replacement-secret"
				resumed, err := Sync(req, config())
				if err != nil || condition(resumed, "DependenciesReady").Status != "True" ||
					!reflect.DeepEqual(secret["data"], databaseChild(t, resumed, "Secret", name)["data"]) {
					t.Fatal("stale deletion blocked use of the persisted replacement")
				}
			}
			delete(req.Children, "Secret.v1")
			delete(req.Related["Secret.v1"], name)
			retried := provisionManagedDatabase(t, req)
			if condition(retried, "DependenciesReady").Status != "True" {
				t.Fatal("fresh bootstrap did not resume after the deleting Secret disappeared")
			}
		})
	}
}

func TestManagedDatabaseProvisionsCompleteInstallation(t *testing.T) {
	req := managedFixture()
	got := provisionManagedDatabase(t, req)
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

func TestManagedDatabaseWiresResumableStartup(t *testing.T) {
	req := managedFixture()
	name := databaseName(req.Parent)
	initial, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	req.Children = observeDatabaseChildren(t, initial)
	// Additional keys can be present when credentials have been persisted
	// and the StatefulSet is about to initialize its first database.
	observed := req.Children["ConfigMap.v1"][req.Parent.Metadata.Namespace+"/"+name]
	object(observed["data"])["99-extra.sql"] = "SELECT 1;"
	got, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
	cm := databaseChild(t, got, "ConfigMap", name)
	data := object(cm["data"])
	template := object(object(databaseChild(t, got, "StatefulSet", name)["spec"])["template"])
	pod := object(template["spec"])
	container := object(pod["containers"].([]any)[0])
	if !reflect.DeepEqual(container["command"], []any{"/bin/bash", "/docker-entrypoint-initdb.d/start-postgres"}) ||
		!reflect.DeepEqual(container["args"], []any{"postgres"}) {
		t.Fatal("PostgreSQL must invoke the resume wrapper before the image entrypoint")
	}
	if object(object(template["metadata"])["annotations"])[fingerprintKey] != hash(data) {
		t.Fatal("bootstrap script updates must roll out the database pod")
	}
	mounted := false
	for _, mount := range container["volumeMounts"].([]any) {
		m := object(mount)
		if m["mountPath"] != "/docker-entrypoint-initdb.d" || m["readOnly"] != true {
			continue
		}
		for _, volume := range pod["volumes"].([]any) {
			v := object(volume)
			source := object(v["configMap"])
			if v["name"] != m["name"] || source["name"] != name || number(source["defaultMode"]) != 0444 {
				continue
			}
			items, _ := source["items"].([]any)
			files := Object{}
			for _, raw := range items {
				item := object(raw)
				path, _ := item["path"].(string)
				files[path] = item["key"]
			}
			if !reflect.DeepEqual(files, Object{"01-registry.sh": "01-registry.sh", "start-postgres": "start-postgres"}) {
				t.Fatalf("only generated scripts may reach PostgreSQL's init directory; got items: %v", items)
			}
			mounted = true
		}
	}
	if !mounted {
		t.Fatal("startup scripts must be mounted from the generated ConfigMap as non-executable, read-only files")
	}
	for _, filename := range []string{"01-registry.sh", "start-postgres"} {
		script, ok := data[filename].(string)
		if !ok || script == "" {
			t.Fatalf("missing startup script %s", filename)
		}
		cmd := exec.Command("bash", "-n")
		cmd.Stdin = strings.NewReader(script)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("invalid shell script %s: %s (%v)", filename, output, err)
		}
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
			first := provisionManagedDatabase(t, req)
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

func TestManagedDatabaseRejectsInvalidRestoredCredentials(t *testing.T) {
	req := managedFixture()
	name := databaseName(req.Parent)
	first := provisionManagedDatabase(t, req)
	original := databaseChild(t, first, "Secret", name)
	claim := "data-" + name + "-0"
	req.Related["PersistentVolumeClaim.v1"] = map[string]Object{claim: {
		"apiVersion": "v1", "kind": "PersistentVolumeClaim",
		"metadata": Object{"name": claim, "namespace": req.Parent.Metadata.Namespace},
	}}

	for _, bad := range []string{"shared password", "NUL in role password", "NUL in admin password", "non-PostgreSQL URI", "URI fragment", "uppercase scheme", "mixed-case scheme"} {
		t.Run(bad, func(t *testing.T) {
			restored := asMap(t, original)
			data := object(restored["data"])
			u, err := url.Parse(string(secretData(restored, "url")))
			if err != nil {
				t.Fatal("invalid generated URL")
			}
			switch bad {
			case "shared password":
				data["password"] = data["postgres-password"]
			case "NUL in role password", "NUL in admin password":
				key := "password"
				if bad == "NUL in admin password" {
					key = "postgres-password"
				}
				data[key] = base64.StdEncoding.EncodeToString(append(secretData(restored, key), 0, 'x'))
			case "non-PostgreSQL URI":
				u.Scheme = "https"
			case "URI fragment":
				u.Fragment = "unexpected"
			case "uppercase scheme":
				u.Scheme = "POSTGRES"
			case "mixed-case scheme":
				u.Scheme = "PostgreSQL"
			}
			// Preserve URL/password agreement so it doesn't mask the bad restore.
			u.User = url.UserPassword("registry", string(secretData(restored, "password")))
			data["url"] = base64.StdEncoding.EncodeToString([]byte(u.String()))
			req.Related["Secret.v1"][name] = restored
			got, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			app := number(object(databaseChild(t, got, "Deployment", req.Parent.Metadata.Name)["spec"])["replicas"])
			db := number(object(databaseChild(t, got, "StatefulSet", name)["spec"])["replicas"])
			if condition(got, "DependenciesReady").Reason != "InvalidOrMissingSecret" ||
				condition(got, "Ready").Reason != "DependencyNotReady" || app != 0 || db != 0 {
				t.Fatalf("bad restore requested %.0f registry and %.0f PostgreSQL replicas, want both stopped", app, db)
			}
			if !reflect.DeepEqual(data, databaseChild(t, got, "Secret", name)["data"]) {
				t.Fatal("operator changed the restored credentials")
			}
			req.Related["Secret.v1"][name] = original
			repaired, err := Sync(req, config())
			if err != nil || condition(repaired, "DependenciesReady").Status != "True" || !reflect.DeepEqual(repaired.Children, first.Children) {
				t.Fatal("restoring the original credentials must resume both workloads with their original configuration")
			}
		})
	}
}

func TestManagedDatabaseMissingCredentialsNeverReinitializeExistingStorage(t *testing.T) {
	for _, retained := range []string{"StatefulSet", "PersistentVolumeClaim"} {
		t.Run(retained, func(t *testing.T) {
			req := managedFixture()
			first := provisionManagedDatabase(t, req)
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
			if number(object(databaseChild(t, got, "StatefulSet", name)["spec"])["replicas"]) != 0 {
				t.Fatal("database must remain stopped while its persistent credentials are missing")
			}
			// Restoration may be an unowned Secret seen through related resources.
			deleting := databaseChild(t, first, "Secret", name)
			object(deleting["metadata"])["deletionTimestamp"] = "2026-11-04T12:00:00Z"
			req.Related["Secret.v1"][name] = deleting
			blocked, err := Sync(req, config())
			if err != nil || !reflect.DeepEqual(blocked.Children, got.Children) ||
				condition(blocked, "DependenciesReady").Reason != "InvalidOrMissingSecret" {
				t.Fatal("terminating credentials must leave existing storage in the same stopped recovery state as missing credentials")
			}
			req.Related["Secret.v1"][name] = databaseChild(t, first, "Secret", name)
			restored, err := Sync(req, config())
			if err != nil || condition(restored, "DependenciesReady").Status != "True" {
				t.Fatalf("restoring original credentials must unblock the installation: %v %+v", err, restored.Status)
			}
			if !reflect.DeepEqual(databaseChild(t, first, "Secret", name)["data"], databaseChild(t, restored, "Secret", name)["data"]) {
				t.Fatal("restored database credentials were overwritten")
			}
			if number(object(databaseChild(t, restored, "StatefulSet", name)["spec"])["replicas"]) != 1 {
				t.Fatal("restoring original credentials must restart the database")
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
			got := provisionManagedDatabase(t, req)
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
	got := provisionManagedDatabase(t, req)
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
	first := provisionManagedDatabase(t, req)
	req.Children = observeDatabaseChildren(t, first)
	object(req.Related["Secret.v1"]["auth"]["metadata"])["deletionTimestamp"] = "2026-11-04T12:00:00Z"
	terminating, err := Sync(req, config())
	if err != nil {
		t.Fatal(err)
	}
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
	if !reflect.DeepEqual(terminating.Children, got.Children) {
		t.Fatal("terminating auth must stop the registry and preserve PostgreSQL just as missing auth does")
	}
}

func TestManagedDatabaseReadinessRequiresHealthyStatefulSet(t *testing.T) {
	for _, tc := range []struct{ input, canonical string }{
		{"1024Mi", "1Gi"},
		{"1024Gi", "1Ti"},
		{"1073741824Mi", "1Pi"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			req := managedFixture()
			req.Parent.Spec.Database.Storage = tc.input
			desired, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			req.Children = observeDatabaseChildren(t, desired)
			// Once the generated Secret is observed, reconciliation creates the database
			// workload and starts the application before completing both rollouts.
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
							object(object(object(object(claims[0])["spec"])["resources"])["requests"])["storage"] = tc.canonical
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
		})
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
	for _, parentName := range []string{"catalog", strings.Repeat("a", 52), strings.Repeat("a", 53), strings.Repeat("a", 54), strings.Repeat("a", 55), strings.Repeat("a", 63), strings.Repeat("a", 62) + "b"} {
		req := managedFixture()
		req.Parent.Metadata.Name = parentName
		name := databaseName(req.Parent)
		if !localName(name) || !serviceLabel.MatchString(name) || seen[name] {
			t.Fatalf("full-length parent needs a valid, distinct database Service name, got %q", name)
		}
		seen[name] = true
		if !localName(name + "-0") {
			t.Errorf("PostgreSQL pod hostname exceeds the DNS label limit for a %d-character parent: %q", len(parentName), name+"-0")
		}
		if len(parentName) <= 52 && name != parentName+"-postgres" {
			t.Fatalf("existing valid database identity changed: %q", name)
		}
		got := provisionManagedDatabase(t, req)
		databaseChild(t, got, "Service", name)
		sts := databaseChild(t, got, "StatefulSet", name)
		if object(sts["spec"])["serviceName"] != name {
			t.Fatal("StatefulSet must use the same shortened headless Service name")
		}
		customize, err := Customize(req.Parent)
		if err != nil {
			t.Fatal(err)
		}
		for _, rule := range customize.RelatedResources {
			if rule.Resource == "persistentvolumeclaims" {
				found := false
				for _, watched := range rule.Names {
					found = found || watched == "data-"+name+"-0"
				}
				if !found {
					t.Fatal("retained PVC watch disagrees with the shortened database name")
				}
			}
		}
	}
}
