# 後端分片上傳功能開發指南 (使用 Go 語言)

本指南詳細說明如何使用 Go 語言實作圖片分片上傳的後端功能，並特別處理高併發場景下的細節。

---

## **功能概述**

### **1. 功能需求**

- 支援接收分片並儲存到伺服器。
- 驗證分片的完整性，確保資料未損壞。
- 管理上傳任務和狀態，支援斷點續傳。
- 在高併發請求下，保持伺服器穩定性和資源效率。
- 提供分片合併功能，生成完整的檔案。

---

## **後端處理流程**

### **1. 建立上傳任務請求**

- **路徑**：`/api/createUploadTask`
- **方法**：`POST`
- **說明**：此請求用於向伺服器建立上傳任務，並獲取任務 ID。請求體包含檔案名稱、檔案大小、總分片數量、檔案雜湊值及每個分片的雜湊值。

1. **API 路由**：
   - 功能：建立一個新的上傳任務，返回唯一的 `task_id`。

2. **處理邏輯**：
   - 接收用戶端請求，包含檔案名稱和大小等元數據。
   - 生成唯一的 `task_id`（可使用 UUID）。
   - 將任務資訊儲存在資料庫中。

3. **關鍵點**：
   - 確保任務資料的唯一性，避免重複建立。
   - 支援分布式儲存（如使用 Redis 或資料庫集群）。

---

### **2. 上傳分片請求**

- **路徑**：`/api/uploadChunk`
- **方法**：`POST`
- **說明**：此請求用於逐片上傳分片資料。每次請求需攜帶分片編號及對應的雜湊值，並將分片資料作為 FormData 上傳。

1. **API 路由**：
   - 功能：接收分片數據並儲存。

2. **處理邏輯**：
   - 驗證請求的合法性，檢查 `task_id` 和分片編號。
   - 使用臨時儲存路徑保存分片。
   - 驗證分片的雜湊值，確保資料完整性。
   - 更新資料庫中的分片上傳狀態。

3. **關鍵點**：
   - 高併發處理：
     - 使用 Goroutines 處理每個請求。
     - 使用 Channel 或 Mutex 保護臨界區（如狀態更新）。
   - 資源管理：
     - 限制每個用戶同時上傳的分片數量。
     - 定期清理過期的臨時檔案。

---

### **3. Ping 請求**

- **路徑**：`/api/ping`
- **方法**：`HEAD`
- **說明**：此請求用於定期檢測網路狀態，僅返回 HTTP 標頭以確認網路連通性，減少不必要的資源消耗。

1. **API 路由**：
   - 功能：檢測伺服器與用戶端之間的網路是否穩定。

2. **處理邏輯**：
   - 接收用戶端的簡單 GET 請求。
   - 返回 HTTP 狀態碼 200 表示連線正常。

---

### **4. 合併分片請求**

- **路徑**：`/api/mergeChunks`
- **方法**：`POST`
- **說明**：當所有分片成功上傳後，此請求用於向伺服器發送請求合併分片。請求體包含任務 ID，伺服器完成合併後返回完整圖片的下載連結或存取 URL。

1. **API 路由**：
   - 功能：合併已完成的分片，生成完整檔案。

2. **處理邏輯**：
   - 驗證 `task_id` 是否有效，檢查所有分片是否上傳完成。
   - 使用檔案流按順序讀取分片並合併。
   - 驗證合併後的檔案雜湊值，確保完整性。
   - 儲存完整檔案，更新資料庫狀態。

3. **關鍵點**：
   - 合併過程：
     - 使用 Buffer 優化讀寫效率，避免記憶體溢出。
   - 高效儲存：
     - 將分片儲存於分布式文件系統（如 MinIO 或 Amazon S3）。

---

## **高併發處理細節**

### **1. 請求處理架構**

1. **Goroutines 與 Worker Pool**：
   - 每個請求啟動一個 Goroutine 處理。
   - 使用 Worker Pool 限制同時處理的請求數量，避免過多 Goroutines 導致資源耗盡。

   範例：

   ```go
   func workerPool(tasks chan Request, results chan Response, workerCount int) {
       var wg sync.WaitGroup

       for i := 0; i < workerCount; i++ {
           wg.Add(1)
           go func() {
               defer wg.Done()
               for task := range tasks {
                   results <- processTask(task)
               }
           }()
       }

       wg.Wait()
       close(results)
   }
   ```

