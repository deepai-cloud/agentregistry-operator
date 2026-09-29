// Package operator generates desired Kubernetes objects without Kubernetes API clients.
package operator

import (
	"bytes"
	"encoding/json"
)

type Object = map[string]any

type Metadata struct {
	Name       string `json:"name"`
	Namespace  string `json:"namespace"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}
type Database struct {
	SecretName       string  `json:"secretName,omitempty"`
	Host             string  `json:"host,omitempty"`
	Storage          string  `json:"storage,omitempty"`
	StorageClassName *string `json:"storageClassName,omitempty"`
}
type Resources struct {
	Requests map[string]string `json:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty"`
}
type Exposure struct {
	Hostname      string `json:"hostname"`
	TLSSecretName string `json:"tlsSecretName"`
}
type Spec struct {
	Version        string    `json:"version"`
	Database       Database  `json:"database,omitempty"`
	AuthSecretName string    `json:"authSecretName"`
	Resources      Resources `json:"resources,omitempty"`
	Exposure       *Exposure `json:"exposure,omitempty"`
}

// Reject unknown tenant-controlled settings while permitting Kubernetes metadata.
func (s *Spec) UnmarshalJSON(b []byte) error {
	type plain Spec
	var v plain
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if err := d.Decode(&v); err != nil {
		return err
	}
	*s = Spec(v)
	return nil
}

type Condition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason"`
	Message            string `json:"message"`
	ObservedGeneration int64  `json:"observedGeneration"`
}
type Status struct {
	ObservedGeneration int64             `json:"observedGeneration"`
	InstalledVersion   string            `json:"installedVersion"`
	Endpoints          map[string]string `json:"endpoints"`
	Conditions         []Condition       `json:"conditions"`
}
type Parent struct {
	APIVersion string   `json:"apiVersion"`
	Kind       string   `json:"kind"`
	Metadata   Metadata `json:"metadata"`
	Spec       Spec     `json:"spec"`
	Status     Status   `json:"status,omitempty"`
}
type Request struct {
	Parent   Parent                       `json:"parent"`
	Children map[string]map[string]Object `json:"children"`
	Related  map[string]map[string]Object `json:"related"`
}
type Response struct {
	Status             Status   `json:"status"`
	Children           []Object `json:"children"`
	ResyncAfterSeconds int      `json:"resyncAfterSeconds"`
}
type RelatedRule struct {
	APIVersion string   `json:"apiVersion"`
	Resource   string   `json:"resource"`
	Namespace  string   `json:"namespace"`
	Names      []string `json:"names"`
}
type CustomizeResponse struct {
	RelatedResources []RelatedRule `json:"relatedResources"`
}
type Config struct{ GatewayImage string }
