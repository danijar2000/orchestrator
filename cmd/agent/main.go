package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"orch/internal/model"
)

var (
	server   = flag.String("server", "http://localhost:8080", "server url")
	ollama   = flag.String("ollama", "http://localhost:11434", "ollama url")
	planner  = flag.String("planner", "qwen3.5-abliterated-32k", "model for planning")
	worker   = flag.String("worker", "dolphin3:8b", "model for doing steps")
	reviewer = flag.String("reviewer", "qwen3.5-abliterated-32k", "model for review")
	numCtx   = flag.Int("ctx", 16384, "context size")
	tries    = flag.Int("tries", 3, "attempts per step")
	rounds   = flag.Int("rounds", 2, "extra planning rounds after final check")
	awake    = flag.Bool("awake", true, "prevent sleep while working")
	think    = flag.Bool("think", false, "let models think before answering")
	outDir   = flag.String("out", "output", "folder for step results")

	tmu      sync.Mutex
	cur      *model.Task
	dirty    bool
	outbox   []model.Event
	lastTPS  float64
	cafe     *exec.Cmd
	stateDir = filepath.Join(os.Getenv("HOME"), ".llm-orchestrator")
)

func main() {
	flag.Parse()
	loadProfiles()
	if *workdir != "" {
		*workdir, _ = filepath.Abs(*workdir)
	}
	os.MkdirAll(stateDir, 0o755)
	if *workdir != "" {
		protected(*workdir)
	}
	go flusher()
	go heartbeat()
	for {
		t := claim()
		if t == nil {
			time.Sleep(5 * time.Second)
			continue
		}
		run(t)
	}
}

func post(method, path string, v any, out any) error {
	b, _ := json.Marshal(v)
	req, _ := http.NewRequest(method, *server+path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %s", method, path, res.Status)
	}
	if out != nil && res.StatusCode != 204 {
		return json.NewDecoder(res.Body).Decode(out)
	}
	return nil
}

func claim() *model.Task {
	var t model.Task
	if err := post("GET", "/api/agent/next", nil, &t); err != nil || t.ID == "" {
		return nil
	}
	// локальная копия свежее, если сервер был недоступен
	if b, err := os.ReadFile(filepath.Join(stateDir, t.ID+".json")); err == nil {
		var local model.Task
		if json.Unmarshal(b, &local) == nil && local.Updated.After(t.Updated) {
			t = local
		}
	}
	return &t
}

// изменения задачи только через эту функцию
func update(f func(t *model.Task)) {
	tmu.Lock()
	defer tmu.Unlock()
	f(cur)
	cur.Recalc()
	dirty = true
	b, _ := json.Marshal(cur)
	os.WriteFile(filepath.Join(stateDir, cur.ID+".json"), b, 0o644)
}

func emit(step, kind, msg string, args ...any) {
	msg = fmt.Sprintf(msg, args...)
	log.Printf("[%s] %s", kind, msg)
	tmu.Lock()
	outbox = append(outbox, model.Event{TaskID: cur.ID, StepID: step, TS: time.Now(), Kind: kind, Msg: msg})
	tmu.Unlock()
}

func flusher() {
	for range time.Tick(2 * time.Second) {
		tmu.Lock()
		evs := outbox
		var snap []byte
		var id string
		if dirty && cur != nil {
			snap, _ = json.Marshal(cur)
			id = cur.ID
		}
		tmu.Unlock()
		if len(evs) > 0 && post("POST", "/api/events", evs, nil) == nil {
			tmu.Lock()
			outbox = outbox[len(evs):]
			tmu.Unlock()
		}
		if snap != nil && post("PUT", "/api/tasks/"+id, json.RawMessage(snap), nil) == nil {
			tmu.Lock()
			dirty = false
			tmu.Unlock()
		}
	}
}

func heartbeat() {
	host, _ := os.Hostname()
	for {
		ok := false
		tmu.Lock()
		p := prof
		tmu.Unlock()
		req, _ := http.NewRequest("GET", strings.TrimRight(p.Worker.URL, "/")+"/models", nil)
		if k := os.Getenv(p.Worker.KeyEnv); p.Worker.KeyEnv != "" && k != "" {
			req.Header.Set("Authorization", "Bearer "+k)
		}
		if r, err := http.DefaultClient.Do(req); err == nil {
			r.Body.Close()
			ok = r.StatusCode < 300
		}
		tmu.Lock()
		a := model.Agent{Host: host, Awake: cafe != nil, Ollama: ok, TPS: lastTPS, Models: p.models(), Profiles: profileNames()}
		if cur != nil {
			a.TaskID = cur.ID
		}
		tmu.Unlock()
		post("POST", "/api/agent/heartbeat", a, nil)
		time.Sleep(5 * time.Second)
	}
}

func keepAwake(on bool) {
	tmu.Lock()
	defer tmu.Unlock()
	if on && cafe == nil && *awake {
		cafe = exec.Command("caffeinate", "-i", "-w", strconv.Itoa(os.Getpid()))
		if cafe.Start() != nil {
			cafe = nil
		}
	} else if !on && cafe != nil {
		cafe.Process.Kill()
		cafe.Wait()
		cafe = nil
	}
}

func status() string {
	tmu.Lock()
	defer tmu.Unlock()
	return cur.Status
}

// команды с сайта, пока задача в работе
func pollCommands(ctx context.Context, cancel context.CancelFunc) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(3 * time.Second):
		}
		var cmds []model.Command
		if post("GET", "/api/agent/commands?task="+cur.ID, nil, &cmds) != nil {
			continue
		}
		for _, c := range cmds {
			emit(c.StepID, "command", "команда: %s", c.Kind)
			update(func(t *model.Task) { apply(t, c) })
			if c.Kind == "cancel" {
				cancel()
			}
		}
	}
}

