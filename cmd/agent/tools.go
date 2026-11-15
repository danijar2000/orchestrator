package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"orch/internal/model"
)

var (
	workdir    = flag.String("workdir", "", "folder the model may read/write and run commands in (empty = text only)")
	allow      = flag.String("allow", "go,gofmt,ls,cat,grep,find,head,tail,wc,mkdir", "allowed commands")
	maxActions = flag.Int("actions", 20, "max tool calls per attempt")
)

const toolSystem = `Ты исполнитель с доступом к папке проекта. Выполни ОДИН шаг большой задачи.
Каждый ответ — одно действие в JSON:
- {"action":"list","path":"."} — файлы в папке
- {"action":"read","path":"file"} — прочитать файл
- {"action":"write","path":"file"} — записать файл целиком (текст файла попрошу следующим сообщением)
- {"action":"edit","path":"file","old":"точный кусок текста","new":"на что заменить"} — точечная правка (лучше, чем переписывать файл целиком)
- {"action":"delete","path":"file"} — удалить свой файл
- {"action":"run","cmd":"go test ./..."} — выполнить команду (без | ; && и без shell)
- {"action":"done","result":"что сделано и чем это подтверждено"} — шаг готов
Исходный код проекта менять нельзя. Можно создавать, менять и удалять свои новые файлы и *_test.go. Имя Go-модуля (orch) — это не папка: пакет orch/internal/model лежит в internal/model. Тесты клади рядом с кодом в тот же пакет (например cmd/server/server_test.go, package main).
Сначала изучи нужные файлы, потом действуй. Проверяй свою работу командами. Отвечай на языке задачи.`

var actionSchema = obj(map[string]any{
	"action": map[string]any{"type": "string", "enum": []string{"list", "read", "write", "edit", "delete", "run", "done"}},
	"path":   str, "content": str, "cmd": str, "result": str, "old": str, "new": str,
}, "action")

func work(ctx context.Context, s model.Step, feedback string) (result, error) {
	if prof.Harness == "claude" && *workdir != "" {
		return runHarness(ctx, s, feedback)
	}
	if *workdir == "" {
		return chat(ctx, prof.Worker, workerSystem, workerPrompt(s, feedback), nil)
	}
	msgs := []msg{{"role": "system", "content": toolSystem}, {"role": "user", "content": workerPrompt(s, feedback)}}
	var log strings.Builder
	files := map[string]string{}
	seen := map[string]bool{}
	fails := map[string]int{}
	total := result{}
	for range *maxActions {
		r, err := chatMsgs(ctx, prof.Worker, msgs, actionSchema)
		if err != nil {
			return total, err
		}
		total.Tokens += r.Tokens
		total.TPS = r.TPS
		var a struct{ Action, Path, Content, Cmd, Result, Old, New string }
		if json.Unmarshal([]byte(r.Text), &a) != nil {
			emit(s.ID, "tool", "не JSON: %s", cut(r.Text, 200))
			msgs = append(msgs, msg{"role": "assistant", "content": r.Text}, msg{"role": "user", "content": "Ответ не JSON. Повтори одним действием."})
			continue
		}
		if a.Action == "done" {
			total.Text = "## Итог\n" + a.Result + "\n\n## Журнал действий\n" + log.String()
			return total, nil
		}
		if a.Action == "write" && strings.TrimSpace(a.Content) == "" {
			c, err := writeFile(ctx, s, a.Path, files)
			if err != nil {
				return total, err
			}
			total.Tokens += c.Tokens
			a.Content = c.Text
		}
		key := a.Action + a.Path + a.Cmd
		if a.Action != "write" && a.Action != "run" && seen[key] {
			emit(s.ID, "tool", "повтор: %s", key)
			msgs = append(msgs, msg{"role": "assistant", "content": r.Text}, msg{"role": "user", "content": "Ты это уже делал, результат выше. Не повторяй, сделай следующее действие (например write или run)."})
			continue
		}
		seen[key] = true
		var out string
		if a.Action == "edit" {
			out = edit(a.Path, a.Old, a.New)
		} else {
			out = act(a.Action, a.Path, a.Content, a.Cmd)
		}
		if a.Action == "read" || a.Action == "write" || a.Action == "edit" {
			if b, err := os.ReadFile(filepath.Join(*workdir, a.Path)); err == nil {
				files[a.Path] = string(b)
			}
		}
		if a.Action == "run" {
			files["вывод: "+a.Cmd] = out
		}
		desc := a.Action + " " + a.Path + a.Cmd
		emit(s.ID, "tool", "%s → %s", desc, cut(strings.SplitN(strings.TrimPrefix(out, "$ "+a.Cmd+"\n"), "\n", 2)[0], 120))
		fmt.Fprintf(&log, "### %s\n```\n%s\n```\n", desc, cut(out, 1500))
		if a.Action == "run" && strings.Contains(out, "ОШИБКА:") {
			k := cut(out, 400)
			if fails[k]++; fails[k] >= 3 {
				emit(s.ID, "tool", "застрял: одна и та же ошибка 3 раза")
				total.Text = "## Итог\nЗАСТРЯЛ: одна и та же ошибка повторилась 3 раза, попытка остановлена.\n```\n" + cut(out, 3000) + "\n```\n\n## Журнал действий\n" + log.String()
				return total, nil
			}
		}
		msgs = append(msgs, msg{"role": "assistant", "content": r.Text}, msg{"role": "user", "content": "РЕЗУЛЬТАТ:\n" + cut(out, 6000)})
	}
	total.Text = "## Итог\nЛимит действий исчерпан, шаг не завершён.\n\n## Журнал действий\n" + log.String()
	return total, nil
}

