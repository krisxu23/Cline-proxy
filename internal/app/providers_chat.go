package app

import (
	"net/http"
	"time"
)

// providerDirectClient 直连上游(默认用于非 Gemini provider)。
var providerDirectClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// providerProxiedClient 走系统代理(用于需要海外出口的 Gemini)。
var providerProxiedClient = &http.Client{
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
		Proxy:               http.ProxyFromEnvironment,
	},
}
