package operator

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestResourceValidationCases(t *testing.T) {
	data, err := os.ReadFile("testdata/resource-validation.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name      string    `json:"name"`
		Resources Resources `json:"resources"`
		Valid     bool      `json:"valid"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			if _, err := resources(tc.Resources); (err == nil) != tc.Valid {
				t.Fatalf("resource validation disagrees with admission case: valid=%v, error=%v", tc.Valid, err)
			}
		})
	}
}

func TestResourceQuantitiesNormalizeExactly(t *testing.T) {
	for _, tc := range []struct{ input, cpu string }{
		{"0.009", "9m"},
		{"9m", "9m"},
		{"0001.000", "1"},
		{"1000", "1k"},
		{"1000.000", "1k"},
		{"1000000m", "1k"},
		{"1000000", "1M"},
		{"1000000000", "1G"},
		{"1000000000000", "1T"},
		{"1000000000000000", "1P"},
		{"1500", "1500"},
		{"1000.001", "1000001m"},
		{"1125899906842624", "1125899906842624"},
		{"1125899906842623.999", "1125899906842623999m"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			got, err := resources(Resources{
				Requests: map[string]string{"cpu": tc.input, "memory": "1024Mi"},
				Limits:   map[string]string{"cpu": tc.input, "memory": "1Gi"},
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, values := range []map[string]string{got.Requests, got.Limits} {
				if values["cpu"] != tc.cpu || values["memory"] != "1Gi" {
					t.Fatalf("normalization changed a quantity's exact value: %+v", values)
				}
			}
		})
	}
}

func TestCanonicalCPUDoesNotBlockReadiness(t *testing.T) {
	// Kubernetes persists these whole-core quantities with DecimalSI suffixes.
	// A healthy observed Deployment must match the operator's desired template.
	for _, tc := range []struct{ input, canonical string }{
		{"1000", "1k"},
		{"1000000m", "1k"},
		{"1000000", "1M"},
		{"1000000000", "1G"},
		{"1000000000000", "1T"},
		{"1000000000000000", "1P"},
	} {
		t.Run(tc.input, func(t *testing.T) {
			req := fixture()
			req.Parent.Spec.Resources = Resources{
				Requests: map[string]string{"cpu": tc.input},
				Limits:   map[string]string{"cpu": tc.input},
			}
			desired, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			dep := asMap(t, find(t, desired, "Deployment"))
			object(dep["metadata"])["generation"] = 1
			dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
			pod := object(object(object(dep["spec"])["template"])["spec"])
			registry := object(pod["containers"].([]any)[0])
			for _, group := range []string{"requests", "limits"} {
				object(object(registry["resources"])[group])["cpu"] = tc.canonical
			}
			req.Children = observedChildren(t, desired, dep)
			got, err := Sync(req, config())
			if err != nil || condition(got, "Ready").Status != "True" || got.Status.InstalledVersion != req.Parent.Spec.Version {
				t.Fatalf("API CPU normalization blocked a healthy rollout: %v %+v", err, got.Status)
			}
			// Normalization must not hide a real change in requested resources.
			object(object(registry["resources"])["requests"])["cpu"] = "500m"
			req.Children = observedChildren(t, desired, dep)
			got, err = Sync(req, config())
			if err != nil || condition(got, "Ready").Status != "False" {
				t.Fatalf("resource drift must still prevent readiness: %v %+v", err, got.Status)
			}
		})
	}
}

func TestResourceEditsRollPodsOnlyWhenEffectiveValuesChange(t *testing.T) {
	defaults := Resources{
		Requests: map[string]string{"cpu": "250m", "memory": "256Mi"},
		Limits:   map[string]string{"cpu": "1", "memory": "1Gi"},
	}
	for _, tc := range []struct {
		name          string
		before, after Resources
		unchanged     bool
	}{
		{"explicit-request-default", Resources{}, Resources{Requests: map[string]string{"cpu": "250m"}}, true},
		{"explicit-defaults", Resources{}, defaults, true},
		{"remove-explicit-defaults", defaults, Resources{}, true},
		{"equivalent-defaults", Resources{}, Resources{
			Requests: map[string]string{"cpu": "0.250", "memory": "256Mi"},
			Limits:   map[string]string{"cpu": "1000m", "memory": "1024Mi"},
		}, true},
		{"equivalent-memory", Resources{
			Requests: map[string]string{"memory": "1Gi"}, Limits: map[string]string{"memory": "2Gi"},
		}, Resources{
			Requests: map[string]string{"memory": "1024Mi"}, Limits: map[string]string{"memory": "2048Mi"},
		}, true},
		{"equivalent-large-cpu", Resources{
			Requests: map[string]string{"cpu": "1000"}, Limits: map[string]string{"cpu": "1000"},
		}, Resources{
			Requests: map[string]string{"cpu": "1000000m"}, Limits: map[string]string{"cpu": "1000.000"},
		}, true},
		{"cpu-request-change", Resources{}, Resources{Requests: map[string]string{"cpu": "300m"}}, false},
		{"cpu-limit-change", Resources{}, Resources{Limits: map[string]string{"cpu": "2"}}, false},
		{"memory-request-change", Resources{}, Resources{Requests: map[string]string{"memory": "512Mi"}}, false},
		{"memory-limit-change", Resources{}, Resources{Limits: map[string]string{"memory": "2Gi"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := fixture()
			req.Parent.Spec.Resources = tc.before
			before, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			dep := asMap(t, find(t, before, "Deployment"))
			object(dep["metadata"])["generation"] = 1
			dep["status"] = Object{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1, "availableReplicas": 1}
			req.Children = observedChildren(t, before, dep)
			req.Parent.Metadata.Generation++
			req.Parent.Spec.Resources = tc.after
			observedBefore, _ := json.Marshal(req)
			after, err := Sync(req, config())
			if err != nil {
				t.Fatal(err)
			}
			observedAfter, _ := json.Marshal(req)
			if string(observedBefore) != string(observedAfter) {
				t.Fatal("resource normalization mutated the observed request")
			}
			if reflect.DeepEqual(find(t, before, "Deployment"), find(t, after, "Deployment")) != tc.unchanged {
				t.Fatalf("unexpected Deployment change: equivalent resources=%v", tc.unchanged)
			}
			ready := condition(after, "Ready")
			if (ready.Status == "True") != tc.unchanged || ready.ObservedGeneration != req.Parent.Metadata.Generation {
				t.Fatalf("readiness did not follow the effective resource change: %+v", ready)
			}
		})
	}
}
