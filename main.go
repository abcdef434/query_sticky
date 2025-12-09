package query_sticky

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"time"

	"gitlab.com/traefik_plugin/query_sticky/internal/redis"
)

// Config the plugin configuration.
type Config struct {
	CookieName     string `json:"cookieName"`
	QueryName      string `json:"queryName"`
	HeaderName     string `json:"headerName,omitempty"`
	CacheKeyPrefix string `json:"cacheKeyPrefix,omitempty"`
	RedisAddr      string `json:"redisAddr,omitempty"`
	RedisDB        uint   `json:"redisDb,omitempty" yaml:"redisDb,omitempty"`
	// RedisPassword holds the password used to AUTH against a redis server, if it
	// is protected by a AUTH
	// if you dont want to put the password in clear text in the config definition
	RedisPassword string `json:"redisPassword,omitempty" yaml:"redisPassword,omitempty"`
	// ConnectionTimeout is the read and write connection timeout to redis.
	// By default it is 2 seconds
	RedisConnectionTimeout int64 `json:"redisConnectionTimeout,omitempty" yaml:"redisConnectionTimeout,omitempty"`
	RedisTTL               int64 `json:"redisTTL,omitempty" yaml:"redisDb,omitempty"` // in minute
}

// CreateConfig creates the default plugin configuration.
func CreateConfig() *Config {
	return &Config{
		CookieName:             "traefik_collabora_sticky",
		RedisAddr:              "localhost:6379",
		RedisConnectionTimeout: 1,
		RedisTTL:               30,
	}
}

type QuerySticky struct {
	next        http.Handler
	Config      *Config
	name        string
	redisClient redis.Client
}

func (c *QuerySticky) ServeHTTP(rw http.ResponseWriter, req *http.Request) {
	fmt.Printf("Incoming request: %s %s\n", req.Method, req.URL)

	// Get the headername
	headerName := c.Config.HeaderName
	if headerName != "" {
		headerValue := req.Header.Get(headerName)

		if headerValue == "" {
			fmt.Println("Missing  header:", headerName)
			c.next.ServeHTTP(rw, req)
			return
		}

		resetCookie(c, req, headerValue)

		hash := md5.Sum([]byte(c.Config.CacheKeyPrefix + headerValue))
		hashStr := hex.EncodeToString(hash[:])

		cookieValue, err := c.redisClient.GetKey(hashStr)
		fmt.Printf("[Redis] Search in redis for %s return %s\n", hashStr, cookieValue)
		if err == nil && cookieValue != "" {
			fmt.Printf("[Redis] Found cookie for %s: %s\n", hashStr, cookieValue)
			req.AddCookie(&http.Cookie{
				Name:     c.Config.CookieName,
				Value:    cookieValue,
				Path:     "/",
				HttpOnly: true,
			})
		}

		for _, ck := range req.Cookies() {
			fmt.Printf("Cookies envoyes au backend - %s = %s", ck.Name, ck.Value)
		}

		detectWebSocketUpgradeAndSkipProcessing(c, rw, req, hashStr, cookieValue)
	}

	// Get the queryparam
	queryName := c.Config.QueryName

	if queryName != "" {
		queryValue := req.URL.Query().Get(queryName)

		if queryValue == "" {
			fmt.Println("No Query found")
			c.next.ServeHTTP(rw, req)
			return
		}

		resetCookie(c, req, queryValue)

		hash := md5.Sum([]byte(c.Config.CacheKeyPrefix + queryValue))
		hashStr := hex.EncodeToString(hash[:])

		cookieValue, err := c.redisClient.GetKey(hashStr)
		fmt.Printf("[Redis] Search in redis for %s return %s\n", hashStr, cookieValue)
		if err == nil && cookieValue != "" {
			fmt.Printf("[Redis] Found cookie for %s: %s\n", hashStr, cookieValue)
			req.AddCookie(&http.Cookie{
				Name:     c.Config.CookieName,
				Value:    cookieValue,
				Path:     "/",
				HttpOnly: true,
			})
		}

		for _, ck := range req.Cookies() {
			fmt.Printf("Cookies envoyés au backend - %s = %s", ck.Name, ck.Value)
		}

		detectWebSocketUpgradeAndSkipProcessing(c, rw, req, hashStr, cookieValue)
	}
}

