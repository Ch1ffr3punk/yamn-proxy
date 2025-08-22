package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/net/proxy"
	"gopkg.in/yaml.v3"
)

// Constants replaced with variables that can be modified
var (
	ProxyAddr       = "127.0.0.1:9050" // Change to 127.0.0.1:1080 if you use the nym-socks5-client
	LocalProxy      = "127.0.0.1:4711"
	ConnectTimeout  = 60 * time.Second
	InitialTimeout  = 10 * time.Second
	IoTimeout       = 300 * time.Second
)

// Config struct for YAML configuration
type Config struct {
	ProxyAddr  string            `yaml:"proxy_addr"`
	LocalProxy string            `yaml:"local_proxy"`
	HttpTargets map[string]string `yaml:"http_targets"`
	SmtpTarget string            `yaml:"smtp_target"`
}

var (
	httpTargets = map[string]string{
		"dummy.tld/pubring.mix": "https://www.harmsk.com/yamn/pubring.mix",
		"dummy.tld/mlist2.txt":  "https://www.harmsk.com/yamn/mlist2.txt",
	}

	smtpTarget = "mailrelay.sec3.net:2525"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.SetOutput(os.Stdout)

	log.Println("=== YAMN Proxy Start ===")

	// Load configuration from YAML file if it exists
	loadConfigFromYAML()

	go func() {
		listener, err := net.Listen("tcp", LocalProxy)
		if err != nil {
			log.Fatal("Proxy error:", err)
		}
		defer listener.Close()
		
		log.Printf("✅ Proxy listening on %s", LocalProxy)

		for {
			conn, err := listener.Accept()
			if err != nil {
				log.Println("⚠️ Accept error:", err)
				continue
			}
			go handleConnection(conn, os.Stdout)
		}
	}()

	exePath, err := os.Executable()
	if err != nil {
		log.Fatal("Could not find executable path:", err)
	}
	yamnPath := filepath.Join(filepath.Dir(exePath), "yamn.exe")

	args := os.Args[1:]

	cmd := exec.Command(yamnPath, args...)
	cmd.Env = append(os.Environ(),
		"HTTP_PROXY=http://"+LocalProxy,
		"NO_PROXY=",
	)

	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Run(); err != nil {
		log.Println("⚠️ yamn failed:", err)
	}
}

// loadConfigFromYAML loads configuration from yamn-proxy.yml if it exists
func loadConfigFromYAML() {
	exePath, err := os.Executable()
	if err != nil {
		log.Println("⚠️ Could not determine executable path:", err)
		return
	}

	configPath := filepath.Join(filepath.Dir(exePath), "yamn-proxy.yml")
	
	// Check if config file exists
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		log.Printf("ℹ️ No config file found at %s, using default values", configPath)
		return
	}

	// Read config file
	data, err := os.ReadFile(configPath)
	if err != nil {
		log.Printf("⚠️ Error reading config file %s: %v", configPath, err)
		return
	}

	// Parse YAML
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		log.Printf("⚠️ Error parsing YAML config %s: %v", configPath, err)
		return
	}

	// Update configuration values
	if config.ProxyAddr != "" {
		ProxyAddr = config.ProxyAddr
		log.Printf("✅ Using proxy address from config: %s", ProxyAddr)
	}

	if config.LocalProxy != "" {
		LocalProxy = config.LocalProxy
		log.Printf("✅ Using local proxy from config: %s", LocalProxy)
	}

	if len(config.HttpTargets) > 0 {
		httpTargets = config.HttpTargets
		log.Printf("✅ Using HTTP targets from config: %d entries", len(httpTargets))
	}

	if config.SmtpTarget != "" {
		smtpTarget = config.SmtpTarget
		log.Printf("✅ Using SMTP target from config: %s", smtpTarget)
	}

	log.Printf("✅ Configuration loaded from %s", configPath)
}

func handleConnection(client net.Conn, log io.Writer) {
	defer client.Close()
	io.WriteString(log, "🔌 New connection\n")

	// Set initial deadline for connection type detection
	client.SetDeadline(time.Now().Add(InitialTimeout))
	defer client.SetDeadline(time.Time{})

	reader := bufio.NewReader(client)
	peek, err := reader.Peek(4)
	if err != nil {
		io.WriteString(log, "📧 Starting SMTP session\n")
		handleSMTP(client, log)
		return
	}

	isHTTP := strings.HasPrefix(string(peek), "GET ") || strings.HasPrefix(string(peek), "POST ") || strings.HasPrefix(string(peek), "HEAD ") || strings.HasPrefix(string(peek), "CONNECT")
	if isHTTP {
		io.WriteString(log, "🌐 Starting HTTP session\n")
		handleHTTP(reader, client, log)
	} else {
		io.WriteString(log, "📧 Non-HTTP request detected, treating as raw TCP (SMTP).\n")
		handleSMTP(client, log)
	}
}

