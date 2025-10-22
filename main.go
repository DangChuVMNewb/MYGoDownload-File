package main

import (
    "flag"
    "fmt"
    "io"
    "net/http"
    "net/url"
    "os"
    "path/filepath"
    "strconv"
    "strings"
    "sync"
    "sync/atomic"
    "time"
    "golang.org/x/term"
)

var (
    lastPercent   int32 = -1
    startTime           = time.Now()
    downloaded    int64
    printMu       sync.Mutex
    terminalWidth int
)

func main() {
    // Đọc biến môi trường
    envThreads := getEnvInt("THR", 2)
    binName := getEnvString("FILE_BIN", "dow")

    resume := flag.Bool("c", false, "Resume download if possible")
    threads := flag.Int("th", envThreads, fmt.Sprintf("Number of threads to use (default %d from THR env var)", envThreads))
    flag.Parse()

    // Lấy kích thước terminal
    width, _, err := term.GetSize(int(os.Stdout.Fd()))
    if err == nil && width > 0 {
        terminalWidth = width
    } else {
        terminalWidth = 80
    }

    var filename string
    var urlStr string

    if flag.NArg() == 1 {
        urlStr = flag.Arg(0)
        filename = filepath.Base(urlStr)
    } else if flag.NArg() >= 2 {
        filename = flag.Arg(0)
        urlStr = flag.Arg(1)
    } else {
        fmt.Printf("Usage: %s [-c] [-th N] <filepath> <URL>\n", binName)
        fmt.Printf("Environment variables:\n")
        fmt.Printf("  THR       Number of threads (default: %d)\n", envThreads)
        fmt.Printf("  FILE_BIN  Binary name (default: %s)\n", binName)
        return
    }

    // Kiểm tra URL hợp lệ
    _, err = url.ParseRequestURI(urlStr)
    if err != nil {
        fmt.Printf("Error: Invalid URL - %v\n", err)
        return
    }

    dir := filepath.Dir(filename)
    if dir != "." {
        os.MkdirAll(dir, os.ModePerm)
    }

    // Xử lý tên file trùng (giống wget)
    originalFilename := filename
    if !*resume {
        filename = getUniqueFilename(filename)
        if filename != originalFilename {
            fmt.Printf("File '%s' already exists, downloading as '%s'\n", originalFilename, filename)
        }
    }

    var file *os.File
    var currentSize int64 = 0

    if *resume {
        fileInfo, err := os.Stat(filename)
        if err == nil {
            file, err = os.OpenFile(filename, os.O_RDWR, 0644)
            if err != nil {
                fmt.Printf("Error: Cannot open file - %v\n", err)
                return
            }
            currentSize = fileInfo.Size()
        } else {
            file, err = os.Create(filename)
            if err != nil {
                fmt.Printf("Error: Cannot create file - %v\n", err)
                return
            }
        }
    } else {
        file, err = os.Create(filename)
        if err != nil {
            fmt.Printf("Error: Cannot create file - %v\n", err)
            return
        }
    }
    defer file.Close()

    // Kiểm tra kết nối mạng và lấy thông tin file
    client := &http.Client{Timeout: 10 * time.Second}
    resp, err := client.Head(urlStr)
    if err != nil {
        fmt.Printf("Error: Network error - %v\n", err)
        return
    }
    defer resp.Body.Close()

    if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
        fmt.Printf("Error: Server returned status %d\n", resp.StatusCode)
        return
    }

    total := resp.ContentLength
    if total <= 0 {
        fmt.Printf("Error: Cannot determine file size\n")
        return
    }

    // Hiển thị thông tin download
    if flag.NArg() >= 2 {
        fmt.Printf("Downloading %s (Save in %s) (%d bytes) with %d threads\n\n", 
            filepath.Base(urlStr), filename, total, *threads)
    } else {
        if *resume && currentSize > 0 {
            fmt.Printf("Resuming download of %s (%d bytes remaining) with %d threads\n\n", 
                filename, total-currentSize, *threads)
        } else {
            fmt.Printf("Downloading %s (%d bytes) with %d threads\n\n", 
                filename, total, *threads)
        }
    }

    partSize := total / int64(*threads)
    var wg sync.WaitGroup
    var errorOccurred atomic.Bool
    errorChan := make(chan error, *threads)
    
    atomic.StoreInt64(&downloaded, currentSize)

    updateChan := make(chan int64, 1000)
    defer close(updateChan)
    
    // Goroutine duy nhất để in progress bar
    done := make(chan bool)
    go func() {
        var lastCurrent int64 = currentSize
        var lastUpdate time.Time
        
        for {
            select {
            case current, ok := <-updateChan:
                if !ok {
                    printProgress(lastCurrent, total, startTime, filename)
                    fmt.Println()
                    return
                }
                
                if errorOccurred.Load() {
                    return
                }
                
                now := time.Now()
                if now.Sub(lastUpdate) < 100*time.Millisecond && current < total {
                    continue
                }
                lastUpdate = now
                lastCurrent = current
                printProgress(current, total, startTime, filename)
            case <-done:
                printProgress(lastCurrent, total, startTime, filename)
                fmt.Println()
                return
            }
        }
    }()

    for i := 0; i < *threads; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()

            if errorOccurred.Load() {
                return
            }

            from := int64(i) * partSize + currentSize
            to := from + partSize - 1
            if i == *threads-1 {
                to = total - 1
            }

            req, err := http.NewRequest("GET", urlStr, nil)
            if err != nil {
                errorChan <- fmt.Errorf("Error creating request: %v", err)
                errorOccurred.Store(true)
                return
            }
            
            req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10))
            
            client := &http.Client{
                Timeout: 30 * time.Second,
            }
            
            resp, err := client.Do(req)
            if err != nil {
                errorChan <- fmt.Errorf("Network error: %v", err)
                errorOccurred.Store(true)
                return
            }
            defer resp.Body.Close()

            if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
                errorChan <- fmt.Errorf("Server error: %d", resp.StatusCode)
                errorOccurred.Store(true)
                return
            }

            buf := make([]byte, 32*1024)
            pos := from
            for {
                if errorOccurred.Load() {
                    return
                }
                
                n, err := resp.Body.Read(buf)
                if n > 0 {
                    file.Seek(pos, 0)
                    _, writeErr := file.Write(buf[:n])
                    if writeErr != nil {
                        errorChan <- fmt.Errorf("Error writing to file: %v", writeErr)
                        errorOccurred.Store(true)
                        return
                    }

                    newDownloaded := atomic.AddInt64(&downloaded, int64(n))
                    select {
                    case updateChan <- newDownloaded:
                    default:
                    }
                    pos += int64(n)
                }
                if err != nil {
                    if err == io.EOF {
                        break
                    } else {
                        errorChan <- fmt.Errorf("Error reading response: %v", err)
                        errorOccurred.Store(true)
                        return
                    }
                }
            }
        }(i)
    }

    go func() {
        wg.Wait()
        close(errorChan)
        done <- true
    }()

    hasError := false
    for err := range errorChan {
        if err != nil {
            fmt.Printf("\n%s\n", err)
            hasError = true
        }
    }

    if !hasError {
        time.Sleep(300 * time.Millisecond)
        fmt.Printf("Download complete!\n")
    }
}

