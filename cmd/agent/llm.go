package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"orch/internal/model"
)

type result struct {
	Text   string
	Tokens int
	TPS    float64
}

var noThink = map[string]bool{}

type msg = map[string]string

func chat(ctx context.Context, mdl, system, user string, schema any) (result, error) {
	return chatMsgs(ctx, mdl, []msg{{"role": "system", "content": system}, {"role": "user", "content": user}}, schema)
}

func chatMsgs(ctx context.Context, mdl string, msgs []msg, schema any) (result, error) {
	body := map[string]any{
		"model":    mdl,
		"stream":   false,
		"messages": msgs,
		"options":  map[string]any{"num_ctx": *numCtx, "num_predict": predict(schema)},
	}
	if schema != nil {
		body["format"] = schema
	}
	if !noThink[mdl] {
		body["think"] = *think
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Minute)
	defer cancel()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequestWithContext(ctx, "POST", *ollama+"/api/chat", bytes.NewReader(b))
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return result{}, err
	}
	defer res.Body.Close()
	var out struct {
		Message      struct{ Content string }
		EvalCount    int   `json:"eval_count"`
		EvalDuration int64 `json:"eval_duration"`
		Error        string
	}
	json.NewDecoder(res.Body).Decode(&out)
	if out.Error != "" {
		if strings.Contains(out.Error, "think") && !noThink[mdl] {
			noThink[mdl] = true
			return chatMsgs(ctx, mdl, msgs, schema)
		}
		return result{}, fmt.Errorf("%s", out.Error)
	}
	r := result{Text: clean(out.Message.Content, schema != nil), Tokens: out.EvalCount}
	if out.EvalDuration > 0 {
		r.TPS = float64(out.EvalCount) / (float64(out.EvalDuration) / 1e9)
		tmu.Lock()
		lastTPS = r.TPS
		tmu.Unlock()
	}
	return r, nil
}

func chatJSON(ctx context.Context, mdl, system, user string, schema, out any) error {
	var last error
	for range 3 {
		r, err := chat(ctx, mdl, system, user, schema)
		if err != nil {
			return err
		}
		if last = json.Unmarshal([]byte(r.Text), out); last == nil {
			return nil
		}
	}
	return last
}

func obj(props map[string]any, req ...string) map[string]any {
	return map[string]any{"type": "object", "properties": props, "required": req}
}

var (
	str      = map[string]any{"type": "string"}
	num      = map[string]any{"type": "integer"}
	stepList = map[string]any{"type": "array", "items": obj(map[string]any{"title": str, "criteria": str, "weight": num}, "title", "criteria", "weight")}
)

const workerSystem = "Ты исполнитель. Выполни ровно ОДИН шаг большой задачи. Выдай сам результат полностью, без вступлений и без пересказа задания. Отвечай на языке задачи."

func plan(ctx context.Context, goal string) ([]model.Step, error) {
	var out struct{ Steps []model.Step }
	err := chatJSON(ctx, *planner,
		"Ты планировщик. Разбей большую задачу на 3–12 небольших последовательных шагов. Каждый шаг должен выполняться одним ответом языковой модели. Для каждого шага дай чёткий проверяемый критерий готовности и вес сложности 1–5. Пиши на языке задачи.",
		"ЗАДАЧА:\n"+goal, obj(map[string]any{"steps": stepList}, "steps"), &out)
	if err != nil || len(out.Steps) == 0 {
		return nil, fmt.Errorf("пустой план: %v", err)
	}
	return newSteps(out.Steps, 0), nil
}

func newSteps(in []model.Step, offset int) []model.Step {
	for i := range in {
		in[i].ID = fmt.Sprintf("s%d", offset+i+1)
		in[i].Status = "pending"
		in[i].Attempts = []model.Attempt{}
		in[i].Weight = min(max(in[i].Weight, 1), 5)
	}
	return in
}

