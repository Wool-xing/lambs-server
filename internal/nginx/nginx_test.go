package nginx

import (
	"strings"
	"testing"

	"lambs-server-go/internal/models"
)

func TestBuildConfig(t *testing.T) {
	// Deployment-specific gate address comes from env — tests inject their own.
	oldGate := gateHost
	gateHost = "10.0.0.9:3602"
	defer func() { gateHost = oldGate }()

	projects := []models.Project{
		{Name: "QA质量", BasePath: "/QA_Test", Port: "3501", BackendURL: "http://127.0.0.1:3509"},
		{Name: "无端口项目", BasePath: "/NoPort", Port: ""},
	}
	conf := buildConfig(projects)

	mustContain := []string{
		"location = /lambs-offline",
		"location = /QA_Test/favicon.svg",
		"location = /lambs-gate-QA_Test",
		"location /QA_Test/api/",
		// Web1 must reach App1's TCPProxy, never the App1-local backend
		"proxy_pass http://10.0.0.9:3501/",
		"location /QA_Test {",
	}
	for _, s := range mustContain {
		if !strings.Contains(conf, s) {
			t.Errorf("buildConfig missing %q", s)
		}
	}
	// App1-local backend must NOT leak into the generated config
	if strings.Contains(conf, "127.0.0.1:3509") {
		t.Error("buildConfig leaked App1-local backend 127.0.0.1:3509")
	}
	// Portless project: no API location generated
	if strings.Contains(conf, "location /NoPort/api/") {
		t.Error("portless project should not get an API location")
	}
}

func TestBuildConfigEmpty(t *testing.T) {
	conf := buildConfig(nil)
	if !strings.Contains(conf, "lambs-offline") {
		t.Error("offline handler location missing for empty project list")
	}
}

// TestBuildConfigInjection — hostile base_path/name values must never reach
// the managed vhost as raw directives (QA round 3 idea 4).
func TestBuildConfigInjection(t *testing.T) {
	oldGate := gateHost
	gateHost = "10.0.0.9:3602"
	defer func() { gateHost = oldGate }()

	projects := []models.Project{
		// Semicolon/brace = directive injection — rejected by the charset gate.
		{Name: "evil; return 200;", BasePath: "/safe", Port: "3501"},
		{Name: "safe", BasePath: "/evil;} location /owned {", Port: "3502"},
		// Newline in name must be neutralized, not start a new directive line.
		{Name: "line\nbreak", BasePath: "/ok", Port: "3503"},
		// BackendURL without scheme gets http:// prefixed.
		{Name: "noscheme", BasePath: "/ns", Port: "", BackendURL: "127.0.0.1:9000"},
	}
	conf := buildConfig(projects)

	// Injected project skipped entirely — no location with its port.
	if strings.Contains(conf, "3502") {
		t.Error("injected base_path reached the config")
	}
	if strings.Contains(conf, "location /owned") {
		t.Error("directive injection reached the config")
	}
	// The hostile name survives only inside its comment line (# ...), which
	// is inert in nginx config syntax.
	for _, line := range strings.Split(conf, "\n") {
		if strings.Contains(line, "return 200") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("hostile name leaked outside a comment line: %q", line)
		}
	}
	// Name newline neutralized: no raw newline before its port line appears
	// inside a location block. Simple probe: the literal "line\nbreak" must
	// not survive (config lines would break).
	if strings.Contains(conf, "line\nbreak") {
		t.Error("newline in project name survived into config")
	}
	if !strings.Contains(conf, "line break") {
		t.Error("newline in name was not replaced with space")
	}
	// BackendURL scheme normalized.
	if !strings.Contains(conf, "http://127.0.0.1:9000") {
		t.Error("scheme-less backend_url not normalized")
	}
}
