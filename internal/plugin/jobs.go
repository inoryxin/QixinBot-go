package plugin

import (
	"fmt"
	"sync"
	"time"
)

// jobRegistry 记录正在跑的后台任务。
//
// 插件用 host.background 起任务后，handle_message 立刻返回，
// 用户不用等；任务跑完再发结果。
//
// **注意**：进程隔离意味着插件进程死了，它起的后台任务也一起没了。
// 这里只负责"看得见"，不负责"活过重启"——要跨重启就在启动时自行恢复。
type jobRegistry struct {
	mu   sync.Mutex
	jobs map[string]*Job
}

// Job 是一个后台任务的状态。
type Job struct {
	ID       string `json:"id"`
	Plugin   string `json:"plugin"`
	Name     string `json:"name"`
	ChatType string `json:"chat_type,omitempty"`
	ChatID   int64  `json:"chat_id,omitempty"`
	StartedAt int64 `json:"started_at"`
	Done      bool  `json:"done"`
	Err       string `json:"error,omitempty"`
	EndedAt   int64 `json:"ended_at,omitempty"`
}

func newJobRegistry() *jobRegistry {
	return &jobRegistry{jobs: make(map[string]*Job)}
}

// jobKey 复合键：不同插件的 job id 各自命名空间，互不覆盖/互关。
func jobKey(pluginID, jobID string) string { return pluginID + "/" + jobID }

// Begin 登记一个后台任务。
func (r *jobRegistry) Begin(pluginID, jobID, name, chatType string, chatID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[jobKey(pluginID, jobID)] = &Job{
		ID: jobID, Plugin: pluginID, Name: name,
		ChatType: chatType, ChatID: chatID,
		StartedAt: time.Now().Unix(),
	}
	// 顺手清理：超过 1 小时还挂着的基本是插件忘了调 end
	if len(r.jobs) > 512 {
		cutoff := time.Now().Add(-time.Hour).Unix()
		for id, j := range r.jobs {
			if j.StartedAt < cutoff {
				delete(r.jobs, id)
			}
		}
	}
}

// End 标记完成（校验任务属于调用方，防止互相误关）。
func (r *jobRegistry) End(pluginID, jobID string, err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[jobKey(pluginID, jobID)]
	if !ok || j.Plugin != pluginID {
		return
	}
	j.Done = true
	j.EndedAt = time.Now().Unix()
	if err != nil {
		j.Err = err.Error()
	}
}

// CancelPlugin 清掉某个插件的任务（插件卸载时）。
func (r *jobRegistry) CancelPlugin(pluginID string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for key, j := range r.jobs {
		if j.Plugin == pluginID {
			delete(r.jobs, key)
			n++
		}
	}
	return n
}

// List 返回所有任务（最近的在前）。
func (r *jobRegistry) List() []Job {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]Job, 0, len(r.jobs))
	for _, j := range r.jobs {
		out = append(out, *j)
	}
	// 新的在前
	for i := 1; i < len(out); i++ {
		for k := i; k > 0 && out[k].StartedAt > out[k-1].StartedAt; k-- {
			out[k], out[k-1] = out[k-1], out[k]
		}
	}
	return out
}

// ListJobs 给 WebUI 用。
func (m *Manager) ListJobs() []Job {
	if m.jobs == nil {
		return nil
	}
	return m.jobs.List()
}

// BeginJob / EndJob 给 host.job_begin / host.job_end 用。
func (m *Manager) BeginJob(pluginID, jobID, name, chatType string, chatID int64) {
	if m.jobs == nil || jobID == "" {
		return
	}
	m.jobs.Begin(pluginID, jobID, name, chatType, chatID)
}

func (m *Manager) EndJob(pluginID, jobID string, err error) {
	if m.jobs == nil {
		return
	}
	m.jobs.End(pluginID, jobID, err)
}

var _ = fmt.Sprintf
