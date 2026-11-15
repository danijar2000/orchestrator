package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"sync"
	"time"

	"orch/internal/model"
	"orch/web"

	_ "modernc.org/sqlite"
)

var (
	db    *sql.DB
	mu    sync.Mutex
	agent model.Agent
	subs  = map[chan string]bool{}
)

func main() {
	addr := flag.String("addr", ":8080", "listen address")
	dbPath := flag.String("db", "orch.db", "sqlite file")
	flag.Parse()

	var err error
	db, err = sql.Open("sqlite", *dbPath+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	must(err)
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS tasks(id TEXT PRIMARY KEY, data TEXT, created INTEGER);
CREATE TABLE IF NOT EXISTS events(id INTEGER PRIMARY KEY, task_id TEXT, step_id TEXT, ts INTEGER, kind TEXT, msg TEXT);
CREATE INDEX IF NOT EXISTS ev_task ON events(task_id, id);
CREATE TABLE IF NOT EXISTS commands(id INTEGER PRIMARY KEY, task_id TEXT, data TEXT, done INTEGER DEFAULT 0);`)
	must(err)

	h := http.NewServeMux()
	h.HandleFunc("GET /api/tasks", listTasks)
	h.HandleFunc("POST /api/tasks", createTask)
	h.HandleFunc("GET /api/tasks/{id}", getTask)
	h.HandleFunc("PUT /api/tasks/{id}", putTask)
	h.HandleFunc("DELETE /api/tasks/{id}", deleteTask)
	h.HandleFunc("GET /api/tasks/{id}/events", listEvents)
	h.HandleFunc("POST /api/tasks/{id}/commands", addCommand)
	h.HandleFunc("POST /api/events", addEvents)
	h.HandleFunc("GET /api/agent", func(w http.ResponseWriter, r *http.Request) { mu.Lock(); defer mu.Unlock(); js(w, agent) })
	h.HandleFunc("POST /api/agent/heartbeat", heartbeat)
	h.HandleFunc("GET /api/agent/next", nextTask)
	h.HandleFunc("GET /api/agent/commands", pendingCommands)
	h.HandleFunc("GET /api/stream", stream)
	dist, _ := fs.Sub(web.Dist, "dist")
	h.Handle("/", http.FileServerFS(dist))

	log.Println("listening on", *addr)
	must(http.ListenAndServe(*addr, h))
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}

func js(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func broadcast(kind, id string) {
	msg := fmt.Sprintf(`{"kind":%q,"task_id":%q}`, kind, id)
	mu.Lock()
	defer mu.Unlock()
	for c := range subs {
		select {
		case c <- msg:
		default:
		}
	}
}

func stream(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	c := make(chan string, 64)
	mu.Lock()
	subs[c] = true
	mu.Unlock()
	defer func() { mu.Lock(); delete(subs, c); mu.Unlock() }()
	f := w.(http.Flusher)
	tick := time.NewTicker(20 * time.Second)
	defer tick.Stop()
	for {
		select {
		case m := <-c:
			fmt.Fprintf(w, "data: %s\n\n", m)
		case <-tick.C:
			fmt.Fprint(w, ": ping\n\n")
		case <-r.Context().Done():
			return
		}
		f.Flush()
	}
}

func loadTask(id string) (*model.Task, error) {
	var data string
	if err := db.QueryRow(`SELECT data FROM tasks WHERE id=?`, id).Scan(&data); err != nil {
		return nil, err
	}
	var t model.Task
	return &t, json.Unmarshal([]byte(data), &t)
}

func saveTask(t *model.Task) error {
	b, _ := json.Marshal(t)
	_, err := db.Exec(`INSERT INTO tasks(id,data,created) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET data=excluded.data`,
		t.ID, string(b), t.Created.Unix())
	return err
}

func listTasks(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT data FROM tasks ORDER BY created DESC`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	out := []json.RawMessage{}
	for rows.Next() {
		var s string
		rows.Scan(&s)
		out = append(out, json.RawMessage(s))
	}
	js(w, out)
}

func createTask(w http.ResponseWriter, r *http.Request) {
	var in struct{ Goal, Profile string }
	if json.NewDecoder(r.Body).Decode(&in) != nil || in.Goal == "" {
		http.Error(w, "goal required", 400)
		return
	}
	now := time.Now()
	t := &model.Task{ID: fmt.Sprintf("t%d", now.UnixMilli()), Goal: in.Goal, Profile: in.Profile, Status: "queued", Steps: []model.Step{}, Created: now, Updated: now}
	must2(w, saveTask(t))
	broadcast("task", t.ID)
	js(w, t)
}

func must2(w http.ResponseWriter, err error) {
	if err != nil {
		http.Error(w, err.Error(), 500)
	}
}

func getTask(w http.ResponseWriter, r *http.Request) {
	t, err := loadTask(r.PathValue("id"))
	if err != nil {
		http.Error(w, "not found", 404)
		return
	}
	js(w, t)
}

func putTask(w http.ResponseWriter, r *http.Request) {
	var t model.Task
	if json.NewDecoder(r.Body).Decode(&t) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	t.ID = r.PathValue("id")
	must2(w, saveTask(&t))
	broadcast("task", t.ID)
}

func deleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	db.Exec(`DELETE FROM tasks WHERE id=?`, id)
	db.Exec(`DELETE FROM events WHERE task_id=?`, id)
	db.Exec(`DELETE FROM commands WHERE task_id=?`, id)
	broadcast("task", id)
}

