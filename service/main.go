package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	_ "github.com/mattn/go-sqlite3"
	"golang.org/x/time/rate"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/rs/cors"
)

type Config struct {
	DBPath           string
	MaxOpenConns     int
	MaxIdleConns     int
	ConnMaxLifetime  time.Duration
	RateLimit        rate.Limit
	BurstLimit       int
	UserRateLimit    rate.Limit
	UserBurstLimit   int
	CleanupInterval  time.Duration
	FileRetention    time.Duration
}

var (
	db   *sql.DB
	mu   sync.Mutex
	config Config
	limiter *rate.Limiter
	userUploadLimits = make(map[string]*rate.Limiter)
	tasks = make(map[string]*UploadTask) // 定義並初始化 tasks 變數
	logger = logrus.New()
	requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds",
			Help:    "Duration of HTTP requests.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"handler", "method"},
	)
)

func init() {
	// 檢查是否在測試模式下運行
	if os.Getenv("TEST_MODE") == "true" {
		logger.SetOutput(io.Discard) // 禁用 logger 的輸出
	} else {
		logger.SetFormatter(&logrus.JSONFormatter{})
		logger.SetLevel(logrus.InfoLevel)
		logger.SetOutput(os.Stdout)
	}

	config = Config{
		DBPath:          getEnv("DB_PATH", "./upload_tasks.sqlite"),
		MaxOpenConns:    getEnvAsInt("DB_MAX_OPEN_CONNS", 10),
		MaxIdleConns:    getEnvAsInt("DB_MAX_IDLE_CONNS", 5),
		ConnMaxLifetime: getEnvAsDuration("DB_CONN_MAX_LIFETIME", time.Hour),
		RateLimit:       rate.Every(getEnvAsDuration("RATE_LIMIT", 100*time.Millisecond)),
		BurstLimit:      getEnvAsInt("BURST_LIMIT", 10),
		UserRateLimit:   rate.Every(getEnvAsDuration("USER_RATE_LIMIT", 200*time.Millisecond)),
		UserBurstLimit:  getEnvAsInt("USER_BURST_LIMIT", 5),
		CleanupInterval: getEnvAsDuration("CLEANUP_INTERVAL", 24*time.Hour),
		FileRetention:   getEnvAsDuration("FILE_RETENTION", 7*24*time.Hour),
	}

	limiter = rate.NewLimiter(config.RateLimit, config.BurstLimit)
	prometheus.MustRegister(requestDuration)
}

func getEnv(key, defaultValue string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return defaultValue
}

func getEnvAsInt(name string, defaultValue int) int {
	valueStr := getEnv(name, "")
	if value, err := strconv.Atoi(valueStr); err == nil {
		return value
	}
	return defaultValue
}

func getEnvAsDuration(name string, defaultValue time.Duration) time.Duration {
	valueStr := getEnv(name, "")
	if value, err := time.ParseDuration(valueStr); err == nil {
		return value
	}
	return defaultValue
}

func initDB() {
	var err error
	db, err = sql.Open("sqlite3", config.DBPath)
	if err != nil {
		logger.Fatal(err)
	}

	// 設置資料庫連接池參數
	db.SetMaxOpenConns(config.MaxOpenConns)
	db.SetMaxIdleConns(config.MaxIdleConns)
	db.SetConnMaxLifetime(config.ConnMaxLifetime)

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
		logger.Fatal(err)
	}
}

type UploadTask struct {
	TaskID        string   `json:"taskId"`
	FileName      string   `json:"fileName"`
	FileSize      int64    `json:"fileSize"`
	TotalChunks   int      `json:"totalChunks"`
	FileHash      string   `json:"fileHash"`
	ChunkHashes   []string `json:"chunkHashes"`
	UploadedChunks []int    `json:"uploadedChunks"`
}

type ChunkUpload struct {
	TaskID    string `json:"taskId"`
	ChunkIndex int    `json:"chunkIndex"`
	ChunkHash  string `json:"chunkHash"`
}

func rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (!limiter.Allow()) {
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func userRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID := r.Header.Get("X-User-ID")
		if userID == "" {
			http.Error(w, "User ID required", http.StatusBadRequest)
			return
		}
		mu.Lock()
		userLimiter, exists := userUploadLimits[userID]
		if (!exists) {
			userLimiter = rate.NewLimiter(config.UserRateLimit, config.UserBurstLimit)
			userUploadLimits[userID] = userLimiter
		}
		mu.Unlock()

		if (!userLimiter.Allow()) {
			http.Error(w, "Too many requests", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func createUploadTask(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var task UploadTask
	if err := json.NewDecoder(r.Body).Decode(&task); err != nil {
		logger.WithError(err).Error("Failed to decode request body")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	task.TaskID = uuid.New().String()

	// 如果 UploadedChunks 為 nil，設置為空數組
	if task.UploadedChunks == nil {
		task.UploadedChunks = []int{}
	}

	chunkHashesJSON, err := json.Marshal(task.ChunkHashes)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal chunk hashes")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	uploadedChunksJSON, err := json.Marshal(task.UploadedChunks)
	if err != nil {
		logger.WithError(err).Error("Failed to marshal uploaded chunks")
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	mu.Lock()
	tasks[task.TaskID] = &task // 將任務儲存在 tasks 中
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
	json.NewEncoder(w).Encode(map[string]interface{}{
		"taskId": task.TaskID,
		"chunkSize": getChunkSize(),
		"totalChunks": task.TotalChunks,
		"fileHash": task.FileHash,
		"chunkHashes": task.ChunkHashes,
		"uploadedChunks": task.UploadedChunks,
	})
}

func uploadChunk(w http.ResponseWriter, r *http.Request) {
    var chunk ChunkUpload

    chunk.TaskID = r.FormValue("taskId")
    chunk.ChunkIndex, _ = strconv.Atoi(r.FormValue("chunkIndex"))
    chunk.ChunkHash = r.FormValue("chunkHash")

    file, _, err := r.FormFile("fileChunk")
    if err != nil {
        logger.WithError(err).Error("Failed to get file chunk")
        http.Error(w, "Failed to get file chunk", http.StatusBadRequest)
        return
    }
    defer file.Close()

    // 驗證請求的合法性，檢查 task_id 和分片編號
    mu.Lock()
    task, exists := tasks[chunk.TaskID]
    mu.Unlock()
    if (!exists) {
        logger.WithField("task_id", chunk.TaskID).Warn("Task not found")
        http.Error(w, "Task not found", http.StatusNotFound)
        return
    }

    // 使用臨時儲存路徑保存分片
    tempFilePath := fmt.Sprintf("./uploads/%s_%d", chunk.TaskID, chunk.ChunkIndex)
    tempFile, err := os.Create(tempFilePath)
    if err != nil {
        logger.WithError(err).Error("Failed to create temp file")
        http.Error(w, "Failed to create temp file", http.StatusInternalServerError)
        return
    }
    defer tempFile.Close()

    _, err = io.Copy(tempFile, file)
    if err != nil {
        logger.WithError(err).Error("Failed to save file chunk")
        http.Error(w, "Failed to save file chunk", http.StatusInternalServerError)
        return
    }

    // 驗證分片的雜湊值，確保資料完整性
    go func() {
        tempFile, err := os.Open(tempFilePath)
        if err != nil {
            logger.WithError(err).Error("Failed to open temp file")
            return
        }
        defer tempFile.Close()

        hash := sha256.New()
        if _, err := io.Copy(hash, tempFile); err != nil {
            logger.WithError(err).Error("Failed to calculate hash")
            return
        }
        calculatedHash := fmt.Sprintf("%x", hash.Sum(nil))
        if calculatedHash != chunk.ChunkHash {
            logger.Errorf("Hash mismatch: expected %s, got %s", chunk.ChunkHash, calculatedHash)
            return
        }

        // 更新資料庫中的分片上傳狀態
        mu.Lock()
        defer mu.Unlock()
        uploadedChunks := append(task.UploadedChunks, chunk.ChunkIndex)
        uploadedChunksJSON, err := json.Marshal(uploadedChunks)
        if err != nil {
            logger.WithError(err).Error("Failed to marshal uploaded chunks")
            return
        }

        _, err = db.Exec(`UPDATE upload_tasks SET uploaded_chunks = ? WHERE task_id = ?`, string(uploadedChunksJSON), chunk.TaskID)
        if err != nil {
            logger.WithError(err).Error("Failed to update database")
            return
        }

        logger.WithFields(logrus.Fields{
            "task_id":    chunk.TaskID,
            "chunk_index": chunk.ChunkIndex,
        }).Info("Uploaded chunk")
    }()

    w.WriteHeader(http.StatusOK)
}

func cleanUpExpiredFiles() {
    for {
        time.Sleep(config.CleanupInterval) // 每 24 小時執行一次清理
        files, err := os.ReadDir("./uploads")
        if (err != nil) {
            logger.Errorf("Failed to read uploads directory: %v", err)
            continue
        }

        for _, file := range files {
            info, err := file.Info()
            if (err != nil) {
                logger.Errorf("Failed to get file info: %v", err)
                continue
            }

            if time.Since(info.ModTime()) > config.FileRetention { // 清理 7 天前的檔案
                err := os.Remove("./uploads/" + file.Name())
                if (err != nil) {
                    logger.Errorf("Failed to remove file: %v", err)
                } else {
                    logger.Infof("Removed expired file: %s", file.Name())
                }
            }
        }
    }
}

func ping(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

func mergeChunks(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req struct {
		TaskID string `json:"taskId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		logger.WithError(err).Error("Failed to decode request body")
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	mu.Lock()
	task, exists := tasks[req.TaskID]
	mu.Unlock()
	if (!exists) {
		logger.WithField("task_id", req.TaskID).Warn("Invalid task_id")
		http.Error(w, "Invalid task_id", http.StatusBadRequest)
		return
	}

	// 打印取出的 task
	logger.WithFields(logrus.Fields{
		"task_id": task.TaskID,
		"file_name": task.FileName,
		"file_size": task.FileSize,
		"total_chunks": task.TotalChunks,
		"file_hash": task.FileHash,
		"chunk_hashes": task.ChunkHashes,
		"uploaded_chunks": task.UploadedChunks,
	}).Info("Fetched task")

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
		if (err != nil) {
			logger.WithError(err).Error("Error removing chunk file")
		}
	}

	// 驗證合併後的檔案雜湊值
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
	json.NewEncoder(w).Encode(map[string]string{"downloadUrl": downloadURL})
	logger.WithField("task_id", req.TaskID).Info("Merged file created")
}

// 定義一個簡單的 middleware 函數
func loggingMiddleware(next http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        start := time.Now()
        logger.WithFields(logrus.Fields{
            "method": r.Method,
            "url":    r.URL.Path,
        }).Info("Incoming request")
        next.ServeHTTP(w, r)
        duration := time.Since(start)
        requestDuration.WithLabelValues(r.URL.Path, r.Method).Observe(duration.Seconds())
        logger.WithFields(logrus.Fields{
            "method":   r.Method,
            "url":      r.URL.Path,
            "duration": duration,
        }).Info("Request processed")
    })
}

func getChunkSize() int {
	// 根據網路狀況設置分片大小
	// 這裡可以根據具體需求進行調整
	return 1 * 1024 * 1024 // 預設 1MB
}

func main() {
	defer func() {
		if r := recover(); r != nil {
			logger.Errorf("Recovered from panic: %v", r)
		}
	}()

	initDB()
	defer db.Close()

	go cleanUpExpiredFiles() // 啟動清理過期臨時檔案的 Goroutine

	// 使用 middleware 包裝處理函數
	http.Handle("/api/createUploadTask", loggingMiddleware(rateLimit(http.HandlerFunc(createUploadTask))))
	http.Handle("/api/uploadChunk", loggingMiddleware(rateLimit(userRateLimit(http.HandlerFunc(uploadChunk)))))
	http.Handle("/api/ping", rateLimit(http.HandlerFunc(ping)))
	http.Handle("/api/mergeChunks", loggingMiddleware(rateLimit(http.HandlerFunc(mergeChunks))))
	http.Handle("/metrics", promhttp.Handler())

	// 添加 CORS 支援
	c := cors.New(cors.Options{
		AllowedOrigins:   []string{"http://127.0.0.1:8081"},
		AllowCredentials: true,
		AllowedMethods:   []string{"GET", "POST", "PUT", "DELETE", "OPTIONS"},
		AllowedHeaders:   []string{"Content-Type", "X-User-ID"},
	})
	handler := c.Handler(http.DefaultServeMux)

	// 捕捉系統信號以釋放資源
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				logger.Errorf("Recovered from panic in signal handler: %v", r)
			}
		}()
		sig := <-sigChan
		logger.Infof("Received signal: %s. Shutting down...", sig)
		db.Close()
		os.Exit(0)
	}()

	logger.Info("Service started on port 8080")
	if err := http.ListenAndServe(":8080", handler); err != nil {
		logger.Fatal(err)
	}
}
