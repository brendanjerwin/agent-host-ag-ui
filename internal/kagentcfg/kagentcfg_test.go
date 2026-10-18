package kagentcfg

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMaterializeFromKagentConfig(t *testing.T) {
	t.Setenv("KAGENT_CONFIG_JSON", `{"name":"dot","instruction":"be brief","model":{"type":"openai","model":"glm-5.3-flash","base_url":"https://api.ollama.com/v1","api_key":"__KAGENT_ENV[KAGENT_CREDENTIAL_TESTKEY]__"}}`)
	t.Setenv("KAGENT_CREDENTIAL_TESTKEY", "kagent-credential-injected")

	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	models, err := os.ReadFile(filepath.Join(dir, "models.yml"))
	if err != nil {
		t.Fatal(err)
	}
	s := string(models)
	for _, want := range []string{"baseUrl: https://api.ollama.com/v1", "id: glm-5.3-flash", `apiKey: "kagent-credential-injected"`} {
		if !strings.Contains(s, want) {
			t.Errorf("models.yml missing %q in:\n%s", want, s)
		}
	}
	cfg, err := os.ReadFile(filepath.Join(dir, "config.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cfg), "default: kagent/glm-5.3-flash") {
		t.Errorf("config.yml role missing:\n%s", cfg)
	}
}

func TestNoopWithoutEnv(t *testing.T) {
	t.Setenv("KAGENT_CONFIG_JSON", "")
	dir := t.TempDir()
	if err := Materialize(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "models.yml")); !os.IsNotExist(err) {
		t.Errorf("models.yml written without kagent config")
	}
}