func detectWebSocketUpgradeAndSkipProcessing(c *QuerySticky, rw http.ResponseWriter, req *http.Request, hashKey string, cookieValue string) {
	// Detect WebSocket upgrade and skip processing
	if strings.ToLower(req.Header.Get("Connection")) == "upgrade" &&
		strings.ToLower(req.Header.Get("Upgrade")) == "websocket" {
		fmt.Println("WebSocket detected")

		req.Header.Set(c.Config.CookieName, cookieValue)
		c.next.ServeHTTP(rw, req)
	} else {

		rec := &responseRecorder{
			ResponseWriter: rw,
			header:         http.Header{},
			body:           &bytes.Buffer{},
		}
		c.next.ServeHTTP(rec, req)
		rw.WriteHeader(rec.statusCode)

		fmt.Printf("resp cookie: %v, len: %v \n", rec.cookies, len(rec.cookies))
		for _, cookie := range rec.cookies {
			if cookie.Name == c.Config.CookieName {
				fmt.Printf("[Redis] update cookie, query: %s, cookie: %s\n", hashKey, cookie.Value)
				_ = c.redisClient.Set(hashKey, cookie.Value, time.Duration(c.Config.RedisTTL)*time.Minute)
			}
			http.SetCookie(rw, cookie)
		}

		rw.Write(rec.body.Bytes())
	}
}
func resetCookie(c *QuerySticky, req *http.Request, queryValue string) {
	cookieHeader := req.Header.Get("Cookie")
	newCookies := ""
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		if !strings.HasPrefix(part, c.Config.CookieName+"=") {
			if newCookies != "" {
				newCookies += "; "
			}
			newCookies += part
		}
	}
	if newCookies != "" {
		fmt.Printf("Set cookie For queryValue %s : %s", queryValue, newCookies)
		req.Header.Set("Cookie", newCookies)
	} else {
		req.Header.Del("Cookie")
	}
	fmt.Printf("Removed existing sticky cookie for custom Query: %s", queryValue)
}

type responseRecorder struct {
	http.ResponseWriter
	header     http.Header
	cookies    []*http.Cookie
	body       *bytes.Buffer
	statusCode int
}

func (r *responseRecorder) Header() http.Header {
	return r.header
}

func (r *responseRecorder) WriteHeader(statusCode int) {
	r.statusCode = statusCode

	// Set-Cookie
	if cookies, ok := r.header["Set-Cookie"]; ok {
		for _, c := range cookies {
			cookie, err := ParseSetCookie(c)
			if err != nil {
				fmt.Printf("failed to parse Set-Cookie, cookie: %v, err: %v", cookie, err)
				continue
			}
			r.cookies = append(r.cookies, cookie)
		}
	}

	r.ResponseWriter.WriteHeader(statusCode)
}

func (r *responseRecorder) Write(b []byte) (int, error) {
	if r.body == nil {
		r.body = &bytes.Buffer{}
	}
	r.body.Write(b)
	return len(b), nil
}

func New(ctx context.Context, next http.Handler, config *Config, name string) (http.Handler, error) {
	goVersion := runtime.Version()
	fmt.Printf("set up StickyHeader plugin, go version: %v, config: %v", goVersion, config)

	if config.RedisAddr == "" {
		config.RedisAddr = "localhost:6379"
	}

	if config.RedisConnectionTimeout < 1 {
		config.RedisConnectionTimeout = 1
	}

	if config.RedisTTL <= 0 {
		config.RedisTTL = 30 // fallback to 30 minutes
	}

	client, err := redis.NewClient(
		config.RedisAddr,
		config.RedisDB,
		config.RedisPassword,
		time.Duration(config.RedisConnectionTimeout)*time.Second,
	)

	if err != nil {
		return nil, fmt.Errorf("unable to create redis client: %v", err)
	}

	return &QuerySticky{
		Config:      config,
		next:        next,
		name:        name,
		redisClient: client,
	}, nil
}
