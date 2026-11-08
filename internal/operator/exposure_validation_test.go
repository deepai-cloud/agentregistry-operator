package operator

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestExposureHostnameValidationCases(t *testing.T) {
	data, err := os.ReadFile("testdata/exposure-hostname-validation.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name     string `json:"name"`
		Hostname string `json:"hostname"`
		Valid    bool   `json:"valid"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, tc := range cases {
		t.Run(tc.Name, func(t *testing.T) {
			req := fixture()
			req.Parent.Spec.Exposure = &Exposure{Hostname: tc.Hostname, TLSSecretName: "tls"}
			got, err := Sync(req, config())
			if (err == nil) != tc.Valid {
				t.Fatalf("hostname validation disagrees with admission case: valid=%v, error=%v", tc.Valid, err)
			}
			if !tc.Valid {
				if !strings.Contains(err.Error(), "DNS hostname") {
					t.Fatalf("expected a hostname validation error: %v", err)
				}
				return
			}
			if got.Status.Endpoints["ui"] != "https://"+tc.Hostname+"/" {
				t.Fatalf("valid hostname was changed: %v", got.Status.Endpoints)
			}
			// A valid name does not replace the required TLS Secret. Keep the
			// application stopped until the certificate dependency is supplied.
			if condition(got, "DependenciesReady").Status != "False" ||
				asMap(t, find(t, got, "Deployment")["spec"])["replicas"] != float64(0) {
				t.Fatal("a valid hostname must not bypass the certificate dependency")
			}
		})
	}
}
