package kitexx

import (
	"testing"

	"github.com/sezznaw/devkit-common/mysqlx"
)

// source service: own database and account from tenant + domain, password
// from the service's variable, address from infra.yaml; what is missing is
// named (the runtime then falls back to the shared tenant database).
func TestServiceMySQL(t *testing.T) {
	cfg := Config{}
	cfg.Service.Name = "ser-geo"
	cfg.MySQL = mysqlx.Config{Enabled: true, Source: mysqlx.SourceService}
	doc := infraDoc{MySQL: map[string]infraMySQL{"tenant_a": {Addr: "mysql.sportsbook-dev-infra:3306", Params: "a=1"}}}
	env := map[string]string{"MYSQL_SERVICE_PASSWORD": "pw"}
	getenv := func(k string) string { return env[k] }

	res, missing, err := serviceMySQL(cfg, "tenant_a", doc, getenv)
	if err != nil || missing != "" {
		t.Fatalf("%v %q", err, missing)
	}
	if res.rule != mysqlRuleService || res.domain != "geo" || res.target.DB != "tenant_a_geo" || res.target.User != "tenant_a_geo" ||
		res.target.Password != "pw" || res.target.Addr != "mysql.sportsbook-dev-infra:3306" || res.target.Params != "a=1" {
		t.Fatalf("%+v", res)
	}
	// no password yet: falls back, naming the variable
	delete(env, "MYSQL_SERVICE_PASSWORD")
	if _, missing, err := serviceMySQL(cfg, "tenant_a", doc, getenv); err != nil || missing == "" || missing[:20] != "environment variable" {
		t.Fatalf("%v %q", err, missing)
	}
	// password but no address for the tenant in infra.yaml
	env["MYSQL_SERVICE_PASSWORD"] = "pw"
	if _, missing, err := serviceMySQL(cfg, "tenant_a", infraDoc{}, getenv); err != nil || missing == "" || missing[:10] != "infra.yaml" {
		t.Fatalf("%v %q", err, missing)
	}
	// a name that can never work is an error, not a fallback
	if _, _, err := serviceMySQL(cfg, "", doc, getenv); err == nil {
		t.Fatal("empty tenant accepted")
	}
	// a custom domain and password variable
	cfg.MySQL.Domain, cfg.MySQL.PasswordEnv = "geoip", "GEO_DB_PASSWORD"
	env["GEO_DB_PASSWORD"] = "other"
	res, missing, _ = serviceMySQL(cfg, "tenant_a", doc, getenv)
	if missing != "" || res.target.DB != "tenant_a_geoip" || res.target.Password != "other" {
		t.Fatalf("%+v %q", res, missing)
	}
}
