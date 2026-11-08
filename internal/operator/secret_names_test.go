package operator

import (
	"encoding/json"
	"os"
	"slices"
	"testing"
)

func TestSecretReferenceNames(t *testing.T) {
	data, err := os.ReadFile("testdata/secret-name-validation.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string `json:"name"`
		Secret string `json:"secret"`
		Valid  bool   `json:"valid"`
	}
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"auth", "database", "tls"} {
		for _, tc := range cases {
			t.Run(field+"/"+tc.Name, func(t *testing.T) {
				req := rotationFixture(t)
				switch field {
				case "auth":
					req.Parent.Spec.AuthSecretName = tc.Secret
				case "database":
					req.Parent.Spec.Database.SecretName = tc.Secret
				case "tls":
					req.Parent.Spec.Exposure.TLSSecretName = tc.Secret
				}
				secret := req.Related["Secret.v1"][field]
				object(secret["metadata"])["name"] = tc.Secret
				delete(req.Related["Secret.v1"], field)
				req.Related["Secret.v1"][tc.Secret] = secret

				customized, customizeErr := Customize(req.Parent)
				got, syncErr := Sync(req, config())
				if (customizeErr == nil) != tc.Valid || (syncErr == nil) != tc.Valid {
					t.Fatalf("Secret validation disagrees with admission case: valid=%v, customize=%v, sync=%v", tc.Valid, customizeErr, syncErr)
				}
				if !tc.Valid {
					return
				}
				selected := false
				for _, rule := range customized.RelatedResources {
					if rule.Resource == "secrets" && rule.Namespace == req.Parent.Metadata.Namespace && slices.Contains(rule.Names, tc.Secret) {
						selected = true
					}
				}
				if !selected || condition(got, "DependenciesReady").Status != "True" {
					t.Fatal("valid Secret name was not selected and resolved in the parent namespace")
				}
				pod := asMap(t, asMap(t, asMap(t, find(t, got, "Deployment")["spec"])["template"])["spec"])
				mounted := false
				for _, raw := range pod["volumes"].([]any) {
					volume := object(raw)
					if volume["name"] == field && object(volume["secret"])["secretName"] == tc.Secret {
						mounted = true
					}
				}
				if !mounted {
					t.Fatal("pod volume did not preserve the referenced Secret name")
				}
			})
		}
	}
}
