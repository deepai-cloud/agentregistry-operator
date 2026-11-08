package operator

import (
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// Freeze the old names independently of the compatibility helper: these may
// already identify credentials and retained storage created by an earlier build.
var legacyDatabaseNames = []struct{ parent, database string }{
	{strings.Repeat("a", 53), strings.Repeat("a", 53) + "-postgres"},
	{strings.Repeat("a", 54), strings.Repeat("a", 54) + "-postgres"},
	{strings.Repeat("a", 55), strings.Repeat("a", 43) + "-899c27160f-postgres"},
	{strings.Repeat("a", 63), strings.Repeat("a", 43) + "-e7d55ca106-postgres"},
}

func TestLongDatabaseNamesWatchLegacyCredentialsAndStorage(t *testing.T) {
	for _, tc := range legacyDatabaseNames {
		req := managedFixture()
		req.Parent.Metadata.Name = tc.parent
		current := databaseName(req.Parent)
		if legacyDatabaseName(req.Parent) != tc.database || current == tc.database {
			t.Fatalf("lost previous database identity for %q", tc.parent)
		}
		customize, err := Customize(req.Parent)
		if err != nil {
			t.Fatal(err)
		}
		secrets := []string{req.Parent.Spec.AuthSecretName, current, tc.database}
		claims := []string{"data-" + current + "-0", "data-" + tc.database + "-0"}
		sort.Strings(secrets)
		sort.Strings(claims)
		want := []RelatedRule{
			{APIVersion: "v1", Resource: "secrets", Namespace: req.Parent.Metadata.Namespace, Names: secrets},
			{APIVersion: "v1", Resource: "persistentvolumeclaims", Namespace: req.Parent.Metadata.Namespace, Names: claims},
		}
		if !reflect.DeepEqual(customize.RelatedResources, want) {
			t.Fatalf("retained database state is missing from namespace-local watches: %#v", customize)
		}
	}
}

func TestLegacyDatabaseStateBlocksAutomaticRename(t *testing.T) {
	for _, tc := range legacyDatabaseNames {
		for _, kind := range []string{"Secret", "StatefulSet", "PersistentVolumeClaim"} {
			for _, source := range []string{"children", "related"} {
				t.Run(tc.database+"/"+kind+"/"+source, func(t *testing.T) {
					req := managedFixture()
					req.Parent.Metadata.Name = tc.parent
					name := tc.database
					if kind == "PersistentVolumeClaim" {
						name = "data-" + name + "-0"
					}
					old := Object{"kind": kind, "metadata": Object{"name": name, "namespace": req.Parent.Metadata.Namespace}, "data": Object{"password": "preserve-existing-credentials"}}
					if source == "children" {
						req.Children = map[string]map[string]Object{"legacy": {name: old}}
					} else {
						req.Related["legacy"] = map[string]Object{name: old}
					}
					before, _ := json.Marshal(req)
					got, err := Sync(req, config())
					if err == nil || !strings.Contains(err.Error(), "legacy managed database "+kind+" "+name) {
						t.Fatalf("legacy state must require recovery before credentials or storage can be replaced: %v", err)
					}
					if len(got.Children) != 0 || strings.Contains(err.Error(), "preserve-existing-credentials") {
						t.Fatal("failed reconciliation must not return replacements or expose credentials")
					}
					after, _ := json.Marshal(req)
					if string(before) != string(after) {
						t.Fatal("legacy observations were modified")
					}
					// A same-named object in another namespace must not interfere.
					object(old["metadata"])["namespace"] = "another-tenant"
					if _, err := Sync(req, config()); err != nil {
						t.Fatalf("foreign namespace blocked a new database: %v", err)
					}
					// Removing obsolete state after an explicit recovery unblocks setup.
					req.Children = nil
					delete(req.Related, "legacy")
					started := provisionManagedDatabase(t, req)
					databaseChild(t, started, "StatefulSet", databaseName(req.Parent))
					if condition(started, "DependenciesReady").Status != "True" {
						t.Fatal("corrected database did not become provisionable")
					}
				})
			}
		}
	}
}
