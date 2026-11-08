package operator

import (
	"encoding/base64"
	"net"
	"net/url"
	"reflect"
	"testing"
)

func TestRemoteDatabaseRequiresVerifiedTLSAndExactEgress(t *testing.T) {
	ca := rotationCertificate(t, true)["tls.crt"]
	for _, host := range []string{
		"192.0.2.10", "10.20.30.40", "2001:db8::10", "fd00::10",
		"2001:0DB8:0000:0000:0000:0000:0000:0010", "::ffff:192.0.2.10",
	} {
		for _, mode := range []string{"disable", "require", "verify-full"} {
			t.Run(host+"/"+mode, func(t *testing.T) {
				req := fixture()
				req.Parent.Spec.Database.Host = host
				db := object(req.Related["Secret.v1"]["database"]["data"])
				connection := url.URL{Scheme: "postgres", User: url.UserPassword("alice", "remote-test-password"),
					Host: net.JoinHostPort(host, "5432"), Path: "/catalog",
					RawQuery: url.Values{"sslmode": {mode}, "sslrootcert": {"/database/ca.crt"}}.Encode()}
				db["url"] = base64.StdEncoding.EncodeToString([]byte(connection.String()))
				db["ca.crt"] = ca
				got, err := Sync(req, config())
				if err != nil {
					t.Fatal(err)
				}
				wantStatus, wantReplicas := "False", float64(0)
				if mode == "verify-full" {
					wantStatus, wantReplicas = "True", 1
				}
				if condition(got, "DependenciesReady").Status != wantStatus ||
					number(object(find(t, got, "Deployment")["spec"])["replicas"]) != wantReplicas {
					t.Fatalf("remote database must require identity-verifying TLS, even when a CA is supplied: %+v", got.Status)
				}
				if len(got.Children) != 4 {
					t.Fatal("remote TLS validation must preserve application children without provisioning a managed database")
				}
				policy := asMap(t, find(t, got, "NetworkPolicy")["spec"])
				databaseRules := 0
				for _, raw := range policy["egress"].([]any) {
					rule := object(raw)
					for _, target := range rule["to"].([]any) {
						block, ok := object(target)["ipBlock"]
						if !ok {
							continue
						}
						databaseRules++
						cidr, _ := object(block)["cidr"].(string)
						ip, network, err := net.ParseCIDR(cidr)
						if err != nil {
							t.Fatalf("invalid remote database CIDR: %q", cidr)
						}
						prefix, bits := network.Mask.Size()
						if prefix != bits || !ip.Equal(net.ParseIP(host)) || len(object(block)) != 1 {
							t.Fatalf("database egress must select exactly the configured IP: %v", block)
						}
						if len(rule["to"].([]any)) != 1 || !reflect.DeepEqual(rule["ports"], []any{Object{"protocol": "TCP", "port": float64(5432)}}) {
							t.Fatalf("remote database rule must allow only TCP 5432 to its one address: %v", rule)
						}
					}
				}
				if databaseRules != 1 {
					t.Fatal("remote database must have exactly one IP-based egress rule")
				}
			})
		}
	}
}

func TestRemoteDatabaseRejectsNonUnicastAndLocalOnlyAddresses(t *testing.T) {
	for _, host := range []string{
		"0.0.0.0", "127.0.0.1", "127.42.0.1", "169.254.10.20", "224.0.0.1", "255.255.255.255",
		"::", "::1", "fe80::1", "ff02::1", "::ffff:127.0.0.1", "::ffff:169.254.10.20",
	} {
		t.Run(host, func(t *testing.T) {
			req := fixture()
			req.Parent.Spec.Database.Host = host
			got, err := Sync(req, config())
			if err == nil || len(got.Children) != 0 {
				t.Fatal("unsupported database address must fail before producing workload or network changes")
			}
		})
	}
}