func cut(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

// JSON-ответы короткие, текст и код длиннее; без лимита модель может зациклиться
func predict(schema any) int {
	n := 6000
	if schema != nil {
		n = 1500
	}
	if *think {
		n += 6000 // размышления тоже считаются в лимит
	}
	return n
}

func squeeze(s string) string {
	r := []rune(s)
	if len(r) <= 9000 {
		return s
	}
	return string(r[:2500]) + "\n…\n" + string(r[len(r)-6500:])
}

func overview() string {
	tmu.Lock()
	defer tmu.Unlock()
	var b strings.Builder
	for i, s := range cur.Steps {
		fmt.Fprintf(&b, "%d. [%s] %s\n", i+1, s.Status, s.Title)
	}
	return b.String()
}

func doneOutputs(limit, per int) string {
	tmu.Lock()
	defer tmu.Unlock()
	var parts []string
	for _, s := range cur.Steps {
		if s.Status == "done" && len(s.Attempts) > 0 {
			parts = append(parts, "### "+s.Title+"\n"+cut(s.Attempts[len(s.Attempts)-1].Output, per))
		}
	}
	if len(parts) > limit {
		parts = parts[len(parts)-limit:]
	}
	return strings.Join(parts, "\n\n")
}

func workerPrompt(s model.Step, feedback string) string {
	p := fmt.Sprintf("ГЛАВНАЯ ЦЕЛЬ:\n%s\n\nПЛАН:\n%s\nРЕЗУЛЬТАТЫ ПРЕДЫДУЩИХ ШАГОВ:\n%s\n\nТЕКУЩИЙ ШАГ: %s\nКРИТЕРИЙ ГОТОВНОСТИ: %s",
		cur.Goal, overview(), doneOutputs(3, 1500), s.Title, s.Criteria)
	if feedback != "" {
		p += "\n\nЗАМЕЧАНИЯ К ПРОШЛОЙ ПОПЫТКЕ (исправь их):\n" + feedback
	}
	return p
}

func review(ctx context.Context, s model.Step, output string) (*model.Review, error) {
	var r model.Review
	err := chatJSON(ctx, *reviewer,
		"Ты строгий проверяющий. Оцени, выполнен ли шаг по критерию готовности. score 0–10. passed=true только если критерий выполнен полностью. В feedback кратко и конкретно: что не так и как исправить. Если есть журнал действий, верь только реальному выводу команд, а не словам исполнителя. Пиши на языке задачи.",
		fmt.Sprintf("ГЛАВНАЯ ЦЕЛЬ:\n%s\n\nШАГ: %s\nКРИТЕРИЙ: %s\n\nРЕЗУЛЬТАТ:\n%s", cur.Goal, s.Title, s.Criteria, squeeze(output)),
		obj(map[string]any{"passed": map[string]any{"type": "boolean"}, "score": num, "feedback": str}, "passed", "score", "feedback"), &r)
	if err != nil {
		return nil, err
	}
	r.Passed = r.Score >= 7
	return &r, nil
}

// проверка всей задачи; true если добавлены новые шаги
func finalCheck(ctx context.Context) bool {
	tmu.Lock()
	round := cur.Round
	tmu.Unlock()
	emit("", "final", "итоговая проверка всей задачи")
	var out struct {
		Score   int
		Summary string
		Missing []model.Step
	}
	err := chatJSON(ctx, *reviewer,
		"Ты проверяешь, достигнута ли главная цель целиком. Дай score 0–10 и краткий summary итога. Если чего-то важного не хватает, перечисли недостающие шаги в missing (иначе пустой список). Пиши на языке задачи.",
		fmt.Sprintf("ГЛАВНАЯ ЦЕЛЬ:\n%s\n\nПЛАН:\n%s\nРЕЗУЛЬТАТЫ:\n%s", cur.Goal, overview(), doneOutputs(20, 800)),
		obj(map[string]any{"score": num, "summary": str, "missing": stepList}, "score", "summary", "missing"), &out)
	if err != nil {
		emit("", "error", "итоговая проверка: %v", err)
		return false
	}
	add := len(out.Missing) > 0 && out.Score < 8 && round < *rounds
	update(func(t *model.Task) {
		t.Score, t.Summary = out.Score, out.Summary
		if add {
			t.Round++
			t.Steps = append(t.Steps, newSteps(out.Missing, len(t.Steps))...)
		}
	})
	emit("", "final", "итог %d/10: %s", out.Score, out.Summary)
	if add {
		emit("", "plan", "добавлено шагов: %d", len(out.Missing))
	}
	return add
}

// убирает утёкшие рассуждения модели; для JSON — всё до первой {
func clean(s string, isJSON bool) string {
	if i := strings.LastIndex(s, "</think>"); i >= 0 {
		s = s[i+len("</think>"):]
	}
	s = strings.TrimSpace(s)
	if isJSON {
		if i := strings.Index(s, "{"); i > 0 {
			s = s[i:]
		}
		if i := strings.LastIndex(s, "}"); i >= 0 {
			s = s[:i+1]
		}
	}
	return s
}
