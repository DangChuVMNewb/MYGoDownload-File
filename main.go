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
)

var (
    lastPercent int32 = -1
    startTime        = time.Now()
    printMu    sync.Mutex
)

func main() {
    resume := flag.Bool("c", false, "Resume download if possible")
    threads := flag.Int("th", 2, "Number of threads to use (default 2)")
    flag.Parse()

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
    var err error
    if *resume {
        fileInfo, err := os.Stat(filename)
        if err == nil {
            file, err = os.OpenFile(filename, os.O_RDWR, 0644)
            if err != nil {
                panic(err)
            }
            // Nếu resume, bắt đầu từ cuối file
            currentSize := fileInfo.Size()
            file.Seek(currentSize, 0)
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

    // Lấy thông tin file để resume
    fileInfo, _ := file.Stat()
    currentSize := fileInfo.Size()

    resp, err := http.Head(url)
    if err != nil {
        panic(err)
    }
    
    total := resp.ContentLength
    if *resume && currentSize > 0 {
        total = total - currentSize
        fmt.Printf("Resuming download of %s (%d bytes remaining) with %d threads\n", filename, total, *threads)
    } else {
        fmt.Printf("Downloading %s (%d bytes) with %d threads\n", filename, total, *threads)
    }

    partSize := total / int64(*threads)
    var wg sync.WaitGroup
    var downloaded int64 = currentSize // Bắt đầu từ kích thước hiện tại nếu resume

    for i := 0; i < *threads; i++ {
        wg.Add(1)
        go func(i int) {
            defer wg.Done()
            
            from := int64(i) * partSize + currentSize
            to := from + partSize - 1
            if i == *threads-1 {
                to = total + currentSize - 1 // Đảm bảo tải đủ tổng số byte
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
                    printProgress(newDownloaded, total+currentSize, startTime)
                    
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
    fmt.Printf("\nDownload complete!\n")
}

func formatBytes(bytes int64) string {
    units := []string{"B", "K", "M", "G", "T"}
    var unit string
    value := float64(bytes)
    
    for _, unit = range units {
        if value < 1024.0 || unit == "T" {
            break
        }
        value /= 1024.0
    }
    
    if unit == "B" {
        return fmt.Sprintf("%.0f%s", value, unit)
    } else if value < 10 {
        return fmt.Sprintf("%.1f%s", value, unit)
    }
    return fmt.Sprintf("%.0f%s", value, unit)
}

func formatTime(seconds int) string {
    if seconds < 60 {
        return fmt.Sprintf("%02ds", seconds)
    } else if seconds < 3600 {
        return fmt.Sprintf("%02dm%02ds", seconds/60, seconds%60)
    } else {
        return fmt.Sprintf("%02dh%02dm", seconds/3600, (seconds%3600)/60)
    }
}

func printProgress(current, total int64, startTime time.Time) {
    printMu.Lock()
    defer printMu.Unlock()

    percent := int(float64(current) / float64(total) * 100)
    if int32(percent) == atomic.LoadInt32(&lastPercent) && percent < 100 {
        return
    }
    atomic.StoreInt32(&lastPercent, int32(percent))

    // Tính toán tốc độ và thời gian còn lại
    elapsed := time.Since(startTime).Seconds()
    speed := float64(current) / elapsed
    
    var remaining string
    if speed > 0 {
        remainingSeconds := int((float64(total) - float64(current)) / speed)
        remaining = formatTime(remainingSeconds)
    } else {
        remaining = "--:--"
    }

    // Định dạng giống wget
    barWidth := 50
    filled := percent * barWidth / 100
    
    var bar string
    if filled == barWidth {
        bar = stringRepeat("=", filled)
    } else if filled > 0 {
        bar = stringRepeat("=", filled-1) + ">" + stringRepeat(" ", barWidth-filled)
    } else {
        bar = stringRepeat(" ", barWidth)
    }

    // In progress bar giống wget
    fmt.Printf("\r%s [%s] %d%% %s %s",
        formatBytes(current), 
        bar,
        percent,
        formatBytes(int64(speed)) + "/s",
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
