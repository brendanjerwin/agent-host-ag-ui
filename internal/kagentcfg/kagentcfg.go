// Package kagentcfg materializes kagent's compiled agent configuration
// (KAGENT_CONFIG_JSON, injected by the byo Harness) into the omp child's
// agent directory: models.yml (provider + model + credential) and config.yml
// (default model role). With no KAGENT_CONFIG_JSON (rung-3 plain deployment),
// it is a no-op and the child falls back to its own mounted config.
package kagentcfg

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Config is the subset of kagent's adk.AgentConfig the adapter consumes.
type Config struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Instruction string `json:"instruction"`
	Model       Model  `json:"model"`
}

// Model is the compiled model block. Credentials arrive as
// __KAGENT_ENV[NAME]__ placeholders (the egress gateway swaps the real key
// into the request header, host-scoped to the base_url).
type Model struct {
	Type    string `json:"type"`
	Model   string `json:"model"`
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
}

var placeholderRe = regexp.MustCompile(`__KAGENT_ENV\[([A-Za-z0-9_]+)\]__`)

// ExpandEnv resolves kagent credential placeholders from the process env
// (mirror of kagent's own MaterializeFromEnv). Unresolvable placeholders stay
// verbatim so the failure is visible at the model call instead of silent.
func ExpandEnv(s string) string {
	return placeholderRe.ReplaceAllStringFunc(s, func(m string) string {
		name := placeholderRe.FindStringSubmatch(m)[1]
		if v, ok := os.LookupEnv(name); ok && v != "" {
			return v
		}
		return m
	})
}

// Parse reads and materializes the compiled config. Returns nil,false when
// KAGENT_CONFIG_JSON is absent (not a kagent-managed run).
func Parse() (*Config, bool, error) {
	raw := os.Getenv("KAGENT_CONFIG_JSON")
	if strings.TrimSpace(raw) == "" {
		return nil, false, nil
	}
	raw = ExpandEnv(raw)
	var cfg Config
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return nil, true, fmt.Errorf("kagentcfg: parse KAGENT_CONFIG_JSON: %w", err)
	}
	if cfg.Model.Model == "" {
		return nil, true, fmt.Errorf("kagentcfg: compiled config has no model")
	}
	return &cfg, true, nil
}

// Materialize writes the omp child's models.yml + config.yml into agentDir.
func Materialize(agentDir string) error {
	cfg, ok, err := Parse()
	if err != nil || !ok {
		return err
	}
	if err := os.MkdirAll(agentDir, 0o755); err != nil {
		return fmt.Errorf("kagentcfg: agent dir: %w", err)
	}

	providerName := "kagent"
	modelID := cfg.Model.Model

	// omp models.yml: apiKey as a LITERAL (already expanded above). The
	// substrate egress gateway swaps the Bearer header host-scoped, so the
	// placeholder value is correct there; a resolvable env name would also
	// work but the literal keeps both paths identical.
	modelsYml := fmt.Sprintf(`providers:
  %s:
    baseUrl: %s
    api: openai-completions
    apiKey: "%s"
    models:
      - id: %s
        name: %s
        contextWindow: 400000
        maxTokens: 32768
`, providerName, cfg.Model.BaseURL, cfg.Model.APIKey, modelID, modelID)
	if err := os.WriteFile(filepath.Join(agentDir, "models.yml"), []byte(modelsYml), 0o600); err != nil {
		return fmt.Errorf("kagentcfg: models.yml: %w", err)
	}

	// Default role: omp auto-selects this at child startup.
	configYml := fmt.Sprintf(`modelRoles:
  default: %s/%s
`, providerName, modelID)
	if err := os.WriteFile(filepath.Join(agentDir, "config.yml"), []byte(configYml), 0o600); err != nil {
		return fmt.Errorf("kagentcfg: config.yml: %w", err)
	}
	return nil
}
