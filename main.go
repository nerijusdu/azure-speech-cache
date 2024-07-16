package main

import (
	"bytes"
	"encoding/gob"
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

var c = cache.New(cache.NoExpiration, cache.NoExpiration)
var tempC = cache.New(time.Minute*5, time.Minute*10)
var persist = os.Getenv("PERSIST_CACHE") != "false"

func init() {
	gob.Register(CacheEntry{})
}

func main() {
	http.HandleFunc("/tts", handleTTSRequest)
	http.HandleFunc("/status", handleStatusRequest)
	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	if persist {
		loadCache()
	}

	fmt.Printf("Listening on :%s\n", port)
	log.Fatal(http.ListenAndServe(fmt.Sprintf(":%s", port), nil))
}

func loadCache() {
	file, err := os.Open("cache-data.bin")
	if err != nil {
		log.Println("Cache file not found. Starting with empty cache.")
		return
	}
	defer file.Close()

	decoder := gob.NewDecoder(file)
	var items map[string]cache.Item
	err = decoder.Decode(&items)
	if err != nil {
		log.Fatal(err)
	}

	for key, value := range items {
		c.Set(key, value.Object.(CacheEntry), cache.DefaultExpiration)
	}

	log.Println("Cache loaded from binary file, items count:", c.ItemCount())
}

func saveCache() {
	file, err := os.Create("cache-data.bin")
	if err != nil {
		log.Println("Failed to create cache file", err)
	}
	defer file.Close()

	encoder := gob.NewEncoder(file)
	err = encoder.Encode(c.Items())
	if err != nil {
		log.Println("Failed to save cache", err)
		return
	}

	log.Println("Cache saved to binary file")
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

	if val, ok := tempC.Get(ttsRequest.Text); ok {
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
	headers.Set("X-Microsoft-OutputFormat", "audio-16khz-64kbitrate-mono-mp3")
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

	fmt.Println("received response from azure", resp.Header.Get("X-Envoy-Upstream-Service-Time"), time.Since(start))

	if resp.StatusCode != http.StatusOK {
		http.Error(w, fmt.Sprintf("Azure returned %d", resp.StatusCode), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Transfer-Encoding", "chunked")

	var buffer = &bytes.Buffer{}
	multi := io.MultiWriter(w, buffer)
	io.Copy(multi, resp.Body)
	fmt.Println("copied response to buffer", time.Since(start))

	entry := CacheEntry{
		Audio: buffer.Bytes(),
		Type:  resp.Header.Get("Content-Type"),
	}
	if ttsRequest.ShouldCache {
		c.Set(ttsRequest.Text, entry, cache.NoExpiration)
	} else {
		tempC.Set(ttsRequest.Text, entry, time.Minute*5)
	}

	if persist && ttsRequest.ShouldCache {
		go func() {
			saveCache()
		}()
	}
}