func listEvents(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT id,task_id,step_id,ts,kind,msg FROM events WHERE task_id=? ORDER BY id DESC LIMIT 1000`, r.PathValue("id"))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	out := []model.Event{}
	for rows.Next() {
		var e model.Event
		var ts int64
		rows.Scan(&e.ID, &e.TaskID, &e.StepID, &ts, &e.Kind, &e.Msg)
		e.TS = time.UnixMilli(ts)
		out = append(out, e)
	}
	js(w, out)
}

func addEvents(w http.ResponseWriter, r *http.Request) {
	var evs []model.Event
	if json.NewDecoder(r.Body).Decode(&evs) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	for _, e := range evs {
		db.Exec(`INSERT INTO events(task_id,step_id,ts,kind,msg) VALUES(?,?,?,?,?)`, e.TaskID, e.StepID, e.TS.UnixMilli(), e.Kind, e.Msg)
		broadcast("event", e.TaskID)
	}
}

func addCommand(w http.ResponseWriter, r *http.Request) {
	var c model.Command
	if json.NewDecoder(r.Body).Decode(&c) != nil {
		http.Error(w, "bad json", 400)
		return
	}
	c.TaskID = r.PathValue("id")
	// задачу, которую агент ещё не взял, правим сразу
	if t, err := loadTask(c.TaskID); err == nil && t.Status == "queued" && c.Kind == "cancel" {
		t.Status = "canceled"
		saveTask(t)
		broadcast("task", t.ID)
		return
	}
	// завершённую задачу переоткрываем, агент подхватит её снова
	if t, err := loadTask(c.TaskID); err == nil && (t.Status == "done" || t.Status == "failed") && (c.Kind == "retry" || c.Kind == "skip") {
		for i := range t.Steps {
			if t.Steps[i].ID == c.StepID {
				t.Steps[i].Status = map[string]string{"retry": "pending", "skip": "skipped"}[c.Kind]
			}
		}
		t.Status = "running"
		t.Recalc()
		saveTask(t)
		broadcast("task", t.ID)
		return
	}
	b, _ := json.Marshal(c)
	db.Exec(`INSERT INTO commands(task_id,data) VALUES(?,?)`, c.TaskID, string(b))
}

func pendingCommands(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("task")
	rows, err := db.Query(`SELECT id,data FROM commands WHERE task_id=? AND done=0 ORDER BY id`, id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	out := []model.Command{}
	var ids []int64
	for rows.Next() {
		var c model.Command
		var cid int64
		var s string
		rows.Scan(&cid, &s)
		json.Unmarshal([]byte(s), &c)
		c.ID = cid
		out = append(out, c)
		ids = append(ids, cid)
	}
	rows.Close()
	for _, cid := range ids {
		db.Exec(`UPDATE commands SET done=1 WHERE id=?`, cid)
	}
	js(w, out)
}

func heartbeat(w http.ResponseWriter, r *http.Request) {
	var a model.Agent
	json.NewDecoder(r.Body).Decode(&a)
	a.Seen = time.Now()
	mu.Lock()
	agent = a
	mu.Unlock()
	broadcast("agent", "")
}

// незавершённые задачи по очереди: сначала уже начатые (после перезапуска агента), потом новые
func nextTask(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(`SELECT data FROM tasks ORDER BY created`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var queued *model.Task
	for rows.Next() {
		var s string
		var t model.Task
		rows.Scan(&s)
		json.Unmarshal([]byte(s), &t)
		switch t.Status {
		case "planning", "running", "paused":
			js(w, t)
			return
		case "queued":
			if queued == nil {
				queued = &t
			}
		}
	}
	if queued == nil {
		w.WriteHeader(204)
		return
	}
	js(w, queued)
}
