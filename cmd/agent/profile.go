package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
)

// Role — одна модель за OpenAI-совместимым API (Ollama /v1, vLLM, OpenRouter, ArliAI…)
type Role struct {
	URL    string         `json:"url"`     // до /chat/completions, например http://localhost:11434/v1
	KeyEnv string         `json:"key_env"` // имя переменной окружения с ключом
	Model  string         `json:"model"`
	In     float64        `json:"in"`    // $ за 1M входных токенов
	Out    float64        `json:"out"`   // $ за 1M выходных токенов
	JSON   string         `json:"json"`  // schema (по умолчанию) | object — если провайдер не умеет json_schema
	Extra  map[string]any `json:"extra"` // дополнительные поля запроса (temperature, reasoning_effort…)
}

func (r Role) cost(in, out int) float64 {
	return (float64(in)*r.In + float64(out)*r.Out) / 1e6
}

type Profile struct {
	Name     string `json:"name"`
	Harness  string `json:"harness"` // builtin | claude
	Planner  Role   `json:"planner"`
	Worker   Role   `json:"worker"`
	Reviewer Role   `json:"reviewer"`
	// для -harness claude: адрес в формате Anthropic API (у Ollama — корень без /v1); пусто — API Anthropic
	HarnessURL    string `json:"harness_url"`
	HarnessKeyEnv string `json:"harness_key_env"`
}

var (
	profilesPath = flag.String("profiles", "profiles.json", "file with profiles; if missing, a \"local\" profile is built from flags")
	profiles     []Profile
	prof         *Profile
)

func loadProfiles() {
	b, err := os.ReadFile(*profilesPath)
	if err == nil {
		if err := json.Unmarshal(b, &profiles); err != nil {
			fmt.Fprintln(os.Stderr, "profiles:", err)
			os.Exit(1)
		}
	}
	if len(profiles) == 0 {
		role := func(m string) Role {
			r := Role{URL: strings.TrimRight(*ollama, "/") + "/v1", Model: m}
			if !*think {
				r.Extra = map[string]any{"reasoning_effort": "none"}
			}
			return r
		}
		p := Profile{Name: "local", Harness: *harness, Planner: role(*planner), Worker: role(*worker), Reviewer: role(*reviewer)}
		if *hLocal {
			p.HarnessURL = *ollama
		}
		profiles = []Profile{p}
	}
	for i := range profiles {
		if profiles[i].Harness == "" {
			profiles[i].Harness = "builtin"
		}
	}
	prof = &profiles[0]
}

func pickProfile(name string) *Profile {
	for i := range profiles {
		if profiles[i].Name == name {
			return &profiles[i]
		}
	}
	return &profiles[0]
}

func profileNames() []string {
	var out []string
	for _, p := range profiles {
		out = append(out, p.Name)
	}
	return out
}

func (p *Profile) models() map[string]string {
	return map[string]string{"planner": p.Planner.Model, "worker": p.Worker.Model, "reviewer": p.Reviewer.Model, "harness": p.Harness}
}
