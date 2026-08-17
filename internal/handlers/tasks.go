package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"lambs-server-go/internal/auth"
	"lambs-server-go/internal/db"
	"lambs-server-go/internal/runtime"
)

type taskInput struct {
	Name    string `json:"name"`
	Cron    string `json:"cron"`
	Command string `json:"command"`
	Host    string `json:"host"`
	Enabled *bool  `json:"enabled"`
}

func validateTaskInput(name, cron, command, host string) error {
	if name == "" || len([]rune(name)) > 100 {
		return fmt.Errorf("任务名不能为空且不超过100字")
	}
	if err := runtime.ParseCron(cron); err != nil {
		return fmt.Errorf("cron 表达式无效: %v", err)
	}
	if command == "" {
		return fmt.Errorf("命令不能为空")
	}
	if host != "app1" && host != "windows" {
		return fmt.Errorf("执行机必须是 app1 或 windows")
	}
	return nil
}

func decodeTaskInput(r *http.Request) (taskInput, error) {
	var in taskInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		return in, fmt.Errorf("请求格式错误")
	}
	if in.Host == "" {
		in.Host = "app1"
	}
	if err := validateTaskInput(in.Name, in.Cron, in.Command, in.Host); err != nil {
		return in, err
	}
	return in, nil
}

// ListTasks returns all scheduled tasks of a project.
func ListTasks(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	rows, err := db.DB.Query("SELECT id, project_id, name, cron, command, host, enabled, COALESCE(last_run_at::text,''), last_status, last_log FROM scheduled_tasks WHERE project_id=$1 ORDER BY created_at", pid)
	if err != nil {
		auth.JSONErr(w, 500, "查询任务失败")
		return
	}
	defer rows.Close()
	out := []map[string]interface{}{}
	for rows.Next() {
		var id, project, name, cron, command, host, lastRun, status, logline string
		var enabled bool
		rows.Scan(&id, &project, &name, &cron, &command, &host, &enabled, &lastRun, &status, &logline)
		out = append(out, map[string]interface{}{
			"id": id, "project_id": project, "name": name, "cron": cron,
			"command": command, "host": host, "enabled": enabled,
			"last_run_at": lastRun, "last_status": status, "last_log": logline,
		})
	}
	if out == nil {
		out = []map[string]interface{}{}
	}
	auth.JSONOK(w, map[string]interface{}{"tasks": out})
}

// CreateTask adds a scheduled task to a project.
func CreateTask(w http.ResponseWriter, r *http.Request) {
	pid := r.PathValue("id")
	in, err := decodeTaskInput(r)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	id := fmt.Sprintf("t%d", time.Now().UnixNano())
	if _, err := db.DB.Exec("INSERT INTO scheduled_tasks (id, project_id, name, cron, command, host, enabled) VALUES ($1,$2,$3,$4,$5,$6,$7)", id, pid, in.Name, in.Cron, in.Command, in.Host, enabled); err != nil {
		auth.JSONErr(w, 500, "创建任务失败")
		return
	}
	auth.JSONOK(w, map[string]interface{}{"id": id})
}

// UpdateTask edits an existing task.
func UpdateTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	in, err := decodeTaskInput(r)
	if err != nil {
		auth.JSONErr(w, 400, err.Error())
		return
	}
	enabled := true
	if in.Enabled != nil {
		enabled = *in.Enabled
	}
	res, err := db.DB.Exec("UPDATE scheduled_tasks SET name=$1, cron=$2, command=$3, host=$4, enabled=$5 WHERE id=$6", in.Name, in.Cron, in.Command, in.Host, enabled, id)
	if err != nil {
		auth.JSONErr(w, 500, "更新任务失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		auth.JSONErr(w, 404, "任务不存在")
		return
	}
	auth.JSONOK(w, map[string]interface{}{"updated": id})
}

// DeleteTask removes a task.
func DeleteTask(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	res, err := db.DB.Exec("DELETE FROM scheduled_tasks WHERE id=$1", id)
	if err != nil {
		auth.JSONErr(w, 500, "删除任务失败")
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		auth.JSONErr(w, 404, "任务不存在")
		return
	}
	auth.JSONOK(w, map[string]interface{}{"deleted": id})
}

// RunTaskNow triggers an immediate run.
func RunTaskNow(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := runtime.StartTaskRun(id); err != nil {
		auth.JSONErr(w, 404, err.Error())
		return
	}
	auth.JSONOK(w, map[string]interface{}{"started": id})
}
