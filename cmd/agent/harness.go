package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"orch/internal/model"
)

var (
	harness  = flag.String("harness", "builtin", "who does steps: builtin | claude")
	hTools   = flag.String("harness-tools", "Read Edit Write Glob Grep Bash(go:*) Bash(gofmt:*) Bash(ls:*) Bash(mkdir:*)", "tools allowed for the harness")
	hLocal   = flag.Bool("harness-local", true, "default profile: point the harness at Ollama instead of the Anthropic API")
	hTimeout = flag.Duration("harness-timeout", 60*time.Minute, "max time per attempt")
	check    = flag.String("check", "", "command run after each attempt, its real output goes to the reviewer (e.g. \"go test ./...\")")
)

const harnessPrompt = `Ты выполняешь ОДИН шаг большой задачи в папке проекта.
Исходный код проекта (не *_test.go) не меняй, только добавляй свои файлы и тесты.
Проверяй работу командами. В конце коротко напиши, что сделано и чем подтверждено.

`

func runHarness(ctx context.Context, s model.Step, feedback string) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, *hTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, "claude", "-p", harnessPrompt+workerPrompt(s, feedback),
		"--bare", "--model", prof.Worker.Model, "--permission-mode", "acceptEdits",
		"--allowedTools", *hTools, "--output-format", "stream-json", "--verbose")
	c.Dir = *workdir
	c.Env = os.Environ()
	if prof.HarnessURL != "" {
		key := "ollama"
		if k := os.Getenv(prof.HarnessKeyEnv); k != "" {
			key = k
		}
		c.Env = append(c.Env, "ANTHROPIC_BASE_URL="+prof.HarnessURL, "ANTHROPIC_AUTH_TOKEN="+key, "ANTHROPIC_API_KEY=", "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1")
	}
	out, err := c.StdoutPipe()
	if err != nil {
		return result{}, err
	}
	c.Stderr = c.Stdout
	t0 := time.Now()
	if err := c.Start(); err != nil {
		return result{}, err
	}
	var log strings.Builder
	res := result{}
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for sc.Scan() {
		var e struct {
			Type    string
			Result  string
			CostUSD float64 `json:"total_cost_usd"`
			Usage   struct {
				In     int `json:"input_tokens"`
				CacheW int `json:"cache_creation_input_tokens"`
				CacheR int `json:"cache_read_input_tokens"`
				Out    int `json:"output_tokens"`
			}
			Message struct {
				Content []struct {
					Type, Name, Text string
					Input            map[string]any
				}
				Usage struct {
					OutputTokens int `json:"output_tokens"`
				}
			}
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "assistant":
			for _, b := range e.Message.Content {
				if b.Type == "tool_use" {
					arg := fmt.Sprint(b.Input["command"], b.Input["file_path"], b.Input["pattern"])
					arg = strings.NewReplacer("<nil>", "", *workdir+"/", "").Replace(arg)
					emit(s.ID, "tool", "%s %s", b.Name, cut(arg, 150))
					fmt.Fprintf(&log, "- %s %s\n", b.Name, cut(arg, 300))
				}
			}
		case "result":
			res.Text = e.Result
			res.In, res.Tokens = e.Usage.In+e.Usage.CacheW+e.Usage.CacheR, e.Usage.Out
			// своя цена из профиля; без неё — цена, которую посчитал Claude Code (для API Anthropic)
			res.Cost = prof.Worker.cost(res.In, res.Tokens)
			if res.Cost == 0 && prof.HarnessURL == "" {
				res.Cost = e.CostUSD
			}
		}
	}
	err = c.Wait()
	if d := time.Since(t0).Seconds(); res.Tokens > 0 {
		res.TPS = float64(res.Tokens) / d
	}
	spend(res)
	if res.Text == "" && err != nil {
		return res, fmt.Errorf("claude: %v", err)
	}
	res.Text = "## Итог\n" + res.Text + "\n\n## Действия\n" + log.String()
	return res, nil
}

// реальная проверка после попытки: вывод и прошла ли команда
func runCheck() (string, bool) {
	if *check == "" || *workdir == "" {
		return "", true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	c := exec.CommandContext(ctx, "sh", "-c", *check)
	c.Dir = *workdir
	out, err := c.CombinedOutput()
	return fmt.Sprintf("\n\n## Проверка агентом: `%s` (%s)\n```\n%s\n```\n", *check, map[bool]string{true: "OK", false: "ОШИБКА"}[err == nil], cut(string(out), 4000)), err == nil
}
