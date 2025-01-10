package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/time/rate"
)

type UploadTask struct {
	TaskID        string   `json:"task_id"`
	FileName      string   `json:"file_name"`
	FileSize      int64    `json:"file_size"`
	TotalChunks   int      `json:"total_chunks"`
	FileHash      string   `json:"file_hash"`
	ChunkHashes   []string `json:"chunk_hashes"`
	UploadedChunks []int    `json:"uploaded_chunks"`
}

type ChunkUpload struct {
	TaskID    string `json:"task_id"`
	ChunkIndex int    `json:"chunk_index"`
	ChunkHash  string `json:"chunk_hash"`
}

var (
	db   *sql.DB
	mu   sync.Mutex
	limiter = rate.NewLimiter(1, 5) // 每秒 1 個請求，最多允許 5 個突發請求
	userUploadLimits = make(map[string]*rate.Limiter)
	logger = logrus.New()
)

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", "./upload_tasks.db")
	if err != nil {
		logger.WithError(err).Fatal("Failed to open database")
	}

	createTableSQL := `CREATE TABLE IF NOT EXISTS upload_tasks (
		"task_id" TEXT NOT NULL PRIMARY KEY,
		"file_name" TEXT,
		"file_size" INTEGER,
		"total_chunks" INTEGER,
		"file_hash" TEXT,
		"chunk_hashes" TEXT,
		"uploaded_chunks" TEXT
	);`

	_, err = db.Exec(createTableSQL)
	if err != nil {
		logger.WithError(err).Fatal("Failed to create table")
	}
}