func act(action, path, content, cmd string) string {
	switch action {
	case "delete":
		p, err := inside(path)
		if err != nil {
			return err.Error()
		}
		if protected(p) {
			return "ОТКАЗ: исходный код проекта удалять нельзя. Удалять можно только свои новые файлы."
		}
		if err := os.Remove(p); err != nil {
			return err.Error()
		}
		return "удалено"
	case "list", "read", "write":
		p, err := inside(path)
		if err != nil {
			return err.Error()
		}
		switch action {
		case "list":
			var files []string
			filepath.WalkDir(p, func(f string, d os.DirEntry, _ error) error {
				if d != nil && d.IsDir() && (d.Name() == "node_modules" || d.Name() == ".git") {
					return filepath.SkipDir
				}
				if d != nil && !d.IsDir() {
					rel, _ := filepath.Rel(*workdir, f)
					files = append(files, rel)
				}
				return nil
			})
			return strings.Join(files, "\n")
		case "read":
			b, err := os.ReadFile(p)
			if err != nil {
				return err.Error()
			}
			return string(b)
		default:
			if strings.TrimSpace(content) == "" {
				return "ОТКАЗ: пустой content. Передай полный текст файла в поле content."
			}
			if protected(p) {
				return "ОТКАЗ: исходный код проекта менять нельзя (кроме *_test.go). Свои новые файлы менять можно."
			}
			os.MkdirAll(filepath.Dir(p), 0o755)
			content, note := fixPackage(p, content)
			if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
				return err.Error()
			}
			return fmt.Sprintf("записано %d байт%s", len(content), note)
		}
	case "run":
		args := strings.Fields(cmd)
		if len(args) == 0 || !slices.Contains(strings.Split(*allow, ","), args[0]) || strings.ContainsAny(cmd, "|;&><`$") {
			return "команда запрещена. Разрешено: " + *allow + " (без | ; & > < ` $)"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		c := exec.CommandContext(ctx, args[0], args[1:]...)
		c.Dir = *workdir
		out, err := c.CombinedOutput()
		res := string(out)
		if err != nil {
			res += "\nОШИБКА: " + err.Error()
		}
		return "$ " + cmd + "\n" + res
	}
	return "неизвестное действие"
}

func inside(path string) (string, error) {
	p := filepath.Join(*workdir, path)
	if rel, err := filepath.Rel(*workdir, p); err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("путь вне рабочей папки")
	}
	return p, nil
}

func stripFence(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "```") {
		s = s[strings.Index(s, "\n")+1:]
		s = strings.TrimSuffix(strings.TrimSpace(s), "```")
	}
	return s + "\n"
}

func writeFile(ctx context.Context, s model.Step, path string, files map[string]string) (result, error) {
	var b strings.Builder
	for k, v := range files {
		fmt.Fprintf(&b, "=== %s ===\n%s\n\n", k, cut(v, 12000))
	}
	user := workerPrompt(s, "") + "\n\nИЗВЕСТНЫЕ ФАЙЛЫ И ВЫВОДЫ КОМАНД:\n" + b.String() + "\nНапиши полный текст файла " + path
	r, err := chat(ctx, prof.Worker, "Ты программист. Выведи только полное содержимое одного файла, без пояснений и без JSON.", user, nil)
	r.Text = stripFence(r.Text)
	if strings.HasSuffix(path, ".go") {
		if i := strings.Index(r.Text, "package "); i > 0 {
			r.Text = r.Text[i:]
		}
	}
	return r, err
}

// исходные файлы проекта, которые были в папке при первом запуске агента
var baseline map[string]bool

func protected(p string) bool {
	if baseline == nil {
		baseline = map[string]bool{}
		f := filepath.Join(stateDir, "baseline-"+strings.ReplaceAll(*workdir, "/", "_")+".json")
		if b, err := os.ReadFile(f); err == nil {
			json.Unmarshal(b, &baseline)
		} else {
			filepath.WalkDir(*workdir, func(x string, d os.DirEntry, _ error) error {
				if d != nil && !d.IsDir() {
					baseline[x] = true
				}
				return nil
			})
			b, _ := json.Marshal(baseline)
			os.WriteFile(f, b, 0o644)
		}
	}
	return baseline[p] && !strings.HasSuffix(p, "_test.go")
}

var pkgRe = regexp.MustCompile(`(?m)^package\s+(\w+)`)

// подставляет в .go-файл тот же пакет, что у остальных файлов папки
func fixPackage(p, content string) (string, string) {
	if !strings.HasSuffix(p, ".go") {
		return content, ""
	}
	others, _ := filepath.Glob(filepath.Join(filepath.Dir(p), "*.go"))
	for _, o := range others {
		if o == p || strings.HasSuffix(o, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(o)
		want := pkgRe.FindStringSubmatch(string(b))
		got := pkgRe.FindStringSubmatch(content)
		if want != nil && got != nil && want[1] != got[1] {
			return pkgRe.ReplaceAllString(content, "package "+want[1]), fmt.Sprintf(" (package %s заменён на %s, как в %s)", got[1], want[1], filepath.Base(o))
		}
		break
	}
	return content, ""
}

func edit(path, old, new string) string {
	p, err := inside(path)
	if err != nil {
		return err.Error()
	}
	if protected(p) {
		return "ОТКАЗ: исходный код проекта менять нельзя (кроме *_test.go)."
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err.Error()
	}
	if n := strings.Count(string(b), old); old == "" || n != 1 {
		return fmt.Sprintf("ОТКАЗ: кусок old должен встречаться в файле ровно 1 раз (найдено %d). Прочитай файл и скопируй точный текст.", n)
	}
	c, note := fixPackage(p, strings.Replace(string(b), old, new, 1))
	os.WriteFile(p, []byte(c), 0o644)
	return "исправлено" + note
}
