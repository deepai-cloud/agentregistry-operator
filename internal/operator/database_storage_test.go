package operator

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestManagedDatabaseStorageValidationCases(t *testing.T) {
	data, err := os.ReadFile("testdata/database-storage-validation.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name       string   `json:"name"`
		Database   Database `json:"database"`
		Valid      bool     `json:"valid"`
		Normalized string   `json:"normalized"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			req := managedFixture()
			req.Parent.Spec.Database = tc.Database
			if _, err := Sync(req, config()); (err == nil) != tc.Valid {
				t.Fatalf("storage validation disagrees with admission case: valid=%v, error=%v", tc.Valid, err)
			}
			if !tc.Valid {
				return
			}
			got := provisionManagedDatabase(t, req)
			sts := databaseChild(t, got, "StatefulSet", databaseName(req.Parent))
			claims := object(sts["spec"])["volumeClaimTemplates"].([]any)
			claim := object(object(claims[0])["spec"])
			storage := object(object(claim["resources"])["requests"])["storage"]
			if storage != tc.Normalized {
				t.Fatalf("PVC size changed or was not canonicalized: got %v, want %s", storage, tc.Normalized)
			}
			class, present := claim["storageClassName"]
			if expected := tc.Database.StorageClassName; expected == nil {
				if present {
					t.Fatal("omitted storageClassName must allow the cluster default")
				}
			} else if !present || class != *expected {
				t.Fatalf("explicit storageClassName must be preserved, including empty: got %v, want %q", class, *expected)
			}
		})
	}
}

func TestManagedDatabaseRejectsOverflowingStorage(t *testing.T) {
	// Defense for legacy CRDs and direct webhook requests: ParseFloat previously
	// returned a negative sentinel on overflow, bypassing the upper-bound check.
	req := managedFixture()
	req.Parent.Spec.Database.Storage = strings.Repeat("9", 400) + "Gi"
	got, err := Sync(req, config())
	if err == nil || len(got.Children) != 0 {
		t.Fatal("overflowing storage must be rejected before provisioning credentials or a PVC")
	}
}
