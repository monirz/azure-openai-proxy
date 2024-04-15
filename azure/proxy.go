package azure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"strings"
	"sync"
	"time"

	"github.com/stulzq/azure-openai-proxy/util"

	"github.com/bytedance/sonic"
	"github.com/gin-gonic/gin"
	"github.com/pkg/errors"
)

func ProxyWithConverter(requestConverter RequestConverter) gin.HandlerFunc {
	return func(c *gin.Context) {
		Proxy(c, requestConverter)
	}
}

type DeploymentInfo struct {
	Data   []map[string]interface{} `json:"data"`
	Object string                   `json:"object"`
}

func ModelProxy(c *gin.Context) {
	// Create a channel to receive the results of each request
	results := make(chan []map[string]interface{}, len(ModelDeploymentConfig))

	// Send a request for each deployment in the map
	for _, deployment := range ModelDeploymentConfig {
		go func(deployment DeploymentConfig) {
			// Create the request
			req, err := http.NewRequest(http.MethodGet, deployment.Endpoint+"/openai/deployments?api-version=2022-12-01", nil)
			if err != nil {
				log.Printf("error parsing response body for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}

			// Set the auth header
			req.Header.Set(AuthHeaderKey, deployment.ApiKey)

			// Send the request
			client := &http.Client{}
			resp, err := client.Do(req)
			if err != nil {
				log.Printf("error sending request for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				log.Printf("unexpected status code %d for deployment %s", resp.StatusCode, deployment.DeploymentName)
				results <- nil
				return
			}

			// Read the response body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				log.Printf("error reading response body for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}

			// Parse the response body as JSON
			var deplotmentInfo DeploymentInfo
			err = json.Unmarshal(body, &deplotmentInfo)
			if err != nil {
				log.Printf("error parsing response body for deployment %s: %v", deployment.DeploymentName, err)
				results <- nil
				return
			}
			results <- deplotmentInfo.Data
		}(deployment)
	}

	// Wait for all requests to finish and collect the results
	var allResults []map[string]interface{}
	for i := 0; i < len(ModelDeploymentConfig); i++ {
		result := <-results
		if result != nil {
			allResults = append(allResults, result...)
		}
	}
	var info = DeploymentInfo{Data: allResults, Object: "list"}
	combinedResults, err := json.Marshal(info)
	if err != nil {
		log.Printf("error marshalling results: %v", err)
		util.SendError(c, err)
		return
	}

	// Set the response headers and body
	c.Header("Content-Type", "application/json")
	c.String(http.StatusOK, string(combinedResults))
}

var bufferPool = &sync.Pool{
	New: func() interface{} {
		return &bytes.Buffer{}
	},
}

// Proxy Azure OpenAI
func Proxy(c *gin.Context, requestConverter RequestConverter) {

	if c.Request.Method == http.MethodOptions {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, OPTIONS, POST")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Status(200)
		return
	}

	// preserve request body for error logging
	var buf bytes.Buffer
	tee := io.TeeReader(c.Request.Body, &buf)
	bodyBytes, err := io.ReadAll(tee)
	if err != nil {
		log.Printf("Error reading request body: %v", err)
		return
	}
	c.Request.Body = io.NopCloser(&buf)

	director := func(req *http.Request) {
		if req.Body == nil {
			util.SendError(c, errors.New("request body is empty"))
			return
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewBuffer(body))

		// get model from url params or body
		model := c.Param("model")
		if model == "" {
			_model, err := sonic.Get(body, "model")
			if err != nil {
				util.SendError(c, errors.Wrap(err, "get model error"))
				return
			}
			_modelStr, err := _model.String()
			if err != nil {
				util.SendError(c, errors.Wrap(err, "get model name error"))
				return
			}
			model = _modelStr
		}

		// get deployment from request
		deployment, err := GetDeploymentByModel(model)
		if err != nil {
			util.SendError(c, err)
			return
		}

		// get auth token from header or deployemnt config
		token := deployment.ApiKey
		if token == "" {
			rawToken := req.Header.Get("Authorization")
			token = strings.TrimPrefix(rawToken, "Bearer ")
		}
		if token == "" {
			util.SendError(c, errors.New("token is empty"))
			return
		}
		req.Header.Set(AuthHeaderKey, token)
		req.Header.Del("Authorization")

		req.Header.Set("Transfer-Encoding", "chunked")
		originURL := req.URL.String()
		req, err = requestConverter.Convert(req, deployment)
		if err != nil {
			util.SendError(c, errors.Wrap(err, "convert request error"))
			return
		}
		log.Printf("proxying request [%s] %s -> %s", model, originURL, req.URL.String())
	}

	proxy := &httputil.ReverseProxy{Director: director}
	transport, err := util.NewProxyFromEnv()
	if err != nil {
		util.SendError(c, errors.Wrap(err, "get proxy error"))
		return
	}
	if transport != nil {
		proxy.Transport = transport
	}

	// Get a buffer from the pool
	buffer := bufferPool.Get().(*bytes.Buffer)
	defer bufferPool.Put(buffer)

	// Set up a timer to flush the buffer periodically
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	chunkedWriter := httputil.NewChunkedWriter(c.Writer)
	defer chunkedWriter.Close()

	// Use a buffered channel for backpressure
	flushChan := make(chan []byte, 10) // Adjust the buffer size as needed

	// Start a goroutine pool for flushing the buffers concurrently
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ { // Adjust the number of goroutines as needed
		wg.Add(1)
		go func() {
			defer wg.Done()
			for data := range flushChan {
				_, err := chunkedWriter.Write(data)
				if err != nil {
					log.Printf("Error writing response: %v", err)
					return
				}
			}
		}()
	}

	go func() {
		defer func() {
			close(flushChan) // Signal the end of the channel
			wg.Wait()        // Wait for all goroutines to finish
		}()

		for {
			select {
			case <-ticker.C:
				// Flush the buffer to the client
				data := buffer.Bytes()
				if len(data) > 0 {
					select {
					case flushChan <- data:
						buffer.Reset()
					default:

					}
				}
			case <-c.Writer.CloseNotify():
				// Client disconnected, exit the goroutine
				return
			}
		}
	}()

	// Proxy the request and append the response chunks to the buffer
	proxy.ServeHTTP(&bufferWriterCloser{ResponseWriter: c.Writer, WriteCloser: chunkedWriter, Buffer: buffer}, c.Request)

	// Flush any remaining data in the buffer
	if buffer.Len() > 0 {
		flushChan <- buffer.Bytes()
	}

	// Streaming the response
	if c.Writer.Status() == http.StatusOK {
		log.Println("test-------------")
		// Stream the response from the original server to the client
		_, err := io.Copy(c.Writer, c.Request.Body)
		if err != nil {
			log.Println(err)
		}
	}

	// issue: https://github.com/Chanzhaoyu/chatgpt-web/issues/831
	if c.Writer.Header().Get("Content-Type") == "text/event-stream" {
		if _, err := c.Writer.Write([]byte{'\n'}); err != nil {
			log.Printf("rewrite response error: %v", err)
		}
	}

	if c.Writer.Status() != 200 {
		log.Printf("encountering error with body: %s", string(bodyBytes))
	}
}

func GetDeploymentByModel(model string) (*DeploymentConfig, error) {
	deploymentConfig, exist := ModelDeploymentConfig[model]
	if !exist {
		return nil, errors.New(fmt.Sprintf("deployment config for %s not found", model))
	}
	return &deploymentConfig, nil
}

type bufferWriterCloser struct {
	http.ResponseWriter
	io.WriteCloser
	Buffer *bytes.Buffer
}

func (w *bufferWriterCloser) Write(data []byte) (int, error) {
	n, err := w.WriteCloser.Write(data)
	if err != nil {
		return n, err
	}
	m, err := w.Buffer.Write(data)
	if err != nil {
		return m, err
	}
	return len(data), nil
}
