package nginx

import (
	"strings"
	"testing"

	"lambs-server-go/internal/models"
)

func TestBuildConfig(t *testing.T) {
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
		"proxy_pass http://100.92.91.11:3501/",
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
