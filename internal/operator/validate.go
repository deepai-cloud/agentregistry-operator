package operator

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/deepai-cloud/agentregistry-operator/internal/certificates"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
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
		if _, err := databaseStorage(p.Spec.Database.Storage); err != nil {
			return err
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
			if len(v) > 32 {
				return r, errors.New("resource quantities must be at most 32 characters")
			}
			if (k != "cpu" && k != "memory") || (k == "cpu" && !cpuQuantity.MatchString(v)) || (k == "memory" && !memoryQuantity.MatchString(v)) {
				return r, errors.New("resources accept only positive CPU quantities and memory in Mi or Gi")
			}
			pair.dst[k] = v
		}
	}
	for _, k := range []string{"cpu", "memory"} {
		request, requestErr := resourceUnits(k, r.Requests[k])
		limit, limitErr := resourceUnits(k, r.Limits[k])
		if requestErr != nil || limitErr != nil || request > limit {
			return r, fmt.Errorf("%s request must be positive and no larger than its limit", k)
		}
		for _, value := range []struct {
			fields map[string]string
			units  int64
		}{{r.Requests, request}, {r.Limits, limit}} {
			if k == "cpu" {
				value.fields[k] = canonicalCPU(value.units)
			} else {
				value.fields[k] = canonicalMemory(value.units)
			}
		}
	}
	return r, nil
}

// Match Kubernetes' largest whole-number DecimalSI unit, including k through P.
// Callers supply positive millicores bounded by 1Pi cores.
func canonicalCPU(millicores int64) string {
	suffix := "m"
	for _, next := range []string{"", "k", "M", "G", "T", "P"} {
		if millicores%1000 != 0 {
			break
		}
		millicores /= 1000
		suffix = next
	}
	return strconv.FormatInt(millicores, 10) + suffix
}

// Kubernetes chooses the largest binary unit with a whole-number mantissa.
// Use the same representation so API persistence does not cause endless drift.
// Callers supply validated Mi/Gi quantities bounded by 1Pi.
func canonicalMemory(bytes int64) string {
	for _, unit := range []struct {
		suffix string
		size   int64
	}{{"Pi", 1 << 50}, {"Ti", 1 << 40}, {"Gi", 1 << 30}} {
		if bytes%unit.size == 0 {
			return strconv.FormatInt(bytes/unit.size, 10) + unit.suffix
		}
	}
	return strconv.FormatInt(bytes/(1<<20), 10) + "Mi"
}

