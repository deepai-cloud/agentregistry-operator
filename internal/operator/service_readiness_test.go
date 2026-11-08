package operator

import (
	"encoding/json"
	"testing"
)

func TestReadinessRequiresExactServiceSelectors(t *testing.T) {
	for _, tc := range []struct {
		name              string
		managed, database bool
	}{
		{"external application", false, false},
		{"managed application", true, false},
		{"managed database", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixture()
			if tc.managed {
				req = managedFixture()
			}
			desired, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if tc.managed {
				desired = provisionManagedDatabase(t, req)
			}
			req.Children = observeDatabaseChildren(t, desired)
			for _, group := range req.Children {
				for _, child := range group {
					switch child["kind"] {
					case "Deployment", "StatefulSet":
						object(child["metadata"])["generation"] = 1
						child["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "availableReplicas": 1, "updatedReplicas": 1, "currentRevision": "revision-1", "updateRevision": "revision-1"}
					case "Service":
						// API defaults and unrelated metadata still allow readiness.
						object(child["spec"])["sessionAffinity"] = "None"
						object(child["metadata"])["annotations"] = Object{"example.test/owner": "platform"}
					}
				}
			}
			req.Parent.Status.InstalledVersion = "0.3.2"
			ready, err := Sync(req, config())
			if err != nil || condition(ready, "Ready").Status != "True" {
				t.Fatalf("healthy installation with Service defaults must be ready: %v %+v", err, ready.Status)
			}
			name := req.Parent.Metadata.Name
			if tc.database {
				name = databaseName(req.Parent)
			}
			service := req.Children["Service.v1"][req.Parent.Metadata.Namespace+"/"+name]
			selector := object(object(service["spec"])["selector"])
			// This label is absent from the pods. Keeping the intended labels
			// alongside it still leaves this Service without matching endpoints.
			selector["example.test/unexpected"] = "no-matching-pods"
			before, _ := json.Marshal(req)
			blocked, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			if condition(blocked, "Ready").Status != "False" || condition(blocked, "Ready").Reason != "Progressing" || blocked.Status.InstalledVersion != "0.3.2" {
				t.Fatalf("Service selector drift must block readiness and installed-version advancement: %+v", blocked.Status)
			}
			if condition(blocked, "DependenciesReady").Status != "True" || len(blocked.Children) != len(desired.Children) || number(object(find(t, blocked, "Deployment")["spec"])["replicas"]) != 1 {
				t.Fatal("selector repair must preserve running workloads and valid dependencies")
			}
			fixed := databaseChild(t, blocked, "Service", name)
			if _, exists := object(object(fixed["spec"])["selector"])["example.test/unexpected"]; exists {
				t.Fatal("desired Service retained the unexpected selector")
			}
			after, _ := json.Marshal(req)
			if string(before) != string(after) {
				t.Fatal("readiness check mutated observed resources")
			}
			delete(selector, "example.test/unexpected")
			recovered, err := Sync(req, config())
			if err != nil || condition(recovered, "Ready").Status != "True" || recovered.Status.InstalledVersion != "0.3.3" {
				t.Fatalf("observing the repaired selector must restore readiness: %v %+v", err, recovered.Status)
			}
		})
	}
}
