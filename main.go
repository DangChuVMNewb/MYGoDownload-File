package main

import (
    "flag"
    "fmt"
    "io"
    "net/http"
    "os"
    "path/filepath"
    "strconv"
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
    resume := flag.Bool("c", false, "Resume download if possible")
    threads := flag.Int("th", 2, "Number of threads to use (default 2)")
    flag.Parse()

    // Lấy kích thước terminal
    width, _, err := term.GetSize(int(os.Stdout.Fd()))
    if err == nil && width > 0 {
        terminalWidth = width
    } else {
        terminalWidth = 80 // Mặc định nếu không lấy được
    }

    var filename string
    var url string

    if flag.NArg() == 1 {
        url = flag.Arg(0)
        filename = filepath.Base(url)
    } else if flag.NArg() >= 2 {
        filename = flag.Arg(0)
        url = flag.Arg(1)
    } else {
        fmt.Println("Usage: dow [-c] [-th N] <filepath> <URL>")
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
                panic(err)
            }
            currentSize = fileInfo.Size()
        } else {
            file, err = os.Create(filename)
            if err != nil {
                panic(err)
            }
        }
    } else {
        file, err = os.Create(filename)
        if err != nil {
            panic(err)
        }
    }
    defer file.Close()

    resp, err := http.Head(url)
    if err != nil {
        panic(err)
    }

    total := resp.ContentLength
    if *resume && currentSize > 0 {
        fmt.Printf("Resuming download of %s (%d bytes remaining) with %d threads\n", filename, total-currentSize, *threads)
    } else {
        fmt.Printf("Downloading %s (%d bytes) with %d threads\n", filename, total, *threads)
    }

    partSize := total / int64(*threads)
    var wg sync.WaitGroup
    
    atomic.StoreInt64(&downloaded, currentSize)

    updateChan := make(chan int64, 1000)
    defer close(updateChan)
    
    // Goroutine duy nhất để in progress bar
    go func() {
        var lastCurrent int64 = currentSize
        var lastUpdate time.Time
        
        for current := range updateChan {
            now := time.Now()
            // Giới hạn tần suất in - tối đa 10 lần/giây
            if now.Sub(lastUpdate) < 100*time.Millisecond && current < total {
                continue
            }
            lastUpdate = now
            lastCurrent = current
            printProgress(current, total, startTime, filename)
        }
        
        // In lần cuối khi hoàn thành
        printProgress(lastCurrent, total, startTime, filename)
        fmt.Println()
    }()

    for i := 0; i < *threads; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()

            from := int64(i) * partSize + currentSize
            to := from + partSize - 1
            if i == *threads-1 {
                to = total - 1
            }

            req, _ := http.NewRequest("GET", url, nil)
            req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10))
            client := &http.Client{}
            resp, err := client.Do(req)
            if err != nil {
                panic(err)
            }
            defer resp.Body.Close()

            buf := make([]byte, 32*1024)
            pos := from
            for {
                n, err := resp.Body.Read(buf)
                if n > 0 {
                    file.Seek(pos, 0)
                    file.Write(buf[:n])

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
                        panic(err)
                    }
                }
            }
        }(i)
    }

    wg.Wait()
    time.Sleep(200 * time.Millisecond)
    fmt.Printf("Download complete!\n")
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

    // Tính toán tốc độ và thời gian còn lại
    elapsed := time.Since(startTime).Seconds()
    var speed float64
    if elapsed > 0 {
        speed = float64(current) / elapsed
    }

    var remaining string
    if speed > 0 && current < total {
        remainingSeconds := int((float64(total) - float64(current)) / speed)
        remaining = formatTime(remainingSeconds)
    } else {
        remaining = "unknown"
    }

    // Tính toán kích thước các phần tử
    baseFilename := filepath.Base(filename)
    
    // Kích thước tối thiểu cho progress bar
    minBarWidth := 10
    
    // Tính toán chiều rộng khả dụng cho progress bar
    // Định dạng: "filename XX%[bar] X.XXM XXXB/s eta Xs"
    // Ước lượng kích thước các phần cố định
    fixedPartsWidth := len(fmt.Sprintf(" %3d%%[", percent)) + 
        len(fmt.Sprintf("] %s %s/s eta %s", 
            formatBytes(current), 
            formatSpeed(int64(speed)), 
            remaining))
    
    availableWidth := terminalWidth - fixedPartsWidth
    
    // Giới hạn chiều rộng tên file tối đa 30 ký tự
    maxFilenameWidth := 30
    if availableWidth < maxFilenameWidth + minBarWidth {
        maxFilenameWidth = availableWidth - minBarWidth
        if maxFilenameWidth < 8 {
            maxFilenameWidth = 8
        }
    }
    
    // Xử lý tên file
    displayName := truncateFilename(baseFilename, maxFilenameWidth)
    
    // Tính toán chiều rộng progress bar thực tế
    barWidth := availableWidth - len(displayName)
    if barWidth < minBarWidth {
        barWidth = minBarWidth
    }
    
    // Tạo progress bar
    filled := percent * barWidth / 100
    if filled > barWidth {
        filled = barWidth
    }

    var bar string
    if filled == barWidth {
        bar = stringRepeat("=", filled)
    } else if filled > 0 {
        bar = stringRepeat("=", filled) + ">" + stringRepeat(" ", barWidth-filled-1)
    } else {
        bar = stringRepeat(" ", barWidth)
    }

    // In progress bar
    fmt.Printf("\r%s %3d%%[%s] %s %s/s eta %s",
        displayName,
        percent,
        bar,
        formatBytes(current),
        formatSpeed(int64(speed)),
        remaining)
}

func stringRepeat(s string, count int) string {
    if count <= 0 {
        return ""
    }
    res := ""
    for i := 0; i < count; i++ {
        res += s
    }
    return res
}