// Hàm đọc biến môi trường số nguyên
func getEnvInt(key string, defaultValue int) int {
    if value, exists := os.LookupEnv(key); exists {
        if intValue, err := strconv.Atoi(value); err == nil && intValue > 0 {
            return intValue
        }
    }
    return defaultValue
}

// Hàm đọc biến môi trường chuỗi
func getEnvString(key string, defaultValue string) string {
    if value, exists := os.LookupEnv(key); exists {
        return value
    }
    return defaultValue
}

// Hàm tạo tên file duy nhất giống wget
func getUniqueFilename(filename string) string {
    if _, err := os.Stat(filename); os.IsNotExist(err) {
        return filename
    }

    ext := filepath.Ext(filename)
    base := strings.TrimSuffix(filename, ext)
    
    counter := 1
    for {
        newFilename := fmt.Sprintf("%s.%d%s", base, counter, ext)
        if _, err := os.Stat(newFilename); os.IsNotExist(err) {
            return newFilename
        }
        counter++
        if counter > 1000 {
            return filename
        }
    }
}

func formatBytes(bytes int64) string {
    if bytes < 1024 {
        return fmt.Sprintf("%dB", bytes)
    } else if bytes < 1024*1024 {
        return fmt.Sprintf("%.1fK", float64(bytes)/1024)
    } else if bytes < 1024*1024*1024 {
        return fmt.Sprintf("%.1fM", float64(bytes)/(1024*1024))
    } else {
        return fmt.Sprintf("%.1fG", float64(bytes)/(1024*1024*1024))
    }
}

