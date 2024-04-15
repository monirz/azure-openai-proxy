package azure

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"

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

// Proxy Azure OpenAI
func Proxy(c *gin.Context, requestConverter RequestConverter) {
	if c.Request.Method == http.MethodOptions {
		c.Header("Access-Control-Allow-Origin", "*")
		c.Header("Access-Control-Allow-Methods", "GET, OPTIONS, POST")
		c.Header("Access-Control-Allow-Headers", "Authorization, Content-Type")
		c.Status(200)
		return
	}

	// Check if the request body is empty
	if c.Request.Body == nil {
		util.SendError(c, errors.New("request body is empty"))
		return
	}

	// Read the request body
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		util.SendError(c, errors.Wrap(err, "error reading request body"))
		return
	}
	defer c.Request.Body.Close()

	// Create a new request object with the original request's properties
	req := c.Request.WithContext(c.Request.Context())
	req.Body = io.NopCloser(bytes.NewReader(body))

	// Get model from URL params or body
	model := c.Param("model")
	if model == "" {
		_model, err := sonic.Get(body, "model")
		if err != nil {
			util.SendError(c, errors.Wrap(err, "get model error"))
			return
		}
		model, err = _model.String()
		if err != nil {
			util.SendError(c, errors.Wrap(err, "get model name error"))
			return
		}
	}

	// Get deployment by model
	deployment, err := GetDeploymentByModel(model)
	if err != nil {
		util.SendError(c, err)
		return
	}

	// Get auth token from header or deployment config
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

	// Convert request using the request converter
	req, err = requestConverter.Convert(req, deployment)
	if err != nil {
		util.SendError(c, errors.Wrap(err, "convert request error"))
		return
	}

	// Log the proxying request
	log.Printf("proxying request [%s] %s -> %s", model, c.Request.URL.String(), req.URL.String())

	// Forward the request to the target URL
	targetURL := req.URL.String()
	resp, err := forwardRequest(req, targetURL)
	if err != nil {
		util.SendError(c, errors.Wrap(err, "forward request error"))
		return
	}
	defer resp.Body.Close()

	// Copy the response headers from the target to the client
	for key, values := range resp.Header {
		for _, value := range values {
			c.Header(key, value)
		}
	}

	c.Status(resp.StatusCode)

	// Set the status code of the response
	// Get the client's response writer
	clientWriter := c.Writer

	// Stream the response body from the target to the client
	buf := make([]byte, 1024)
	for {
		n, err := resp.Body.Read(buf)
		if err != nil {
			if err != io.EOF {
				log.Printf("Error reading response body: %v", err)
			}
			break
		}

		// Write the chunk of data to the client
		_, err = clientWriter.Write(buf[:n])
		if err != nil {
			log.Printf("Error writing response body to client: %v", err)
			break
		}

		// Flush the response writer to ensure data is sent immediately
		clientWriter.Flush()
	}

	// issue: https://github.com/Chanzhaoyu/chatgpt-web/issues/831
	if c.Writer.Header().Get("Content-Type") == "text/event-stream" {
		log.Println("Content-Type: event-stream")
		if _, err := c.Writer.Write([]byte{'\n'}); err != nil {
			log.Printf("rewrite response error: %v", err)
		}
	}

	if c.Writer.Status() != 200 {
		log.Printf("encountering error with body: %s", string(body))
	}
}

func forwardRequest(req *http.Request, targetURL string) (*http.Response, error) {
	// Create a new HTTP client
	client := &http.Client{}

	// Create a new request to the target URL
	targetReq, err := http.NewRequest(req.Method, targetURL, req.Body)
	if err != nil {
		return nil, err
	}

	// Set headers and other properties from the original request
	targetReq.Header = req.Header

	// Perform the proxy request
	resp, err := client.Do(targetReq)
	if err != nil {
		return nil, err
	}

	return resp, nil
}

// StreamingWriter is a wrapper to handle streaming response bodies
type customResponseWriter struct {
	http.ResponseWriter
	writer io.Writer
}

func (w *customResponseWriter) Write(p []byte) (int, error) {
	written, err := w.ResponseWriter.Write(p)
	if err == nil {
		w.writer.Write(p)
	}
	return written, err
}

func GetDeploymentByModel(model string) (*DeploymentConfig, error) {
	deploymentConfig, exist := ModelDeploymentConfig[model]
	if !exist {
		return nil, errors.New(fmt.Sprintf("deployment config for %s not found", model))
	}
	return &deploymentConfig, nil
}
