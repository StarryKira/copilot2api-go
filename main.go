package main

import (
	"flag"
	"fmt"
	"log"
	"sync"

	"copilot-go/config"
	"copilot-go/handler"
	"copilot-go/instance"
	"copilot-go/store"

	"github.com/gin-gonic/gin"
)

func main() {
	webPort := flag.Int("web-port", 3000, "Web console port")
	proxyPort := flag.Int("proxy-port", 4141, "Proxy server port")
	verbose := flag.Bool("verbose", false, "Enable verbose logging")
	autoStart := flag.Bool("auto-start", true, "Auto-start enabled accounts")
	flag.Parse()

	if !*verbose {
		gin.SetMode(gin.ReleaseMode)
	}

	// Ensure data directories exist
	if err := store.EnsurePaths(); err != nil {
		log.Fatalf("Failed to initialize data paths: %v", err)
	}

	// Restore persisted usage stats from disk
	instance.LoadUsage()

	// Load proxy config and apply to HTTP clients
	if proxyCfg, err := store.GetProxyConfig(); err == nil && proxyCfg.ProxyURL != "" {
		config.SetProxyURL(proxyCfg.ProxyURL)
		instance.RebuildHTTPClients()
		log.Printf("Using HTTP proxy: %s", proxyCfg.ProxyURL)
	}

	// Auto-start enabled accounts
	if *autoStart {
		accounts, err := store.GetEnabledAccounts()
		if err != nil {
			log.Printf("Warning: failed to load accounts: %v", err)
		} else {
			for _, account := range accounts {
				go func(a store.Account) {
					if err := instance.StartInstance(a); err != nil {
						log.Printf("Failed to auto-start account %s: %v", a.Name, err)
					}
				}(account)
			}
		}
	}

	var wg sync.WaitGroup
	wg.Add(2)

	// Start Web Console
	go func() {
		defer wg.Done()
		webEngine := gin.New()
		if *verbose {
			webEngine.Use(gin.Logger())
		}
		webEngine.Use(gin.Recovery())

		handler.RegisterConsoleAPI(webEngine, *proxyPort)

		log.Printf("Web Console listening on :%d", *webPort)
		if err := webEngine.Run(fmt.Sprintf(":%d", *webPort)); err != nil {
			log.Fatalf("Web Console failed: %v", err)
		}
	}()

	// Start Proxy
	go func() {
		defer wg.Done()
		proxyEngine := gin.New()
		if *verbose {
			proxyEngine.Use(gin.Logger())
		}
		proxyEngine.Use(gin.Recovery())

		handler.RegisterProxy(proxyEngine)

		log.Printf("Proxy listening on :%d", *proxyPort)
		if err := proxyEngine.Run(fmt.Sprintf(":%d", *proxyPort)); err != nil {
			log.Fatalf("Proxy failed: %v", err)
		}
	}()

	wg.Wait()
}
