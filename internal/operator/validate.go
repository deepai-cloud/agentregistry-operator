package operator

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/deepai-cloud/agentregistry-operator/internal/certificates"
	"math"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var dnsLabel = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)
var serviceLabel = regexp.MustCompile(`^[a-z]([-a-z0-9]*[a-z0-9])?$`)
var cpuQuantity = regexp.MustCompile(`^([1-9][0-9]*m|[0-9]+(\.[0-9]{1,3})?)$`)
var memoryQuantity = regexp.MustCompile(`^[1-9][0-9]*(Mi|Gi)$`)

func localName(s string) bool { return len(s) > 0 && len(s) <= 63 && dnsLabel.MatchString(s) }
func secretName(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, p := range strings.Split(s, ".") {
		if !localName(p) {
			return false
		}
	}
	return true
}
func validate(p Parent, c Config) error {
	if p.APIVersion != "registry.deepai.cloud/v1alpha1" || p.Kind != "AgentRegistry" || !localName(p.Metadata.Name) || !serviceLabel.MatchString(p.Metadata.Name) || !localName(p.Metadata.Namespace) || p.Metadata.UID == "" || p.Metadata.Generation < 1 {
		return errors.New("invalid AgentRegistry identity; name must be a DNS label beginning with a letter, at most 63 characters")
	}
	if _, ok := images[p.Spec.Version]; !ok {
		return errors.New("version must be 0.3.2 or 0.3.3; 0.4 requires a fresh database")
	}
	if p.Status.InstalledVersion == "0.3.3" && p.Spec.Version == "0.3.2" {
		return errors.New("downgrade to 0.3.2 is unsupported; restore a compatible database into a new installation")
	}
	if !secretName(p.Spec.AuthSecretName) {
		return errors.New("Secret references must be namespace-local names")
	}
	if managedDatabase(p.Spec.Database) {
		if p.Spec.AuthSecretName == databaseName(p) || (p.Spec.Exposure != nil && p.Spec.Exposure.TLSSecretName == databaseName(p)) {
			return errors.New("auth and TLS Secrets must not use the managed database Secret name")
		}
		if storage := p.Spec.Database.Storage; storage != "" && (!memoryQuantity.MatchString(storage) || quantity(storage) > float64(1<<50)) {
			return errors.New("database storage must be a positive quantity in Mi or Gi, at most 1Pi")
		}
		if sc := p.Spec.Database.StorageClassName; sc != nil && *sc != "" && !secretName(*sc) {
			return errors.New("database storageClassName must be a valid name")
		}
	} else {
		if !secretName(p.Spec.Database.SecretName) || p.Spec.Database.Storage != "" || p.Spec.Database.StorageClassName != nil {
			return errors.New("external database requires host and secretName, without managed storage settings")
		}
		host := p.Spec.Database.Host
		if ip := net.ParseIP(host); ip != nil {
			if !ip.IsGlobalUnicast() || ip.IsLoopback() {
				return errors.New("database IP must be global unicast (private ranges allowed)")
			}
		} else {
			suffix := "." + p.Metadata.Namespace + ".svc.cluster.local"
			if !strings.HasSuffix(host, suffix) || !localName(strings.TrimSuffix(host, suffix)) {
				return errors.New("database host must be a namespace-local Service FQDN or a literal IP")
			}
		}
	}
	if p.Spec.Exposure != nil {
		e := p.Spec.Exposure
		if !secretName(e.TLSSecretName) || !secretName(e.Hostname) || !strings.Contains(e.Hostname, ".") || net.ParseIP(e.Hostname) != nil {
			return errors.New("TLS exposure requires a valid DNS hostname and namespace-local TLS Secret")
		}
	}
	if c.GatewayImage == "" {
		return errors.New("operator GATEWAY_IMAGE is required")
	}
	_, err := resources(p.Spec.Resources)
	return err
}
func resources(in Resources) (Resources, error) {
	r := Resources{Requests: map[string]string{"cpu": "250m", "memory": "256Mi"}, Limits: map[string]string{"cpu": "1", "memory": "1Gi"}}
	for _, pair := range []struct{ src, dst map[string]string }{{in.Requests, r.Requests}, {in.Limits, r.Limits}} {
		for k, v := range pair.src {
			if (k != "cpu" && k != "memory") || (k == "cpu" && !cpuQuantity.MatchString(v)) || (k == "memory" && !memoryQuantity.MatchString(v)) {
				return r, errors.New("resources accept only positive CPU quantities and memory in Mi or Gi")
			}
			pair.dst[k] = v
		}
	}
	for _, k := range []string{"cpu", "memory"} {
		if quantity(r.Requests[k]) <= 0 || quantity(r.Limits[k]) <= 0 || quantity(r.Requests[k]) > quantity(r.Limits[k]) || quantity(r.Limits[k]) > float64(1<<50) {
			return r, fmt.Errorf("%s request must be positive and no larger than its limit", k)
		}
	}
	for _, values := range []map[string]string{r.Requests, r.Limits} {
		milli := int64(math.Round(quantity(values["cpu"]) * 1000))
		if milli%1000 == 0 {
			values["cpu"] = strconv.FormatInt(milli/1000, 10)
		} else {
			values["cpu"] = strconv.FormatInt(milli, 10) + "m"
		}
		bytes := int64(quantity(values["memory"]))
		if bytes%(1<<30) == 0 {
			values["memory"] = strconv.FormatInt(bytes/(1<<30), 10) + "Gi"
		} else {
			values["memory"] = strconv.FormatInt(bytes/(1<<20), 10) + "Mi"
		}
	}
	return r, nil
}
func quantity(s string) float64 {
	mult := 1.0
	for _, u := range []struct {
		suffix string
		mult   float64
	}{{"Gi", 1073741824}, {"Mi", 1048576}, {"m", 0.001}} {
		if strings.HasSuffix(s, u.suffix) {
			s = strings.TrimSuffix(s, u.suffix)
			mult = u.mult
			break
		}
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return -1
	}
	return v * mult
}
func Customize(p Parent) (CustomizeResponse, error) {
	if !localName(p.Metadata.Namespace) || !secretName(p.Spec.AuthSecretName) || (!managedDatabase(p.Spec.Database) && !secretName(p.Spec.Database.SecretName)) {
		return CustomizeResponse{}, errors.New("invalid namespace-local Secret reference")
	}
	names := []string{p.Spec.AuthSecretName}
	if managedDatabase(p.Spec.Database) {
		if !localName(p.Metadata.Name) {
			return CustomizeResponse{}, errors.New("invalid managed database name")
		}
		names = append(names, databaseName(p))
	} else {
		names = append(names, p.Spec.Database.SecretName)
	}
	if p.Spec.Exposure != nil {
		if !secretName(p.Spec.Exposure.TLSSecretName) {
			return CustomizeResponse{}, errors.New("invalid TLS Secret reference")
		}
		names = append(names, p.Spec.Exposure.TLSSecretName)
	}
	sort.Strings(names)
	unique := names[:0]
	for _, name := range names {
		if len(unique) == 0 || unique[len(unique)-1] != name {
			unique = append(unique, name)
		}
	}
	rules := []RelatedRule{{APIVersion: "v1", Resource: "secrets", Namespace: p.Metadata.Namespace, Names: unique}}
	if managedDatabase(p.Spec.Database) {
		rules = append(rules, RelatedRule{APIVersion: "v1", Resource: "persistentvolumeclaims", Namespace: p.Metadata.Namespace, Names: []string{"data-" + databaseName(p) + "-0"}})
	}
	return CustomizeResponse{RelatedResources: rules}, nil
}
func relatedSecret(r Request, name string) Object {
	for _, group := range r.Related {
		for _, o := range group {
			m := object(o["metadata"])
			if o["kind"] == "Secret" && o["apiVersion"] == "v1" && m["name"] == name && m["namespace"] == r.Parent.Metadata.Namespace {
				return o
			}
		}
	}
	return nil
}
func secretData(o Object, key string) []byte {
	s, _ := object(o["data"])[key].(string)
	v, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil
	}
	return v
}
func dependencies(r Request) (bool, string, []string) {
	rule, _ := Customize(r.Parent)
	tokens := []string{}
	missing := []string{}
	for _, name := range rule.RelatedResources[0].Names {
		o := relatedSecret(r, name)
		if o == nil {
			missing = append(missing, name)
			tokens = append(tokens, name+":missing")
		} else {
			m := object(o["metadata"])
			tokens = append(tokens, fmt.Sprintf("%s:%v:%v", name, m["uid"], m["resourceVersion"]))
		}
	}
	if len(missing) > 0 {
		return false, "Create required Secrets in this namespace: " + strings.Join(missing, ", "), tokens
	}
	auth := relatedSecret(r, r.Parent.Spec.AuthSecretName)
	username, password, key := secretData(auth, "username"), secretData(auth, "password"), secretData(auth, "jwt-key")
	_, hexErr := hex.DecodeString(string(key))
	if len(username) == 0 || strings.ContainsAny(string(username), ":\r\n") || len(password) < 16 || len(key) != 64 || hexErr != nil {
		return false, "Auth Secret requires username, password (at least 16 bytes), and jwt-key (64 hex characters)", tokens
	}
	db := relatedSecret(r, r.Parent.Spec.Database.SecretName)
	u, err := url.Parse(string(secretData(db, "url")))
	if err != nil || u == nil || (u.Scheme != "postgres" && u.Scheme != "postgresql") || u.Hostname() != r.Parent.Spec.Database.Host || (u.Port() != "" && u.Port() != "5432") || u.User == nil || u.User.Username() == "" || u.Path == "" || u.Path == "/" || u.Fragment != "" {
		return false, "Database Secret url must name the configured host on port 5432, a database and dedicated credentials", tokens
	}
	if password, ok := u.User.Password(); !ok || password == "" {
		return false, "Database Secret url requires a dedicated password", tokens
	}
	q, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false, "Database Secret url has invalid query parameters", tokens
	}
	// libpq query keys can override URL connection fields or read arbitrary mounted files.
	for k, values := range q {
		if len(values) != 1 || (k != "sslmode" && k != "sslrootcert" && k != "connect_timeout") {
			return false, "Database URL query supports only sslmode, sslrootcert and connect_timeout", tokens
		}
	}
	mode := q.Get("sslmode")
	if mode != "disable" && mode != "require" && mode != "verify-full" {
		return false, "Database URL requires explicit sslmode=disable, require or verify-full", tokens
	}
	if net.ParseIP(r.Parent.Spec.Database.Host) != nil && mode != "verify-full" {
		return false, "Remote database IP requires sslmode=verify-full and a certificate with its IP SAN", tokens
	}
	if root := q.Get("sslrootcert"); root != "" && root != "/database/ca.crt" {
		return false, "sslrootcert must be /database/ca.crt", tokens
	}
	if mode == "verify-full" {
		pool := x509.NewCertPool()
		if q.Get("sslrootcert") != "/database/ca.crt" || !pool.AppendCertsFromPEM(secretData(db, "ca.crt")) {
			return false, "verify-full requires ca.crt in the database Secret and sslrootcert=/database/ca.crt", tokens
		}
	}
	if e := r.Parent.Spec.Exposure; e != nil {
		o := relatedSecret(r, e.TLSSecretName)
		cert, err := tls.X509KeyPair(secretData(o, "tls.crt"), secretData(o, "tls.key"))
		if err != nil {
			return false, "TLS Secret requires a matching tls.crt and tls.key", tokens
		}
		leaf, err := x509.ParseCertificate(cert.Certificate[0])
		if err != nil || leaf.VerifyHostname(e.Hostname) != nil {
			return false, "TLS certificate must cover the exposure hostname", tokens
		}
		if err := certificates.ValidateLeaf(leaf); err != nil {
			return false, err.Error(), tokens
		}
	}
	return true, "All namespace-local dependencies are valid", tokens
}