func formatSpeed(bytes int64) string {
    if bytes < 1024 {
        return fmt.Sprintf("%dB", bytes)
    } else if bytes < 1024*1024 {
        return fmt.Sprintf("%.0fK", float64(bytes)/1024)
    } else {
        return fmt.Sprintf("%.0fM", float64(bytes)/(1024*1024))
    }
}

func formatTime(seconds int) string {
    if seconds < 0 {
        return "0s"
    }
    if seconds < 60 {
        return fmt.Sprintf("%ds", seconds)
    } else if seconds < 3600 {
        return fmt.Sprintf("%dm%02ds", seconds/60, seconds%60)
    } else {
        return fmt.Sprintf("%dh%02dm", seconds/3600, (seconds%3600)/60)
    }
}

func truncateFilename(filename string, maxWidth int) string {
    if len(filename) <= maxWidth {
        return filename
    }
    
    if maxWidth < 8 {
        return filename[:maxWidth]
    }
    
    head := (maxWidth - 3) / 2
    tail := maxWidth - 3 - head
    
    return filename[:head] + "..." + filename[len(filename)-tail:]
}

func printProgress(current, total int64, startTime time.Time, filename string) {
    printMu.Lock()
    defer printMu.Unlock()

    if total <= 0 {
        return
    }

    percent := int(float64(current) / float64(total) * 100)
    if percent > 100 {
        percent = 100
    }
    
    if int32(percent) == atomic.LoadInt32(&lastPercent) && current < total {
        return
    }
    atomic.StoreInt32(&lastPercent, int32(percent))

    elapsed := time.Since(startTime).Seconds()
    var speed float64
    if elapsed > 0 {
        speed = float64(current) / elapsed
    }

    var remaining string
    if current >= total {
        remaining = "0s"
    } else if speed > 0 {
        remainingSeconds := int((float64(total) - float64(current)) / speed)
        if remainingSeconds < 0 {
            remaining = "0s"
        } else {
            remaining = formatTime(remainingSeconds)
        }
    } else {
        remaining = "0s"
    }

    baseFilename := filepath.Base(filename)
    minBarWidth := 10
    
    fixedPartsWidth := len(fmt.Sprintf(" %3d%%[", percent)) + 
        len(fmt.Sprintf("] %s %s/s eta %s", 
            formatBytes(current), 
            formatSpeed(int64(speed)), 
            remaining))
    
    availableWidth := terminalWidth - fixedPartsWidth
    
    maxFilenameWidth := 30
    if availableWidth < maxFilenameWidth + minBarWidth {
        maxFilenameWidth = availableWidth - minBarWidth
        if maxFilenameWidth < 8 {
            maxFilenameWidth = 8
        }
    }
    
    displayName := truncateFilename(baseFilename, maxFilenameWidth)
    
    barWidth := availableWidth - len(displayName)
    if barWidth < minBarWidth {
        barWidth = minBarWidth
    }
    
    filled := percent * barWidth / 100
    if filled > barWidth {
        filled = barWidth
    }

    var bar string
    if filled == barWidth {
        bar = strings.Repeat("=", filled)
    } else if filled > 0 {
        bar = strings.Repeat("=", filled) + ">" + strings.Repeat(" ", barWidth-filled-1)
    } else {
        bar = strings.Repeat(" ", barWidth)
    }

    fmt.Printf("\r%s %3d%%[%s] %s %s/s eta %s",
        displayName,
        percent,
        bar,
        formatBytes(current),
        formatSpeed(int64(speed)),
        remaining)
}