func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !limiter.Allow() {
			logger.Warn("Too Many Requests")
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func userRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.FormValue("task_id")
		if userID == "" {
			logger.Warn("Missing task_id")
			http.Error(w, "Missing task_id", http.StatusBadRequest)
			return
		}

		mu.Lock()
		userLimiter, exists := userUploadLimits[userID]
		if !exists {
			userLimiter = rate.NewLimiter(1, 3) // 每秒 1 個請求，最多允許 3 個突發請求
			userUploadLimits[userID] = userLimiter
		}
		mu.Unlock()

		if !userLimiter.Allow() {
			logger.WithField("task_id", userID).Warn("Too Many Requests for user")
			http.Error(w, "Too Many Requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func createUploadTask(w http.ResponseWriter, r *http.Request) {
	var task UploadTask
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		logger.WithError(err).Error("Failed to decode request body")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task.TaskID = uuid.New().String()

	chunkHashesJSON, err := json.Marshal(task.ChunkHashes)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal chunk hashes")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	uploadedChunksJSON, err := json.Marshal([]int{})
	if err != nil {
		logger.WithError(err).Error("Failed to marshal uploaded chunks")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mu.Lock()
	_, err = db.Exec(`INSERT INTO upload_tasks (task_id, file_name, file_size, total_chunks, file_hash, chunk_hashes, uploaded_chunks) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		task.TaskID, task.FileName, task.FileSize, task.TotalChunks, task.FileHash, string(chunkHashesJSON), string(uploadedChunksJSON))
	mu.Unlock()

	if err != nil {
		logger.WithError(err).Error("Failed to insert upload task")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.WithFields(logrus.Fields{
		"task_id": task.TaskID,
		"file_name": task.FileName,
		"file_size": task.FileSize,
		"total_chunks": task.TotalChunks,
	}).Info("Created upload task")
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"task_id": task.TaskID})
}

func uploadChunk(w http.ResponseWriter, r *http.Request) {
	var chunk ChunkUpload
	if err := r.ParseMultipartForm(10 << 20); err != nil { // 10MB
		logger.WithError(err).Error("Failed to parse multipart form")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	chunk.TaskID = r.FormValue("task_id")
	chunk.ChunkIndex, _ = strconv.Atoi(r.FormValue("chunk_index"))
	chunk.ChunkHash = r.FormValue("chunk_hash")

	file, _, err := r.FormFile("file_chunk")
	if err != nil {
		logger.WithError(err).Error("Failed to get file chunk")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	defer file.Close()

	// 驗證請求的合法性，檢查 task_id 和分片編號
	mu.Lock()
	task, exists := tasks[chunk.TaskID]
	mu.Unlock()
	if !exists {
		logger.WithField("task_id", chunk.TaskID).Warn("Invalid task_id")
		http.Error(w, "Invalid task_id", http.StatusBadRequest)
		return
	}

	// 使用臨時儲存路徑保存分片
	tempFilePath := fmt.Sprintf("./uploads/%s_%d", chunk.TaskID, chunk.ChunkIndex)
	tempFile, err := os.Create(tempFilePath)
	if err != nil {
		logger.WithError(err).Error("Failed to create temp file")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tempFile.Close()

	_, err = io.Copy(tempFile, file)
	if err != nil {
		logger.WithError(err).Error("Failed to copy file chunk")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 驗證分片的哈希值，確保資料完整性
	tempFile.Seek(0, 0)
	hash := sha256.New()
	if _, err := io.Copy(hash, tempFile); err != nil {
		logger.WithError(err).Error("Failed to calculate hash")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	calculatedHash := fmt.Sprintf("%x", hash.Sum(nil))
	if calculatedHash != chunk.ChunkHash {
		logger.WithFields(logrus.Fields{
			"task_id": chunk.TaskID,
			"chunk_index": chunk.ChunkIndex,
		}).Warn("Invalid chunk hash")
		http.Error(w, "Invalid chunk hash", http.StatusBadRequest)
		return
	}

	// 更新資料庫中的分片上傳狀態
	mu.Lock()
	defer mu.Unlock()
	uploadedChunks := append(task.UploadedChunks, chunk.ChunkIndex)
	uploadedChunksJSON, err := json.Marshal(uploadedChunks)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal uploaded chunks")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	_, err = db.Exec(`UPDATE upload_tasks SET uploaded_chunks = ? WHERE task_id = ?`, string(uploadedChunksJSON), chunk.TaskID)
	if err != nil {
		logger.WithError(err).Error("Failed to update uploaded chunks")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	logger.WithFields(logrus.Fields{
		"task_id": chunk.TaskID,
		"chunk_index": chunk.ChunkIndex,
	}).Info("Uploaded chunk")
	w.WriteHeader(http.StatusOK)
}

func cleanUpExpiredFiles() {
	for {
		time.Sleep(24 * time.Hour) // 每 24 小時執行一次清理
		mu.Lock()
		files, err := os.ReadDir("./uploads")
		if err != nil {
			logger.WithError(err).Error("Error reading uploads directory")
			mu.Unlock()
			continue
		}

		for _, file := range files {
			info, err := file.Info()
			if err != nil {
				logger.WithError(err).Error("Error getting file info")
				continue
			}

			if time.Since(info.ModTime()) > 24*time.Hour {
				err = os.Remove("./uploads/" + file.Name())
				if err != nil {
					logger.WithError(err).Error("Error removing file")
				}
			}
		}
		mu.Unlock()
	}
}

func ping(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func mergeChunks(w http.ResponseWriter, r *http.Request) {
	var req struct {
		TaskID string `json:"task_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.WithError(err).Error("Failed to decode request body")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mu.Lock()
	task, exists := tasks[req.TaskID]
	mu.Unlock()
	if !exists {
		logger.WithField("task_id", req.TaskID).Warn("Invalid task_id")
		http.Error(w, "Invalid task_id", http.StatusBadRequest)
		return
	}

	// 檢查所有分片是否已上傳完成
	if len(task.UploadedChunks) != task.TotalChunks {
		logger.WithField("task_id", req.TaskID).Warn("Not all chunks uploaded")
		http.Error(w, "Not all chunks uploaded", http.StatusBadRequest)
		return
	}

	// 合併分片
	mergedFilePath := fmt.Sprintf("./uploads/%s", task.FileName)
	mergedFile, err := os.Create(mergedFilePath)
	if err != nil {
		logger.WithError(err).Error("Failed to create merged file")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer mergedFile.Close()

	for i := 0; i < task.TotalChunks; i++ {
		chunkFilePath := fmt.Sprintf("./uploads/%s_%d", task.TaskID, i)
		chunkFile, err := os.Open(chunkFilePath)
		if err != nil {
			logger.WithError(err).Error("Failed to open chunk file")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer chunkFile.Close()

		_, err = io.Copy(mergedFile, chunkFile)
		if err != nil {
			logger.WithError(err).Error("Failed to copy chunk file")
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}

		// 刪除已合併的分片
		err = os.Remove(chunkFilePath)
		if err != nil {
			logger.WithError(err).Error("Error removing chunk file")
		}
	}

	// 驗證合併後的檔案哈希值
	mergedFile.Seek(0, 0)
	hash := sha256.New()
	if _, err := io.Copy(hash, mergedFile); err != nil {
		logger.WithError(err).Error("Failed to calculate hash for merged file")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	calculatedHash := fmt.Sprintf("%x", hash.Sum(nil))
	if calculatedHash != task.FileHash {
		logger.WithField("task_id", req.TaskID).Warn("Invalid file hash")
		http.Error(w, "Invalid file hash", http.StatusBadRequest)
		return
	}

	// 返回下載連結
	downloadURL := fmt.Sprintf("/uploads/%s", task.FileName)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"download_url": downloadURL})
	logger.WithField("task_id", req.TaskID).Info("Merged file created")
}

func main() {
	initDB()
	defer db.Close()

	go cleanUpExpiredFiles() // 啟動清理過期臨時檔案的 Goroutine

	http.Handle("/api/createUploadTask", rateLimit(http.HandlerFunc(createUploadTask)))
	http.Handle("/api/uploadChunk", rateLimit(userRateLimit(http.HandlerFunc(uploadChunk))))
	http.Handle("/api/ping", rateLimit(http.HandlerFunc(ping)))
	http.Handle("/api/mergeChunks", rateLimit(http.HandlerFunc(mergeChunks)))
	log.Fatal(http.ListenAndServe(":8080", nil))
}
