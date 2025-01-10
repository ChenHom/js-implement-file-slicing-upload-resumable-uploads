package main

import (
	"encoding/json"
	"log"
	"net/http"
	"github.com/google/uuid"
	"sync"
)

type UploadTask struct {
	TaskID      string   `json:"task_id"`
	FileName    string   `json:"file_name"`
	FileSize    int64    `json:"file_size"`
	TotalChunks int      `json:"total_chunks"`
	FileHash    string   `json:"file_hash"`
	ChunkHashes []string `json:"chunk_hashes"`
}

var (
	tasks = make(map[string]UploadTask)
	mu    sync.Mutex
)

func createUploadTask(w http.ResponseWriter, r *http.Request) {
	var task UploadTask
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task.TaskID = uuid.New().String()

	mu.Lock()
	tasks[task.TaskID] = task
	mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"task_id": task.TaskID})
}

func main() {
	http.HandleFunc("/api/createUploadTask", createUploadTask)
	log.Fatal(http.ListenAndServe(":8080", nil))
}