func apply(t *model.Task, c model.Command) {
	switch c.Kind {
	case "pause":
		t.Status = "paused"
	case "resume":
		t.Status = "running"
	case "cancel":
		t.Status = "canceled"
	case "retry", "skip":
		for i := range t.Steps {
			if t.Steps[i].ID == c.StepID {
				t.Steps[i].Status = map[string]string{"retry": "pending", "skip": "skipped"}[c.Kind]
			}
		}
	case "reorder":
		pos := map[string]int{}
		for i, id := range c.Order {
			pos[id] = i
		}
		sorted := make([]model.Step, 0, len(t.Steps))
		for _, id := range c.Order {
			for _, s := range t.Steps {
				if s.ID == id {
					sorted = append(sorted, s)
				}
			}
		}
		if len(sorted) == len(t.Steps) {
			t.Steps = sorted
		}
	}
}

func run(t *model.Task) {
	tmu.Lock()
	cur = t
	prof = pickProfile(t.Profile)
	tmu.Unlock()
	update(func(t *model.Task) { t.Profile, t.Models = prof.Name, prof.models() })
	keepAwake(true)
	defer keepAwake(false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go pollCommands(ctx, cancel)

	emit("", "start", "взял задачу (профиль %s): %s", prof.Name, t.Goal)
	if len(t.Steps) == 0 {
		update(func(t *model.Task) { t.Status = "planning" })
		steps, err := plan(ctx, t.Goal)
		if err != nil {
			emit("", "error", "план не получился: %v", err)
			update(func(t *model.Task) { t.Status = "failed" })
			return
		}
		update(func(t *model.Task) { t.Steps = steps })
		emit("", "plan", "план: %d шагов", len(steps))
	}
	if status() != "paused" {
		update(func(t *model.Task) { t.Status = "running" })
	}

	for {
		switch status() {
		case "canceled":
			emit("", "canceled", "задача отменена")
			return
		case "paused":
			time.Sleep(2 * time.Second)
			continue
		}
		i := nextStep()
		if i >= 0 {
			doStep(ctx, i)
			continue
		}
		if finalCheck(ctx) {
			continue
		}
		break
	}
	update(func(t *model.Task) {
		t.Status = "done"
		for _, s := range t.Steps {
			if s.Status == "failed" {
				t.Status = "failed"
			}
		}
	})
	emit("", "finish", "задача завершена: %s", status())
	time.Sleep(3 * time.Second) // дать flusher отправить последнее
}

func nextStep() int {
	tmu.Lock()
	defer tmu.Unlock()
	for i, s := range cur.Steps {
		if s.Status == "pending" || s.Status == "running" {
			return i
		}
	}
	return -1
}

func doStep(ctx context.Context, i int) {
	tmu.Lock()
	s := cur.Steps[i]
	tmu.Unlock()
	feedback := ""
	if n := len(s.Attempts); n > 0 && s.Attempts[n-1].Review != nil {
		feedback = s.Attempts[n-1].Review.Feedback
	}
	update(func(t *model.Task) { t.Steps[i].Status = "running" })
	for try := 1; try <= *tries; try++ {
		if ctx.Err() != nil || status() != "running" {
			update(func(t *model.Task) { t.Steps[i].Status = "pending" })
			return
		}
		emit(s.ID, "attempt", "«%s»: попытка %d", s.Title, try)
		a := model.Attempt{N: len(s.Attempts) + 1, Started: time.Now()}
		tmu.Lock()
		in0, out0, cost0 := cur.TokensIn, cur.TokensOut, cur.Cost
		tmu.Unlock()
		res, err := work(ctx, s, feedback)
		if err != nil {
			emit(s.ID, "error", "исполнитель: %v", err)
			continue
		}
		chk, ok := runCheck()
		res.Text += chk
		a.Output, a.TPS = res.Text, res.TPS
		r, err := review(ctx, s, res.Text)
		if err != nil {
			emit(s.ID, "error", "проверяющий: %v", err)
			r = &model.Review{Feedback: "проверка не удалась"}
		}
		if !ok && r.Passed {
			r.Passed, r.Score = false, min(r.Score, 6)
			r.Feedback = "Проверка `" + *check + "` не прошла. " + r.Feedback
		}
		a.Review, a.Finished = r, time.Now()
		tmu.Lock()
		a.TokensIn, a.Tokens, a.Cost = cur.TokensIn-in0, cur.TokensOut-out0, cur.Cost-cost0
		tmu.Unlock()
		s.Attempts = append(s.Attempts, a)
		update(func(t *model.Task) { t.Steps[i].Attempts = append(t.Steps[i].Attempts, a) })
		emit(s.ID, "review", "оценка %d/10: %s", r.Score, r.Feedback)
		if r.Passed {
			update(func(t *model.Task) { t.Steps[i].Status = "done" })
			saveOutput(i, s, a.Output)
			return
		}
		feedback = r.Feedback
	}
	update(func(t *model.Task) { t.Steps[i].Status = "failed" })
	emit(s.ID, "failed", "«%s» не прошёл проверку", s.Title)
}

func saveOutput(i int, s model.Step, text string) {
	dir := filepath.Join(*outDir, cur.ID)
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, fmt.Sprintf("%02d-%s.md", i+1, s.ID)), []byte("# "+s.Title+"\n\n"+text), 0o644)
}
