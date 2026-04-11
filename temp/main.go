package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model    string    `json:"model"`
	Stream   bool      `json:"stream"`
	Messages []Message `json:"messages"`
}

var (
	apiURL      = flag.String("url", "http://127.0.0.1:38080/v1/chat/completions", "api url")
	apiKey      = flag.String("key", "", "api key")
	model       = flag.String("model", "gpt-5.4", "model")
	concurrency = flag.Int("c", 100, "concurrency")
	duration    = flag.Int("t", 10, "duration seconds")
)

var (
	ttftList       []float64
	completionList []float64
	lock           sync.Mutex
	totalRequests  int64
)

func randomPrompt() string {
	letters := "abcdefghijklmnopqrstuvwxyz"
	b := make([]byte, 16)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return "解释这段文本含义: " + string(b)
}

func percentile(arr []float64, p float64) float64 {
	if len(arr) == 0 {
		return 0
	}
	idx := int(float64(len(arr))*p) - 1
	if idx < 0 {
		idx = 0
	}
	return arr[idx]
}

func worker(client *http.Client, stop <-chan struct{}, wg *sync.WaitGroup) {

	defer wg.Done()

	for {

		select {
		case <-stop:
			return
		default:
		}

		reqBody := Request{
			Model:  *model,
			Stream: true,
			Messages: []Message{
				{Role: "user", Content: randomPrompt()},
			},
		}

		data, _ := json.Marshal(reqBody)

		req, err := http.NewRequest("POST", *apiURL, bytes.NewBuffer(data))
		if err != nil {
			continue
		}

		req.Header.Set("Authorization", "Bearer "+*apiKey)
		req.Header.Set("Content-Type", "application/json")

		start := time.Now()
		firstToken := false

		resp, err := client.Do(req)
		if err != nil {
			continue
		}

		reader := bufio.NewReader(resp.Body)

		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				break
			}

			if strings.HasPrefix(line, "data:") {

				if !firstToken && !strings.Contains(line, "[DONE]") {
					ttft := time.Since(start).Seconds()

					lock.Lock()
					ttftList = append(ttftList, ttft)
					lock.Unlock()

					firstToken = true
				}
			}
		}

		resp.Body.Close()

		total := time.Since(start).Seconds()

		lock.Lock()
		completionList = append(completionList, total)
		lock.Unlock()

		atomic.AddInt64(&totalRequests, 1)
	}
}

func main() {

	flag.Parse()

	client := &http.Client{
		Timeout: 120 * time.Second,
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup

	start := time.Now()

	for i := 0; i < *concurrency; i++ {
		wg.Add(1)
		go worker(client, stop, &wg)
	}

	time.Sleep(time.Duration(*duration) * time.Second)
	close(stop)

	wg.Wait()

	elapsed := time.Since(start).Seconds()

	sort.Float64s(ttftList)
	sort.Float64s(completionList)

	fmt.Println("---- Result ----")
	fmt.Println("Duration:", elapsed)
	fmt.Println("Concurrency:", *concurrency)
	fmt.Println("Requests:", totalRequests)
	fmt.Println("QPS:", float64(totalRequests)/elapsed)

	fmt.Println("\nTTFT")
	fmt.Println("p50:", percentile(ttftList, 0.50))
	fmt.Println("p95:", percentile(ttftList, 0.95))
	fmt.Println("p99:", percentile(ttftList, 0.99))

	fmt.Println("\nCompletion")
	fmt.Println("p50:", percentile(completionList, 0.50))
	fmt.Println("p95:", percentile(completionList, 0.95))
	fmt.Println("p99:", percentile(completionList, 0.99))
}