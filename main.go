package main

import (
	"bufio"
	"flag"
	"fmt"
	"golang.org/x/term"
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
)

var (
	startTime     time.Time
	terminalWidth int
	isTerminal    bool
)

func main() {
	binName := filepath.Base(os.Args[0])
	resume := flag.Bool("c", false, "Resume download if possible")
	resumeLong := flag.Bool("continue", false, "Resume download if possible")
	urlListFile := flag.String("l", "", "Read URLs from file (one per line)")
	flag.Usage = func() {
		fmt.Printf("Usage: %s [options] <URL1> [URL2 ...]\n\n", binName)
		fmt.Printf("Options:\n")
		fmt.Printf("  -c, --continue\tResume download if file exists\n")
		fmt.Printf("  -l FILE\t\tRead list of URLs from FILE\n\n")
		fmt.Printf("Examples:\n")
		fmt.Printf("  %s https://example.com/file.zip\n", binName)
		fmt.Printf("  %s -c https://a.com/file1.zip https://b.com/file2.zip\n", binName)
		fmt.Printf("  %s -l urls.txt\n", binName)
	}
	flag.Parse()
	if *resumeLong {
		*resume = true
	}

	// Detect terminal
	isTerminal = term.IsTerminal(int(os.Stdout.Fd()))
	width := 80
	if isTerminal {
		if w, _, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
			width = w
		}
	}
	terminalWidth = width

	// Build list of URLs to download
	urls := []string{}
	if *urlListFile != "" {
		f, err := os.Open(*urlListFile)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Cannot open url list file: %v\n", err)
			os.Exit(1)
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" && !strings.HasPrefix(line, "#") {
				urls = append(urls, line)
			}
		}
		f.Close()
		if err := scanner.Err(); err != nil {
			fmt.Fprintf(os.Stderr, "Error reading urls file: %v\n", err)
			os.Exit(1)
		}
	}
	urls = append(urls, flag.Args()...)
	if len(urls) == 0 {
		flag.Usage()
		os.Exit(1)
	}

	// Download each file concurrently
	var wg sync.WaitGroup
	for _, urlStr := range urls {
		wg.Add(1)
		go func(u string) {
			defer wg.Done()
			err := downloadFile(u, *resume)
			if err != nil {
				fmt.Fprintf(os.Stderr, "\nError downloading %s: %v\n", u, err)
			}
		}(urlStr)
	}
	wg.Wait()
}

// downloadFile performs download of a single file
func downloadFile(urlStr string, resume bool) error {
	parsed, err := url.ParseRequestURI(urlStr)
	if err != nil {
		return fmt.Errorf("invalid URL: %v", err)
	}
	filename := filepath.Base(parsed.Path)
	if filename == "" || filename == "/" || filename == "." {
		filename = "downloaded.file"
	}
	// If file exists, and not resume, get unique name
	if !resume {
		filename = getUniqueFilename(filename)
	}
	dir := filepath.Dir(filename)
	if dir != "." {
		os.MkdirAll(dir, os.ModePerm)
	}
	var file *os.File
	var currentSize int64 = 0
	if resume {
		info, err := os.Stat(filename)
		if err == nil {
			file, err = os.OpenFile(filename, os.O_RDWR, 0644)
			if err != nil {
				return fmt.Errorf("cannot open file: %v", err)
			}
			currentSize = info.Size()
		} else {
			file, err = os.Create(filename)
			if err != nil {
				return fmt.Errorf("cannot create file: %v", err)
			}
		}
	} else {
		file, err = os.Create(filename)
		if err != nil {
			return fmt.Errorf("cannot create file: %v", err)
		}
	}
	defer file.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Head(urlStr)
	if err != nil {
		return fmt.Errorf("network error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return fmt.Errorf("server returned status %d", resp.StatusCode)
	}
	total := resp.ContentLength
	if total <= 0 {
		return fmt.Errorf("cannot determine file size")
	}
	if isTerminal {
		if resume && currentSize > 0 {
			fmt.Printf("[*] Resuming %s (%d/%d bytes)\n", filename, currentSize, total)
		} else {
			fmt.Printf("[*] Downloading %s (%d bytes)\n", filename, total)
		}
	} else {
		fmt.Printf("START %s %s %d\n", filename, urlStr, total)
	}

	partSize := total / 2
	var wg sync.WaitGroup
	var downloaded int64 = currentSize
	var errorOccurred atomic.Bool
	errorChan := make(chan error, 2)
	updateChan := make(chan int64, 1000)
	done := make(chan bool)
	startTime = time.Now()
	go func() {
		var lastCurrent int64 = currentSize
		var lastUpdate time.Time
		for {
			select {
			case current, ok := <-updateChan:
				if !ok {
					printProgress(filename, lastCurrent, total, startTime, true)
					if isTerminal {
						fmt.Println()
					}
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
				printProgress(filename, current, total, startTime, false)
			case <-done:
				printProgress(filename, lastCurrent, total, startTime, true)
				if isTerminal {
					fmt.Println()
				}
				return
			}
		}
	}()

	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if errorOccurred.Load() {
				return
			}
			from := int64(i)*partSize + currentSize
			to := from + partSize - 1
			if i == 1 {
				to = total - 1
			}
			req, _ := http.NewRequest("GET", urlStr, nil)
			req.Header.Set("Range", "bytes="+strconv.FormatInt(from, 10)+"-"+strconv.FormatInt(to, 10))
			client := &http.Client{Timeout: 30 * time.Second}
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
			fmt.Fprintf(os.Stderr, "\n%s\n", err)
			hasError = true
		}
	}
	if !hasError {
		time.Sleep(300 * time.Millisecond)
		if isTerminal {
			fmt.Printf("[✓] %s - Download complete!\n", filename)
		} else {
			fmt.Printf("DONE %s\n", filename)
		}
	}
	return nil
}

// getUniqueFilename returns a filename that does not exist by appending .N if needed
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

// printProgress prints progress bar for terminal or log line for non-terminal
func printProgress(filename string, current, total int64, startTime time.Time, flush bool) {
	if total <= 0 {
		return
	}
	percent := int(float64(current) / float64(total) * 100)
	if percent > 100 {
		percent = 100
	}
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
		remaining = formatTime(remainingSeconds)
	} else {
		remaining = "0s"
	}
	baseFilename := filepath.Base(filename)
	if isTerminal {
		// Pretty progress bar
		minBarWidth := 10
		fixedWidth := len(fmt.Sprintf(" %3d%%[", percent)) +
			len(fmt.Sprintf("] %s %s/s eta %s", formatBytes(current), formatSpeed(int64(speed)), remaining))
		availWidth := terminalWidth - fixedWidth
		maxFilenameWidth := 30
		if availWidth < maxFilenameWidth+minBarWidth {
			maxFilenameWidth = availWidth - minBarWidth
			if maxFilenameWidth < 8 {
				maxFilenameWidth = 8
			}
		}
		displayName := truncateFilename(baseFilename, maxFilenameWidth)
		barWidth := availWidth - len(displayName)
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
			displayName, percent, bar, formatBytes(current), formatSpeed(int64(speed)), remaining)
		if flush && percent == 100 {
			fmt.Printf("\n")
		}
	} else {
		// Non-terminal: log line
		fmt.Printf("PROGRESS %s %d/%d %d%% %s/s eta %s\n", baseFilename, current, total, percent, formatSpeed(int64(speed)), remaining)
	}
}