func handleHTTP(reader io.Reader, client net.Conn, log io.Writer) {
	req, err := http.ReadRequest(bufio.NewReader(reader))
	if err != nil {
		io.WriteString(log, "⚠️ HTTP parse error: "+err.Error()+"\n")
		return
	}
	defer req.Body.Close()

	requestedURL := req.Host + req.URL.Path
	
	targetURLString, exists := httpTargets[requestedURL]
	if !exists {
		io.WriteString(log, "❌ No target for: "+requestedURL+"\n")
		return
	}

	io.WriteString(log, fmt.Sprintf("🔀 Routing from %s\n", requestedURL))

	newReq, err := http.NewRequest(req.Method, targetURLString, req.Body)
	if err != nil {
		io.WriteString(log, "⚠️ New request creation error: "+err.Error()+"\n")
		return
	}
	newReq.Header = req.Header.Clone()
	
	Dialer, err := proxy.SOCKS5("tcp", ProxyAddr, nil, proxy.Direct)
	if err != nil {
		io.WriteString(log, "⚠️ Proxy error: "+err.Error()+"\n")
		return
	}
	
	proxyTransport := &http.Transport{
		Dial: Dialer.Dial,
	}

	httpClient := &http.Client{Transport: proxyTransport}

	resp, err := httpClient.Do(newReq)
	if err != nil {
		io.WriteString(log, "⚠️ Request failed: "+err.Error()+"\n")
		return
	}
	defer resp.Body.Close()

	if err := resp.Write(client); err != nil {
		io.WriteString(log, "⚠️ Client write error: "+err.Error()+"\n")
		return
	}

	io.WriteString(log, fmt.Sprintf("✅ Success (%d %s)\n", resp.StatusCode, resp.Status))
}

func handleSMTP(client net.Conn, log io.Writer) {
	// Reset deadline after initial detection
	client.SetDeadline(time.Now().Add(IoTimeout))
	defer client.SetDeadline(time.Time{})

	io.WriteString(log, fmt.Sprintf("📧 Connecting to SMTP target\n"))
	
	// Create Tor dialer with longer timeout
	dialer, err := proxy.SOCKS5("tcp", ProxyAddr, nil, &net.Dialer{
		Timeout:   ConnectTimeout,
		KeepAlive: 30 * time.Second,
	})
	if err != nil {
		io.WriteString(log, "⚠️ SOCKS5 dialer creation error: "+err.Error()+"\n")
		return
	}

	// Try to establish connection
	var target net.Conn
	if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
		ctx, cancel := context.WithTimeout(context.Background(), ConnectTimeout)
		defer cancel()
		
		target, err = contextDialer.DialContext(ctx, "tcp", smtpTarget)
	} else {
		// Fallback for non-context dialer
		target, err = dialer.Dial("tcp", smtpTarget)
	}

	if err != nil {
		io.WriteString(log, "⚠️ Failed to connect to SMTP target via Tor: "+err.Error()+"\n")
		return
	}
	defer target.Close()

	// Set TCP keepalive if possible
	if tcpTarget, ok := target.(*net.TCPConn); ok {
		tcpTarget.SetKeepAlive(true)
		tcpTarget.SetKeepAlivePeriod(30 * time.Second)
	}
	if tcpClient, ok := client.(*net.TCPConn); ok {
		tcpClient.SetKeepAlive(true)
		tcpClient.SetKeepAlivePeriod(30 * time.Second)
	}

	io.WriteString(log, "🔗 Connection established, starting data transfer\n")

	// Setup error channels
	errChan := make(chan error, 2)

	// Client → Target
	go func() {
		_, err := io.Copy(target, client)
		errChan <- err
	}()

	// Target → Client
	go func() {
		_, err := io.Copy(client, target)
		errChan <- err
	}()

	// Wait for first error
	if err := <-errChan; err != nil {
		io.WriteString(log, "⚠️ Connection error: "+err.Error()+"\n")
	}

	io.WriteString(log, "✅ SMTP connection closed\n")
}