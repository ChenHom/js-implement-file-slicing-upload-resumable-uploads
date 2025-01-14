package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

var dbOpen bool

func setup(filename string, width, height int) {
	// 設置測試模式環境變量
	os.Setenv("TEST_MODE", "true")
	// 初始化資料庫連接
	if !dbOpen {
		initDB()
		dbOpen = true
	}
	// 生成測試圖片
	generateImage(filename, width, height)
}

func teardown(filename string) {
	// 清除測試模式環境變量
	os.Unsetenv("TEST_MODE")
	// 刪除測試圖片
	if _, err := os.Stat(filename); err == nil {
		if err := os.Remove(filename); err != nil {
			fmt.Printf("Failed to remove %s: %v\n", filename, err)
		}
	}
}

func TestCreateUploadTask(t *testing.T) {
	filename := "test_image_create.png"
	setup(filename, 1024, 128) // 128KB
	defer teardown(filename)

	reqBody := `{"file_name": "test_image_create.png", "file_size": 131072, "total_chunks": 4, "file_hash": "testhash", "chunk_hashes": ["hash1", "hash2", "hash3", "hash4"]}`
	req, err := http.NewRequest("POST", "/api/createUploadTask", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(createUploadTask)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var response map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Errorf("Failed to decode response: %v", err)
	}

	if _, ok := response["task_id"]; !ok {
		t.Errorf("Response does not contain task_id")
	}
}

func TestUploadChunk(t *testing.T) {
	filename := "test_image_upload.png"
	setup(filename, 1024, 128) // 128KB
	defer teardown(filename)

	// Create a dummy upload task
	task := UploadTask{
		TaskID:      "test-task-id",
		FileName:    "test_image_upload.png",
		FileSize:    128 * 1024,
		TotalChunks: 4,
		FileHash:    "testhash",
		ChunkHashes: []string{"hash1", "hash2", "hash3", "hash4"},
	}
	tasks[task.TaskID] = &task

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	writer.WriteField("task_id", task.TaskID)
	writer.WriteField("chunk_index", "0")

	// 讀取測試圖片
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	// 計算測試分片的雜湊值
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	chunkHash := fmt.Sprintf("%x", hash.Sum(nil))

	writer.WriteField("chunk_hash", chunkHash)
	part, err := writer.CreateFormFile("file_chunk", filename)
	if err != nil {
		t.Fatal(err)
	}
	file.Seek(0, 0)
	if _, err := io.Copy(part, file); err != nil {
		t.Fatal(err)
	}
	writer.Close()

	req, err := http.NewRequest("POST", "/api/uploadChunk", body)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(uploadChunk)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
}

func TestMergeChunks(t *testing.T) {
	filename := "test_image_merge.png"
	setup(filename, 1024, 128) // 128KB
	defer teardown(filename)

	// Create a dummy upload task
	task := UploadTask{
		TaskID:        "test-task-id",
		FileName:      "test_image_merge.png",
		FileSize:      128 * 1024,
		TotalChunks:   1,
		ChunkHashes:   []string{"hash1"},
		UploadedChunks: []int{0},
	}

	// 計算合併後文件的雜湊值
	file, err := os.Open(filename)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		t.Fatal(err)
	}
	task.FileHash = fmt.Sprintf("%x", hash.Sum(nil))

	tasks[task.TaskID] = &task

	// Create dummy chunk file
	os.MkdirAll("./uploads", os.ModePerm)
	file.Seek(0, 0)
	chunkFile, err := os.Create("./uploads/test-task-id_0")
	if err != nil {
		t.Fatal(err)
	}
	defer chunkFile.Close()
	if _, err := io.Copy(chunkFile, file); err != nil {
		t.Fatal(err)
	}

	reqBody := `{"task_id": "test-task-id"}`
	req, err := http.NewRequest("POST", "/api/mergeChunks", strings.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	rr := httptest.NewRecorder()
	handler := http.HandlerFunc(mergeChunks)
	handler.ServeHTTP(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var response map[string]string
	if err := json.NewDecoder(rr.Body).Decode(&response); err != nil {
		t.Errorf("Failed to decode response: %v", err)
	}

	if _, ok := response["download_url"]; !ok {
		t.Errorf("Response does not contain download_url")
	}
}

func TestUploadDifferentSizes(t *testing.T) {
	sizes := []struct {
		filename      string
		width, height int
	}{
		{"test_image_128kb.png", 1024, 128},   // 128KB
		{"test_image_1mb.png", 1024, 1024},    // 1MB
		{"test_image_5mb.png", 2048, 2048},    // 5MB
		{"test_image_10mb.png", 4096, 4096},   // 10MB
		{"test_image_25mb.png", 8192, 8192},   // 25MB
	}

	for _, size := range sizes {
		t.Run(size.filename, func(t *testing.T) {
			setup(size.filename, size.width, size.height)
			defer teardown(size.filename)

			TestCreateUploadTask(t)
			TestUploadChunk(t)
			TestMergeChunks(t)
		})
	}
}

// 確保在所有測試完成後關閉資料庫連接
func TestMain(m *testing.M) {
	code := m.Run()
	if dbOpen {
		db.Close()
	}
	os.Exit(code)
}
