async function calculateSHA256(buffer) {
    const hashBuffer = await crypto.subtle.digest('SHA-256', buffer);
    const hashArray = Array.from(new Uint8Array(hashBuffer));
    const hashHex = hashArray.map(b => b.toString(16).padStart(2, '0')).join('');
    return hashHex;
}

function getChunkSize() {
    // 根據網路狀況設置分片大小
    if (navigator.connection) {
        const connectionType = navigator.connection.effectiveType;
        if (connectionType === '4g') {
            return 5 * 1024 * 1024; // 5MB
        } else if (connectionType === '3g') {
            return 1 * 1024 * 1024; // 1MB
        } else if (connectionType === '2g' || connectionType === 'slow-2g') {
            return 512 * 1024; // 512KB
        }
    }
    return 1 * 1024 * 1024; // 預設 1MB
}

export async function selectFile(file) {
    const { taskId, chunkSize, totalChunks, fileHash, chunkHashes, uploadedChunks } = await createUploadTask(file);
    await uploadChunks(file, { chunkSize, totalChunks, taskId, chunkHashes, uploadedChunks });
}

async function createUploadTask(file) {
    const chunkSize = getChunkSize(); // 動態設置分片大小
    const totalChunks = Math.ceil(file.size / chunkSize);

    // 計算整個檔案的雜湊值
    const fileBuffer = await file.arrayBuffer();
    const fileHash = await calculateSHA256(fileBuffer);

    // 計算每個分片雜湊值
    const chunkHashes = [];
    for (let i = 0; i < totalChunks; i++) {
        const start = i * chunkSize;
        const end = Math.min(file.size, start + chunkSize);
        const chunk = file.slice(start, end);
        const chunkBuffer = await chunk.arrayBuffer();
        const chunkHash = await calculateSHA256(chunkBuffer);
        chunkHashes.push(chunkHash);
    }

    try {
        const response = await fetch('/api/createUploadTask', {
            method: 'POST',
            body: JSON.stringify({
                fileName: file.name,
                fileSize: file.size,
                totalChunks: totalChunks,
                fileHash: fileHash,
                chunkHashes: chunkHashes
            }),
            headers: { 'Content-Type': 'application/json' }
        });
        const { taskId, uploadedChunks } = await response.json();
        // 儲存進度到 localStorage
        localStorage.setItem('uploadProgress', JSON.stringify({ taskId, chunkSize, totalChunks, fileHash, chunkHashes, uploadedChunks }));
        return { taskId, chunkSize, totalChunks, fileHash, chunkHashes, uploadedChunks };
    } catch (error) {
        console.error('初始化上傳失敗', error);
        throw error;
    }
}

// 簡易超時函式，使用 AbortController
async function fetchWithTimeout(url, options, timeout = 5000) {
    const controller = new AbortController();
    const id = setTimeout(() => controller.abort(), timeout);
    try {
        const response = await fetch(url, { ...options, signal: controller.signal });
        clearTimeout(id);
        return response;
    } catch (err) {
        clearTimeout(id);
        throw err;
    }
}

async function uploadChunk(file, taskId, chunkHashes, chunkSize, index, retries = 3) {
    const start = index * chunkSize;
    const end = Math.min(file.size, start + chunkSize);
    const chunk = file.slice(start, end);

    // 建立 FormData
    const formData = new FormData();
    formData.append('taskId', taskId);
    formData.append('chunkIndex', index.toString());
    formData.append('chunkHash', chunkHashes[index]);
    formData.append('fileChunk', chunk);

    // 簡易重試機制
    for (let attempt = 1; attempt <= retries; attempt++) {
        try {
            const response = await fetchWithTimeout('/api/uploadChunk', {
                method: 'POST',
                body: formData
            });
            if (!response.ok) throw new Error(`分片 ${index} 上傳失敗`);
            // 更新本地儲存進度
            const progress = JSON.parse(localStorage.getItem('uploadProgress'));
            progress.uploadedChunks.push(index);
            localStorage.setItem('uploadProgress', JSON.stringify(progress));
            return;
        } catch (error) {
            if (attempt === retries) {
                console.error(`分片 ${index} 重試失敗:`, error);
                throw error;
            }
            // 加入 exponential backoff 機制
            const delay = Math.pow(2, attempt) * 1000; // 指數遞增
            await new Promise(resolve => setTimeout(resolve, delay));
        }
    }
}

function updateProgressBar(completedChunks, totalChunks) {
    const progress = (completedChunks / totalChunks) * 100;
    const progressBar = document.getElementById('uploadProgress');
    if (progressBar) {
        progressBar.value = progress;
    }

    const statusMessage = document.getElementById('statusMessage');
    if (statusMessage) {
        statusMessage.innerText = `正在上傳第 ${completedChunks}/${totalChunks} 個分片`;
    }
}

let isPaused = false;
let pendingChunks = [];
let pingInterval;

function pauseUpload() {
    isPaused = true;
    console.log('上傳已暫停');
}

function resumeUpload() {
    isPaused = false;
    console.log('上傳已恢復');
    processPendingChunks();
}

function monitorNetworkStatus() {
    window.addEventListener('offline', () => {
        console.log('網路中斷');
        pauseUpload();
    });

    window.addEventListener('online', () => {
        console.log('網路恢復');
        resumeUpload();
    });

    // 定期發送 Ping 請求檢測網路狀態
    pingInterval = setInterval(async () => {
        try {
            const response = await fetch('/api/ping');
            if (response.ok && isPaused) {
                console.log('網路恢復');
                resumeUpload();
            }
        } catch (error) {
            if (!isPaused) {
                console.log('網路中斷');
                pauseUpload();
            }
        }
    }, 5000); // 每 5 秒檢測一次
}

async function processPendingChunks() {
    while (pendingChunks.length > 0 && !isPaused) {
        const { file, taskId, chunkHashes, chunkSize, index } = pendingChunks.shift();
        await uploadChunk(file, taskId, chunkHashes, chunkSize, index);
        updateProgressBar(pendingChunks.length, totalChunks);
    }
}

async function requestMerge(taskId) {
    try {
        const response = await fetch('/api/mergeChunks', {
            method: 'POST',
            body: JSON.stringify({ taskId }),
            headers: { 'Content-Type': 'application/json' }
        });
        if (!response.ok) throw new Error('合併分片失敗');
        const { downloadUrl } = await response.json();
        console.log('上傳完成，下載連結:', downloadUrl);
        return downloadUrl;
    } catch (error) {
        console.error('完成上傳失敗', error);
        throw error;
    }
}

export async function uploadChunks(file, { chunkSize, totalChunks, taskId, chunkHashes, uploadedChunks }) {
    monitorNetworkStatus();
    const concurrency = 3; // 設置併發請求數量
    const allIndexes = Array.from({ length: totalChunks }, (_, i) => i).filter(i => !uploadedChunks.includes(i));
    let completedChunks = uploadedChunks.length;

    // 分批次按併發量執行
    while (allIndexes.length > 0) {
        const batchIndexes = allIndexes.splice(0, concurrency);
        await Promise.all(
            batchIndexes.map(async (i) => {
                if (isPaused) {
                    pendingChunks.push({ file, taskId, chunkHashes, chunkSize, index: i });
                } else {
                    await uploadChunk(file, taskId, chunkHashes, chunkSize, i);
                    completedChunks++;
                    updateProgressBar(completedChunks, totalChunks);
                }
            })
        );
    }

    // 清除 Ping 檢測
    clearInterval(pingInterval);

    // 完成上傳，請求合併分片
    const downloadUrl = await requestMerge(taskId);
    return downloadUrl;
}

// ...existing code...
