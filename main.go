package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	_ "net/http/pprof"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/patrickmn/go-cache"
)

var c = cache.New(cache.NoExpiration, time.Minute*10)

func main() {
	http.HandleFunc("/tts", handleTTSRequest)
	http.HandleFunc("/status", handleStatusRequest)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	fmt.Printf("Listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
}

type TTSRequest struct {
	Text        string `json:"text"`
	Language    string `json:"language"`
	Gender      string `json:"gender"`
	Name        string `json:"name"`
	Style       string `json:"style"`
	AzureKey    string `json:"azureKey"`
	AzureRegion string `json:"azureRegion"`
	ShouldCache bool   `json:"shouldCache"`
}

type CacheEntry struct {
	Audio []byte
	Type  string
}

func handleStatusRequest(w http.ResponseWriter, r *http.Request) {
	itemsCount := c.ItemCount()
	occupiedMemory := 0.0

	for key, value := range c.Items() {
		occupiedMemory += float64(len(value.Object.(CacheEntry).Audio))
		occupiedMemory += float64(len(key))
	}

	var m runtime.MemStats
	runtime.ReadMemStats(&m)

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"itemsCount":  itemsCount,
		"cacheMemory": fmt.Sprintf("%f mb", occupiedMemory/1024/1024),
		"alloc":       fmt.Sprintf("%f mb", float64(m.Alloc)/1024/1024),
		"totalAlloc":  fmt.Sprintf("%f mb", float64(m.TotalAlloc)/1024/1024),
		"sys":         fmt.Sprintf("%f mb", float64(m.Sys)/1024/1024),
		"numGC":       m.NumGC,
	})
}

func handleTTSRequest(w http.ResponseWriter, r *http.Request) {
	var ttsRequest TTSRequest
	err := json.NewDecoder(r.Body).Decode(&ttsRequest)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if ttsRequest.AzureKey == "" {
		http.Error(w, "azureKey is required", http.StatusBadRequest)
		return
	}

	if ttsRequest.Text == "" {
		http.Error(w, "text is required", http.StatusBadRequest)
		return
	}

	if ttsRequest.AzureRegion == "" {
		http.Error(w, "azureRegion is required", http.StatusBadRequest)
		return
	}

	if val, ok := c.Get(ttsRequest.Text); ok {
		value := val.(CacheEntry)
		w.Header().Set("Transfer-Encoding", "chunked")
		w.Header().Set("Content-Type", value.Type)
		w.Write(value.Audio)
		return
	}

	azureUrl, _ := url.Parse(fmt.Sprintf("https://%s.tts.speech.microsoft.com/cognitiveservices/v1", ttsRequest.AzureRegion))
	requestBody := fmt.Sprintf(`
      <speak version='1.0' xml:lang='en-US'>
        <voice xml:lang='%s' xml:gender='%s' name='%s' style='%s'>
          <prosody rate='0.8'>
            %s
          </prosody>
        </voice>
      </speak>	
	`, ttsRequest.Language, ttsRequest.Gender, ttsRequest.Name, ttsRequest.Style, ttsRequest.Text)
	headers := make(http.Header)
	headers.Set("Content-Type", "application/ssml+xml")
	headers.Set("X-Microsoft-OutputFormat", "audio-16khz-128kbitrate-mono-mp3")
	headers.Set("Ocp-Apim-Subscription-Key", ttsRequest.AzureKey)
	headers.Set("User-Agent", "node")

	req := &http.Request{
		Method: "POST",
		URL:    azureUrl,
		Body:   io.NopCloser(io.Reader(strings.NewReader(requestBody))),
		Header: headers,
	}

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer resp.Body.Close()

	fmt.Println("received response from azure", time.Since(start))

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Azure returned %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Transfer-Encoding", "chunked")

	var buffer = &bytes.Buffer{}
	io.Copy(buffer, resp.Body)

	exp := time.Minute * 5
	if ttsRequest.ShouldCache {
		exp = cache.NoExpiration
	}

	c.Set(ttsRequest.Text, CacheEntry{
		Audio: buffer.Bytes(),
		Type:  resp.Header.Get("Content-Type"),
	}, exp)

	w.Write(buffer.Bytes())
}