2. **限流與排程**：
   - 使用 Token Bucket 或 Leaky Bucket 限制每秒請求數量。
   - 設定用戶層級的限制，例如每用戶最多 5 個同時上傳任務。

### **2. 鎖與併發安全**

1. **使用 Mutex**：
   - 保護共享資源（如資料庫更新、文件系統操作）。

2. **避免死鎖**：
   - 鎖的範圍應盡可能小，並保持鎖的順序一致。

3. **基於 Redis 的分布式鎖**：
   - 確保在分布式架構下，只有一個節點能操作相同的資源。

### **3. 錯誤恢復與重試機制**

1. **自動重試**：
   - 對於暫時性錯誤（如網路中斷），啟動重試機制，最多重試 3 次。

2. **異常處理**：
   - 使用 Deferred 恢復（Recover）處理 Goroutine 中的恐慌（Panic）。

3. **記錄與通知**：
   - 將錯誤記錄到日誌系統，並對嚴重錯誤發送警報通知。

---

## **注意事項**

1. **清理機制**：
   - 定期刪除過期的任務和分片，避免占用伺服器資源。

2. **高效儲存**：
   - 將分片儲存於分布式儲存系統，減少本地磁碟壓力。

3. **日誌與監控**：
   - 使用結構化日誌記錄請求與錯誤。
   - 部署監控工具（如 Prometheus）監測系統性能。

4. **測試與壓力測試**：

---

## **API 請求參數與回應格式**

### **1. 建立上傳任務請求**

- **路徑**：`/api/createUploadTask`
- **方法**：`POST`
- **請求參數**：
  - `fileName` (string): 檔案名稱
  - `fileSize` (int): 檔案大小
  - `totalChunks` (int): 總分片數量
  - `fileHash` (string): 檔案雜湊值
  - `chunkHashes` (array of string): 每個分片的雜湊值

- **回應格式**：
  - `task_id` (string): 任務 ID

- **請求範例**：

  ```json
  {
    "fileName": "example.jpg",
    "fileSize": 1024000,
    "totalChunks": 10,
    "fileHash": "abc123",
    "chunkHashes": ["hash1", "hash2", "hash3", "hash4", "hash5", "hash6", "hash7", "hash8", "hash9", "hash10"]
  }
  ```

- **回應範例**：

  ```json
  {
    "task_id": "unique-task-id"
  }
  ```

### **2. 上傳分片請求**

- **路徑**：`/api/uploadChunk`
- **方法**：`POST`
- **請求參數**：
  - `task_id` (string): 任務 ID
  - `chunk_index` (int): 分片編號
  - `chunk_hash` (string): 分片雜湊值
  - `file_chunk` (file): 分片檔案

- **回應格式**：
  - 無

- **請求範例**：

  ```http
  POST /api/uploadChunk HTTP/1.1
  Content-Type: multipart/form-data; boundary=---011000010111000001101001

  ---011000010111000001101001
  Content-Disposition: form-data; name="task_id"

  unique-task-id
  ---011000010111000001101001
  Content-Disposition: form-data; name="chunk_index"

  0
  ---011000010111000001101001
  Content-Disposition: form-data; name="chunk_hash"

  hash1
  ---011000010111000001101001
  Content-Disposition: form-data; name="file_chunk"; filename="chunk0"
  Content-Type: application/octet-stream

  (binary data)
  ---011000010111000001101001--
  ```

### **3. Ping 請求**

- **路徑**：`/api/ping`
- **方法**：`HEAD`
- **請求參數**：
  - 無

- **回應格式**：
  - 無

### **4. 合併分片請求**

- **路徑**：`/api/mergeChunks`
- **方法**：`POST`
- **請求參數**：
  - `task_id` (string): 任務 ID

- **回應格式**：
  - `download_url` (string): 完整檔案的下載連結

- **請求範例**：

  ```json
  {
    "task_id": "unique-task-id"
  }
  ```

- **回應範例**：

  ```json
  {
    "download_url": "/uploads/example.jpg"
  }
  ```
