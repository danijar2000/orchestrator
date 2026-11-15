package model

import "time"

type Review struct {
	Passed   bool   `json:"passed"`
	Score    int    `json:"score"`
	Feedback string `json:"feedback"`
}

type Attempt struct {
	N        int       `json:"n"`
	Output   string    `json:"output"`
	Review   *Review   `json:"review,omitempty"`
	Started  time.Time `json:"started"`
	Finished time.Time `json:"finished"`
	Tokens   int       `json:"tokens"`
	TPS      float64   `json:"tps"`
}

type Step struct {
	ID       string    `json:"id"`
	Title    string    `json:"title"`
	Criteria string    `json:"criteria"`
	Weight   int       `json:"weight"`
	Status   string    `json:"status"` // pending running done failed skipped
	Attempts []Attempt `json:"attempts"`
}

type Task struct {
	ID       string    `json:"id"`
	Goal     string    `json:"goal"`
	Status   string    `json:"status"` // queued planning running paused done failed canceled
	Progress float64   `json:"progress"`
	Score    int       `json:"score"`
	Summary  string    `json:"summary"`
	Round    int       `json:"round"`
	Steps    []Step    `json:"steps"`
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
}

type Event struct {
	ID     int64     `json:"id"`
	TaskID string    `json:"task_id"`
	StepID string    `json:"step_id,omitempty"`
	TS     time.Time `json:"ts"`
	Kind   string    `json:"kind"`
	Msg    string    `json:"msg"`
}

type Command struct {
	ID     int64    `json:"id"`
	TaskID string   `json:"task_id"`
	Kind   string   `json:"kind"` // pause resume cancel retry skip reorder
	StepID string   `json:"step_id,omitempty"`
	Order  []string `json:"order,omitempty"`
}

type Agent struct {
	Host   string            `json:"host"`
	Awake  bool              `json:"awake"`
	Ollama bool              `json:"ollama"`
	Models map[string]string `json:"models"`
	TaskID string            `json:"task_id"`
	TPS    float64           `json:"tps"`
	Seen   time.Time         `json:"seen"`
}

func (t *Task) Recalc() {
	var all, done int
	for _, s := range t.Steps {
		w := max(s.Weight, 1)
		all += w
		if s.Status == "done" || s.Status == "skipped" {
			done += w
		}
	}
	if all > 0 {
		t.Progress = float64(done) / float64(all)
	}
	t.Updated = time.Now()
}