// Parse syntax-checked quantities exactly as millicores or bytes. Floating point
// multiplication can incorrectly order equivalent values such as 9m and 0.009.
func resourceUnits(name, value string) (int64, error) {
	maximum, multiplier := int64(1<<50), int64(1)
	if name == "cpu" {
		maximum *= 1000
		if strings.HasSuffix(value, "m") {
			value = strings.TrimSuffix(value, "m")
		} else {
			whole, fraction, _ := strings.Cut(value, ".")
			value = whole + fraction + strings.Repeat("0", 3-len(fraction))
		}
	} else {
		multiplier = 1 << 20
		if strings.HasSuffix(value, "Gi") {
			multiplier = 1 << 30
		}
		value = value[:len(value)-2]
	}
	units, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, err
	}
	if units <= 0 || units > maximum/multiplier {
		return 0, errors.New("resource quantity outside supported bounds")
	}
	return units * multiplier, nil
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
		if legacy := legacyDatabaseName(p); legacy != databaseName(p) {
			names = append(names, legacy)
		}
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
		claims := []string{"data-" + databaseName(p) + "-0"}
		if legacy := legacyDatabaseName(p); legacy != databaseName(p) {
			claims = append(claims, "data-"+legacy+"-0")
		}
		sort.Strings(claims)
		rules = append(rules, RelatedRule{APIVersion: "v1", Resource: "persistentvolumeclaims", Namespace: p.Metadata.Namespace, Names: claims})
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
	unavailable := []string{}
	for _, name := range rule.RelatedResources[0].Names {
		o := relatedSecret(r, name)
		if o == nil || object(o["metadata"])["deletionTimestamp"] != nil {
			state := "missing"
			if o != nil {
				state = "terminating"
			}
			unavailable = append(unavailable, name+" ("+state+")")
			tokens = append(tokens, name+":missing")
		} else {
			// Only data changes affect credentials and mounted files. Metadata
			// updates from secret managers must not trigger Recreate downtime.
			tokens = append(tokens, name+":"+hash(o["data"]))
		}
	}
	if len(unavailable) > 0 {
		return false, "Provide required Secrets in this namespace: " + strings.Join(unavailable, ", "), tokens
	}
	auth := relatedSecret(r, r.Parent.Spec.AuthSecretName)
	username, password, key := secretData(auth, "username"), secretData(auth, "password"), secretData(auth, "jwt-key")
	_, hexErr := hex.DecodeString(string(key))
	if len(username) == 0 || strings.ContainsAny(string(username), ":\r\n") || len(password) < 16 || len(key) != 64 || hexErr != nil {
		return false, "Auth Secret requires username, password (at least 16 bytes), and jwt-key (64 hex characters)", tokens
	}
	db := relatedSecret(r, r.Parent.Spec.Database.SecretName)
	u := databaseSecretURL(db)
	if u == nil {
		return false, "Database Secret url must be a lowercase postgres:// or postgresql:// URI with credentials and no fragment", tokens
	}
	if u.Hostname() != r.Parent.Spec.Database.Host || (u.Port() != "" && u.Port() != "5432") || u.User.Username() == "" {
		return false, "Database Secret url must name the configured host on port 5432, a database and dedicated credentials", tokens
	}
	// pgx removes all leading slashes from the decoded path. A slash-only path
	// otherwise selects a default database instead of one explicitly named here.
	if strings.TrimLeft(u.Path, "/") == "" {
		return false, "Database Secret url requires a nonempty database name after leading slashes", tokens
	}
	databasePassword, hasPassword := u.User.Password()
	if !hasPassword || databasePassword == "" {
		return false, "Database Secret url requires a dedicated password", tokens
	}
	// PostgreSQL protocol strings use NUL terminators. Check decoded values:
	// pgx accepts encoded NULs, which can alter startup fields or passwords.
	if strings.ContainsRune(u.User.Username(), '\x00') || strings.ContainsRune(databasePassword, '\x00') || strings.ContainsRune(u.Path, '\x00') {
		return false, "Database Secret url username, password and database must not contain NUL bytes", tokens
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
	if values, present := q["connect_timeout"]; present {
		// The pinned pgx driver parses signed integer seconds, then multiplies
		// by time.Second. Reject invalid values and duration overflow up front.
		const maxSeconds = (1<<63 - 1) / int64(time.Second)
		seconds, err := strconv.ParseInt(values[0], 10, 64)
		if err != nil || seconds < 0 || seconds > maxSeconds {
			return false, fmt.Sprintf("Database URL connect_timeout must be a nonnegative whole number of seconds no greater than %d", maxSeconds), tokens
		}
	}
	mode := q.Get("sslmode")
	if mode != "disable" && mode != "require" && mode != "verify-full" {
		return false, "Database URL requires explicit sslmode=disable, require or verify-full", tokens
	}
	if net.ParseIP(r.Parent.Spec.Database.Host) != nil && mode != "verify-full" {
		return false, "Remote database IP requires sslmode=verify-full and a certificate with its IP SAN", tokens
	}
	root := q.Get("sslrootcert")
	if root != "" && root != "/database/ca.crt" {
		return false, "sslrootcert must be /database/ca.crt", tokens
	}
	if mode == "verify-full" && root == "" {
		return false, "verify-full requires ca.crt in the database Secret and sslrootcert=/database/ca.crt", tokens
	}
	if root != "" {
		// pgx loads an explicitly named root file before checking sslmode,
		// including require and disable. It must exist and contain certificates.
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(secretData(db, "ca.crt")) {
			return false, "sslrootcert requires parseable PEM certificates in the database Secret ca.crt", tokens
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
