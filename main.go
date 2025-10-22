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
    scrollOffset  int
    lastScroll    time.Time
)

func main() {
    resume := flag.Bool("c", false, "Resume download if possible")
    threads := flag.Int("th", 2, "Number of threads to use (default 2)")
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
        fmt.Println("Usage: dow [-c] [-th N] <filepath> <URL>")
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

    if *resume && currentSize > 0 {
        fmt.Printf("Resuming download of %s (%d bytes remaining) with %d threads\n\n", filename, total-currentSize, *threads)
    } else {
        fmt.Printf("Downloading %s (%d bytes) with %d threads\n\n", filename, total, *threads)
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
                    // In lần cuối khi hoàn thành
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
                // In lần cuối khi hoàn thành
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
            
            // Client với timeout
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

    // Goroutine để xử lý lỗi
    go func() {
        wg.Wait()
        close(errorChan)
        done <- true
    }()

    // Kiểm tra lỗi
    hasError := false
    for err := range errorChan {
        if err != nil {
            fmt.Printf("\n%s\n", err)
            hasError = true
        }
    }

    if !hasError {
        // Đợi một chút để đảm bảo in lần cuối
        time.Sleep(300 * time.Millisecond)
        fmt.Printf("Download complete!\n")
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
        return "unknown"
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
    
    // Cắt bớt và thêm "..." ở giữa
    head := (maxWidth - 3) / 2
    tail := maxWidth - 3 - head
    
    return filename[:head] + "..." + filename[len(filename)-tail:]
}

func scrollFilename(filename string, maxWidth int) string {
    if len(filename) <= maxWidth {
        return filename
    }
    
    now := time.Now()
    if now.Sub(lastScroll) > 500*time.Millisecond {
        scrollOffset = (scrollOffset + 1) % (len(filename) + 3) // +3 để tạo độ trễ
        lastScroll = now
    }
    
    // Tạo hiệu ứng trượt từ từ
    var result strings.Builder
    for i := 0; i < maxWidth; i++ {
        idx := (scrollOffset + i) % len(filename)
        result.WriteByte(filename[idx])
    }
    
    return result.String()
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
    if speed > 0 && current < total {
        remainingSeconds := int((float64(total) - float64(current)) / speed)
        if remainingSeconds < 0 {
            remaining = "unknown"
        } else {
            remaining = formatTime(remainingSeconds)
        }
    } else {
        remaining = "unknown"
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
    
    // Sử dụng scrollFilename thay vì truncateFilename
    displayName := scrollFilename(baseFilename, maxFilenameWidth)
    
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

    // In progress bar với khoảng cách rõ ràng
    fmt.Printf("\r%s %3d%%[%s] %s %s/s eta %s",
        displayName,
        percent,
        bar,
        formatBytes(current),
        formatSpeed(int64(speed)),
        remaining)
}
