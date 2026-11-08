package operator

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestReadinessRequiresExactNetworkPolicies(t *testing.T) {
	for _, mode := range []struct {
		name              string
		managed, database bool
	}{
		{"external application", false, false},
		{"managed application", true, false},
		{"managed database", true, true},
	} {
		t.Run(mode.name, func(t *testing.T) {
			cases := []struct {
				name   string
				mutate func(Object)
			}{
				{"unmatched policy selector", func(spec Object) {
					object(object(spec["podSelector"])["matchLabels"])["example.test/unexpected"] = "no-matching-pods"
				}},
				{"cross namespace ingress", func(spec Object) {
					ingress := object(spec["ingress"].([]any)[0])
					// An empty namespace selector broadens the existing pod selector
					// from this namespace to all namespaces.
					object(ingress["from"].([]any)[0])["namespaceSelector"] = Object{}
				}},
				{"ingress port range", func(spec Object) {
					ingress := object(spec["ingress"].([]any)[0])
					object(ingress["ports"].([]any)[0])["endPort"] = 65535
				}},
				{"additional egress access", func(spec Object) {
					if mode.database {
						// The desired database policy has no egress rules. An empty
						// rule instead permits every destination and port.
						spec["egress"] = []Object{{}}
						return
					}
					egress := object(spec["egress"].([]any)[0])
					object(egress["to"].([]any)[0])["namespaceSelector"] = Object{}
				}},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					req := fixture()
					if mode.managed {
						req = managedFixture()
					}
					desired, err := Sync(req, config())
					if err != nil {
						t.Fatal(err)
					}
					if mode.managed {
						desired = provisionManagedDatabase(t, req)
					}
					req.Children = observeDatabaseChildren(t, desired)
					for _, group := range req.Children {
						for _, child := range group {
							switch child["kind"] {
							case "Deployment", "StatefulSet":
								object(child["metadata"])["generation"] = 1
								child["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "availableReplicas": 1, "updatedReplicas": 1, "currentRevision": "revision-1", "updateRevision": "revision-1"}
							case "NetworkPolicy":
								// Unrelated metadata does not change the policy's traffic rules.
								object(child["metadata"])["annotations"] = Object{"example.test/owner": "platform"}
							}
						}
					}
					req.Parent.Status.InstalledVersion = "0.3.2"
					ready, err := Sync(req, config())
					if err != nil || condition(ready, "Ready").Status != "True" {
						t.Fatalf("healthy installation with unrelated policy annotations must be ready: %v %+v", err, ready.Status)
					}
					name := req.Parent.Metadata.Name
					if mode.database {
						name = databaseName(req.Parent)
					}
					policy := req.Children["NetworkPolicy.networking.k8s.io/v1"][req.Parent.Metadata.Namespace+"/"+name]
					original := asMap(t, policy["spec"])
					tc.mutate(object(policy["spec"]))
					before, _ := json.Marshal(req)
					blocked, err := Sync(req, config())
					if err != nil {
						t.Fatal(err)
					}
					if condition(blocked, "Ready").Status != "False" || condition(blocked, "Ready").Reason != "Progressing" || blocked.Status.InstalledVersion != "0.3.2" {
						t.Fatalf("policy drift must block readiness and installed-version advancement: %+v", blocked.Status)
					}
					if condition(blocked, "DependenciesReady").Status != "True" || !reflect.DeepEqual(blocked.Children, ready.Children) {
						t.Fatal("policy drift must preserve desired resources and valid dependencies")
					}
					after, _ := json.Marshal(req)
					if string(before) != string(after) {
						t.Fatal("readiness check mutated observed resources")
					}
					policy["spec"] = original
					recovered, err := Sync(req, config())
					if err != nil || condition(recovered, "Ready").Status != "True" || recovered.Status.InstalledVersion != "0.3.3" {
						t.Fatalf("observing the repaired policy must restore readiness: %v %+v", err, recovered.Status)
					}
				})
			}
		})
	}
}
