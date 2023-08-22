package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bricks-cloud/bricksllm/internal/key"
	"github.com/bricks-cloud/bricksllm/internal/logger"
	"github.com/bricks-cloud/bricksllm/internal/provider/openai"
	"github.com/gin-gonic/gin"
)

type rateLimitError interface {
	Error() string
	RateLimit()
}

type expirationError interface {
	Error() string
	Reason() string
}

type keyMemStorage interface {
	GetKey(hash string) *key.ResponseKey
}

type keyStorage interface {
	UpdateKey(id string, uk *key.UpdateKey) (*key.ResponseKey, error)
}

type estimator interface {
	EstimateChatCompletionPromptCost(r *openai.ChatCompletionRequest) (float64, error)
}

type validator interface {
	Validate(k *key.ResponseKey, promptCost float64, model string) error
}

type encrypter interface {
	Encrypt(secret string) string
}

func JSON(c *gin.Context, code int, message string) {
	c.JSON(code, &openai.ChatCompletionErrorResponse{
		Error: &openai.ErrorContent{
			Message: message,
			Code:    code,
		},
	})
}

func getKeyValidator(kms keyMemStorage, e estimator, v validator, ks keyStorage, log logger.Logger, enc encrypter) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		if c == nil || c.Request == nil {
			JSON(c, http.StatusInternalServerError, "[BricksLLM] request is empty")
			c.Abort()
			return
		}

		split := strings.Split(c.Request.Header.Get("Authorization"), "Bearer ")
		if len(split) < 2 || len(split[1]) == 0 {
			JSON(c, http.StatusUnauthorized, "[BricksLLM] bearer token is not present")
			c.Abort()
			return
		}

		apiKey := split[1]
		hash := enc.Encrypt(apiKey)

		getKeyStart := time.Now()
		kc := kms.GetKey(hash)
		if kc == nil {
			JSON(c, http.StatusUnauthorized, "[BricksLLM] api key is not registered")
			c.Abort()
			return
		}
		log.Debugf("get key latency %dms", time.Now().Sub(getKeyStart).Milliseconds())

		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			JSON(c, http.StatusInternalServerError, "[BricksLLM] error when reading the request body")
			c.Abort()
			return
		}

		c.Request.Body = io.NopCloser(bytes.NewReader(body))
		ccr := &openai.ChatCompletionRequest{}
		err = json.Unmarshal(body, ccr)
		if err != nil {
			JSON(c, http.StatusInternalServerError, "[BricksLLM] error when parsing the request body")
			c.Abort()
			return
		}

		estimationStart := time.Now()
		cost, err := e.EstimateChatCompletionPromptCost(ccr)
		if err != nil {
			JSON(c, http.StatusInternalServerError, "[BricksLLM] error when estimating completion prompt cost")
			c.Abort()
			return
		}

		log.Debugf("cost estimation latency %dms", time.Now().Sub(estimationStart).Milliseconds())
		err = v.Validate(kc, cost, ccr.Model)
		if err != nil {
			if _, ok := err.(ValidationError); ok {
				JSON(c, http.StatusUnauthorized, "[BricksLLM] api key has been revoked")
				c.Abort()
				return
			}

			if _, ok := err.(expirationError); ok {
				truePtr := true
				_, err = ks.UpdateKey(kc.KeyId, &key.UpdateKey{
					Revoked: &truePtr,
				})

				if err != nil {
					log.Debugf("error when updating revoking the api key %s: %v", kc.KeyId, err)
				}

				JSON(c, http.StatusUnauthorized, "[BricksLLM] key has expired")
				c.Abort()
				return
			}

			if _, ok := err.(rateLimitError); ok {
				JSON(c, http.StatusTooManyRequests, "[BricksLLM] too many requests")
				c.Abort()
				return
			}

			JSON(c, http.StatusInternalServerError, "[BricksLLM] error when validating the api request")
			c.Abort()
			return
		}

		log.Debugf("key validation latency %dms", time.Now().Sub(start).Milliseconds())
	}
}
