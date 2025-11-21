package main

import (
	"bufio"
	"context"

	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"strconv"

	"github.com/xtaci/smux"
	"crypto/rand"
	"encoding/base64"
	"embed"
	"go.etcd.io/bbolt"
)

 //go:embed static/**
 var staticFS embed.FS

func serveEmbeddedFile(w http.ResponseWriter, path string) {
	data, err := staticFS.ReadFile(path)
	if err != nil {
		http.Error(w, "Not Found", http.StatusNotFound)
		return
	}
	ct := "application/octet-stream"
	switch {
	case strings.HasSuffix(path, ".html"):
		ct = "text/html; charset=utf-8"
	case strings.HasSuffix(path, ".css"):
		ct = "text/css; charset=utf-8"
	case strings.HasSuffix(path, ".js"):
		ct = "application/javascript; charset=utf-8"
	case strings.HasSuffix(path, ".json"):
		ct = "application/json; charset=utf-8"
	case strings.HasSuffix(path, ".svg"):
		ct = "image/svg+xml"
	case strings.HasSuffix(path, ".png"):
		ct = "image/png"
	case strings.HasSuffix(path, ".jpg") || strings.HasSuffix(path, ".jpeg"):
		ct = "image/jpeg"
	case strings.HasSuffix(path, ".ico"):
		ct = "image/x-icon"
	case strings.HasSuffix(path, ".woff"):
		ct = "font/woff"
	case strings.HasSuffix(path, ".woff2"):
		ct = "font/woff2"
	case strings.HasSuffix(path, ".ttf"):
		ct = "font/ttf"
	case strings.HasSuffix(path, ".map"):
		ct = "application/json; charset=utf-8"
	}
	// 缓存策略：HTML 不缓存，其它静态资源缓存 1 天
	if strings.HasSuffix(path, ".html") {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=86400")
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// Registration 客户端注册信息，新增 Token 字段
type Registration struct {
	ClientID string `json:"client_id"`
	Ports    []int  `json:"ports"`
	Token    string `json:"token,omitempty"`
}

// PortResult 每个请求端口的注册结果
type PortResult struct {
	Port   int    `json:"port"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// RegResult 注册结果
type RegResult struct {
	Results []PortResult `json:"results"`
}

// ClientInfo 客户端信息
type ClientInfo struct {
	ID        string
	Token     string
	Session   *smux.Session
	ports     []int
	listeners map[int]net.Listener
	cancel    context.CancelFunc
	mu        sync.Mutex
}

// 全局状态
var (
	portMap   = make(map[int]*ClientInfo)
	portMapMu sync.RWMutex

	// token -> allowed client id (optional). If value empty, token is allowed for any client id.
	allowedTokens   = make(map[string]string)
	allowedTokensMu sync.RWMutex

	serverAddr string

	wg sync.WaitGroup // 等待所有 accept / handler goroutine 完成
)

var (
	httpAddr            string
	startTime           time.Time
	activeClients       int64
	totalClients        int64
	activePublicConns   int64
	totalPublicAccepted int64
	portBindings        int64

	// 访问判定阈值（可配置）
	accessMinBytes int64        // 默认 4096
	accessMinDur   time.Duration // 默认 1s

	recentMu     sync.Mutex
	recentEvents []EventItem

	// 授权数据库与会话
	db             *bbolt.DB
	dbPath         string
	adminPass      string
	adminPassword  string
	allowedTokenPorts   map[string][]int
	allowedTokenPortsMu sync.RWMutex

	sessionMu sync.Mutex
	sessions  = make(map[string]time.Time) // sessionToken -> 过期时间
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Println("参考：server -server :7000 -http :7001 -adminpass 密码（留空则随机生成）")

	flag.StringVar(&serverAddr, "server", ":7000", "服务端监听地址 (eg :7000)")
	flag.StringVar(&httpAddr, "http", ":7001", "监控 HTTP 地址 (eg :7001)")
	// 新增数据库与管理密码参数
	flag.StringVar(&dbPath, "db", "ftcp.db", "授权数据库文件路径 (bbolt)")
	flag.StringVar(&adminPass, "adminpass", "", "监控页面登录密码（留空则随机生成）")
	// 访问判定参数（仅统计真实访问）
	var accessMinMs int
	flag.Int64Var(&accessMinBytes, "access_min_bytes", 4096, "统计为访问所需的累计字节阈值（双向总字节）")
	flag.IntVar(&accessMinMs, "access_min_ms", 1000, "统计为访问所需的会话持续时间（毫秒）")

	flag.Parse()
	// 初始化访问判定持续时间
	if accessMinMs <= 0 { accessMinMs = 1000 }
	if accessMinBytes <= 0 { accessMinBytes = 4096 }
	accessMinDur = time.Duration(accessMinMs) * time.Millisecond

	startTime = time.Now()

	// 初始化数据库与加载授权
	var err error
	db, err = bbolt.Open(dbPath, 0600, &bbolt.Options{Timeout: 1 * time.Second})
	if err != nil {
		log.Fatalf("打开数据库失败: %v", err)
	}
	if err := initDB(); err != nil {
		log.Fatalf("初始化数据库失败: %v", err)
	}
	if err := loadTokensFromDB(); err != nil {
		log.Fatalf("加载授权失败: %v", err)
	}

	// 初始化管理密码
	adminPassword = strings.TrimSpace(adminPass)
	if adminPassword == "" {
		adminPassword = genRandomPassword(16)
	}
	log.Printf("监控页面登录密码: %s", adminPassword)

	// 认证仅来源于数据库，可在监控页登录后管理授权

	// 捕获中断信号，优雅退出
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)

	ln, err := net.Listen("tcp", serverAddr)
	if err != nil {
		log.Fatalf("监听 %s 失败: %v", serverAddr, err)
	}
	log.Printf("服务端正在监听控制端口 %s", serverAddr)
	go startHTTPServer()

	// accept loop，接收到退出信号则关闭 listener 并等待 goroutine 退出
	go func() {
		<-sigs
		log.Println("收到退出信号，开始优雅关闭...")
		_ = ln.Close() // 触发 Accept 返回错误
		// 取消所有客户端
		cleanupAllClients()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			// listener 被关闭导致 Accept 失败，退出
			if opErr, ok := err.(*net.OpError); ok && opErr.Err.Error() == "use of closed network connection" {
				log.Println("Listener 已关闭，停止接受新连接")
			} else {
				log.Printf("Accept 错误: %v", err)
			}
			break
		}
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			safeHandleConn(c)
		}(conn)
	}

	// 等待所有 goroutine 结束
	log.Println("等待正在处理的连接结束...")
	wg.Wait()
	log.Println("服务端已优雅退出")
}



// safeHandleConn 在 handler 外层 recover，避免单个 panic 崩溃服务
func safeHandleConn(conn net.Conn) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("连接处理发生 panic: %v", r)
		}
	}()
	handleConn(conn)
}

// handleConn 处理客户端连接
func handleConn(conn net.Conn) {
	defer conn.Close()
	remote := conn.RemoteAddr().String()
	log.Printf("[+] 来自 %s 的新连接", remote)

	// 在此连接上创建 smux 服务端会话（启用 KeepAlive）
	cfg := smux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	session, err := smux.Server(conn, cfg)
	if err != nil {
		log.Printf("[%s] smux 启动失败: %v", remote, err)
		return
	}
	log.Printf("[%s] 会话已创建", remote)

	// 接受第一个流，该流必须是注册 JSON 行
	regStream, err := session.AcceptStream()
	if err != nil {
		log.Printf("[%s] 接收注册流失败: %v", remote, err)
		_ = session.Close()
		return
	}

	// 读取注册行 (JSON + \n)
	reader := bufio.NewReader(regStream)
	line, err := reader.ReadString('\n')
	if err != nil {
		log.Printf("[%s] 读取注册信息失败: %v", remote, err)
		regStream.Close()
		session.Close()
		return
	}
	var reg Registration
	if err := json.Unmarshal([]byte(line), &reg); err != nil {
		log.Printf("[%s] 无效的注册 JSON: %v", remote, err)
		resp := RegResult{Results: []PortResult{{Port: 0, OK: false, Reason: "无效的注册 JSON"}}}
		sendJSONLine(regStream, resp)
		regStream.Close()
		session.Close()
		return
	}
	log.Printf("[%s] 注册信息: client_id=%s ports=%v token=%s", remote, reg.ClientID, reg.Ports, maskToken(reg.Token))

	// token 校验
	if !validateToken(reg.Token, reg.ClientID) {
		log.Printf("[%s] token 校验失败: client_id=%s token=%s", remote, reg.ClientID, maskToken(reg.Token))
		// 返回所有端口都失败
		var rs []PortResult
		for _, p := range reg.Ports {
			rs = append(rs, PortResult{Port: p, OK: false, Reason: "token 验证失败"})
		}
		sendJSONLine(regStream, RegResult{Results: rs})
		for _, p := range reg.Ports {
			appendRecentEvent("error", fmt.Sprintf("[注册失败] 公网 %d : token 验证失败", p), reg.ClientID)
		}
		regStream.Close()
		session.Close()
		return
	}


	// 创建客户端信息和用于清理的上下文
	ctx, cancel := context.WithCancel(context.Background())
	ci := &ClientInfo{
		ID:        reg.ClientID,
		Token:     reg.Token,
		Session:   session,
		ports:     []int{},
		listeners: make(map[int]net.Listener),
		cancel:    cancel,
	}

	// 尝试绑定每个请求的端口
	var results []PortResult
	for _, p := range reg.Ports {
		if p <= 0 || p > 65535 {
			results = append(results, PortResult{Port: p, OK: false, Reason: "无效端口"})
			appendRecentEvent("error", fmt.Sprintf("[注册失败] 公网 %d : 无效端口", p), reg.ClientID)
			continue
		}
		portMapMu.Lock()
		if existing, ok := portMap[p]; ok {
			results = append(results, PortResult{Port: p, OK: false, Reason: fmt.Sprintf("端口已被客户端 %s 占用", existing.ID)})
			appendRecentEvent("error", fmt.Sprintf("[注册失败] 公网 %d : 端口已被客户端 %s 占用", p, existing.ID), reg.ClientID)
			portMapMu.Unlock()
			continue
		}
		// 数据库端口授权校验：若该 token 绑定了端口列表，则只允许列表内端口；为空表示不限
		if !isPortAllowedForToken(reg.Token, p) {
			results = append(results, PortResult{Port: p, OK: false, Reason: "端口未授权"})
			appendRecentEvent("error", fmt.Sprintf("[注册失败] 公网 %d : 端口未授权", p), reg.ClientID)
			portMapMu.Unlock()
			continue
		}

		lnAddr := fmt.Sprintf(":%d", p)
		ln, err := net.Listen("tcp", lnAddr)
		if err != nil {
			results = append(results, PortResult{Port: p, OK: false, Reason: err.Error()})
			appendRecentEvent("error", fmt.Sprintf("[注册失败] 公网 %d : %v", p, err), reg.ClientID)
			portMapMu.Unlock()
			continue
		}
		// 成功：存储并启动接受循环
		portMap[p] = ci
		ci.ports = append(ci.ports, p)
		ci.listeners[p] = ln
		results = append(results, PortResult{Port: p, OK: true})
		portMapMu.Unlock()

		// 启动监听 loop
		wg.Add(1)
		go func(ln net.Listener, port int, client *ClientInfo) {
			defer wg.Done()
			acceptPublicLoop(ctx, ln, port, client)
		}(ln, p, ci)

		log.Printf("[%s] 已为客户端 %s 绑定公网端口 %d", remote, reg.ClientID, p)
		atomic.AddInt64(&portBindings, 1)
	}

	// 将注册结果发送回客户端，并根据结果决定是否保持会话
	resp := RegResult{Results: results}
	if err := sendJSONLine(regStream, resp); err != nil {
		log.Printf("[%s] 发送注册结果失败: %v", remote, err)
	}
	regStream.Close()
	// 仅当所有端口均成功时计入活跃客户端；否则要求客户端重连
	allOK := true
	for _, r := range results { if !r.OK { allOK = false; break } }
	if !allOK {
		_ = session.Close()
		return
	}
	// 累计客户端仅在注册成功时加计
	atomic.AddInt64(&totalClients, 1)
	atomic.AddInt64(&activeClients, 1)
	appendRecentEvent("info", fmt.Sprintf("注册成功: client_id=%s 远端=%s 绑定端口=%v", reg.ClientID, remote, reg.Ports), reg.ClientID)

	// 持续读取流以检测会话关闭，并丢弃任何额外的流
	for {
		s, err := session.AcceptStream()
		if err != nil {
			// 会话已关闭或发生错误
			if err == io.EOF {
				appendRecentEvent("warn", "客户端断开: EOF", reg.ClientID)
			} else {
				appendRecentEvent("warn", fmt.Sprintf("会话结束: %v", err), reg.ClientID)
			}
			log.Printf("[%s] 会话结束: %v", remote, err)
			break
		}
		// 不期望额外流，丢弃数据
		wg.Add(1)
		go func(ss *smux.Stream) {
			defer wg.Done()
			io.Copy(io.Discard, ss)
			ss.Close()
		}(s)
	}

	// 会话结束时进行清理
	log.Printf("[%s] 正在清理客户端 %s", remote, reg.ClientID)
	cancel()
	ci.mu.Lock()
	for p, ln := range ci.listeners {
		if ln != nil {
			_ = ln.Close()
		}
		delete(ci.listeners, p)
		portMapMu.Lock()
		delete(portMap, p)
		portMapMu.Unlock()
	}
	ci.mu.Unlock()
	_ = ci.Session.Close()
	atomic.AddInt64(&activeClients, -1)
}

// validateToken 检查 token 是否允许；如果 allowedTokens 中 value 非空，要求 clientID 匹配
func validateToken(token, clientID string) bool {
	// 从数据库预载的内存映射校验：token 存在且 clientID 匹配（空 clientID 表示不限）
	allowedTokensMu.RLock()
	expectedID, ok := allowedTokens[token]
	allowedTokensMu.RUnlock()
	if !ok {
		return false
	}
	if expectedID == "" {
		return true
	}
	return expectedID == clientID
}

func maskToken(t string) string {
	if t == "" {
		return "<nil>"
	}
	if len(t) <= 4 {
		return "****"
	}
	return "****" + t[len(t)-4:]
}

// acceptPublicLoop 接受公网连接的循环
func acceptPublicLoop(ctx context.Context, ln net.Listener, publicPort int, ci *ClientInfo) {
	defer ln.Close()
	log.Printf("[公网:%d] 监听器已启动", publicPort)
	for {
		userConn, err := ln.Accept()
		if err != nil {
			select {
			case <-ctx.Done():
				return
			default:
			}
			log.Printf("[公网:%d] 接受连接错误: %v", publicPort, err)
			continue
		}
		// 每个公网连接一个 handler（统计在实际传输时进行）
		wg.Add(1)
		go func(c net.Conn) {
			defer wg.Done()
			handlePublicConn(c, publicPort, ci)
		}(userConn)
	}
}

// handlePublicConn 处理公网连接
func handlePublicConn(userConn net.Conn, publicPort int, ci *ClientInfo) {
	defer userConn.Close()

	// 向客户端打开一个流
	stream, err := ci.Session.OpenStream()
	if err != nil {
		log.Printf("[公网:%d] 向客户端 %s 打开流失败: %v", publicPort, ci.ID, err)
		return
	}
	// 写入头部 JSON 告诉客户端此流对应的端口
	header := map[string]int{"port": publicPort}
	if err := sendJSONLine(stream, header); err != nil {
		log.Printf("[公网:%d] 写入头部信息失败: %v", publicPort, err)
		stream.Close()
		return
	}

	// 统计策略：
	// - 活跃公网连接：在成功写入转发头部后立即加计；连接结束时减计（用于实时在线展示）
	// - 累计连接：仅当双向均有数据且总字节 >= accessMinBytes 且持续时间 >= accessMinDur 时加计，过滤心跳/探测
	start := time.Now()
	var tx1, tx2 int64
	var madeActive bool

	// 成功建立用户转发连接后，立即记为活跃
	atomic.AddInt64(&activePublicConns, 1)
	madeActive = true

	// 双向传输
	done := make(chan struct{}, 2)
	go func() {
		n, _ := io.Copy(stream, userConn)
		atomic.AddInt64(&tx1, n)
		done <- struct{}{}
	}()
	go func() {
		n, _ := io.Copy(userConn, stream)
		atomic.AddInt64(&tx2, n)
		done <- struct{}{}
	}()

	<-done
	<-done
	stream.Close()

	// 累计连接仅在满足“真实访问”阈值时加计
	total := atomic.LoadInt64(&tx1) + atomic.LoadInt64(&tx2)
	dur := time.Since(start)
	if tx1 > 0 && tx2 > 0 && total >= accessMinBytes && dur >= accessMinDur {
		atomic.AddInt64(&totalPublicAccepted, 1)
	}

	// 连接结束，活跃减计
	if madeActive {
		atomic.AddInt64(&activePublicConns, -1)
	}
}

// cleanupAllClients 关闭所有客户端的会话与监听器（通常在服务端退出时调用）
type EventItem struct {
	Time     string `json:"time"`
	Level    string `json:"level"`
	Message  string `json:"message"`
	ClientID string `json:"client_id,omitempty"`
}

func appendRecentEvent(level, msg string, clientID string) {
	recentMu.Lock()
	defer recentMu.Unlock()
	if len(recentEvents) >= 100 {
		recentEvents = recentEvents[1:]
	}
	recentEvents = append(recentEvents, EventItem{
		Time:     time.Now().Format("2006-01-02 15:04:05"),
		Level:    level,
		Message:  msg,
		ClientID: clientID,
	})
}

func collectStats() map[string]interface{} {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	allowedTokensMu.RLock()
	tokenCount := len(allowedTokens)
	allowedTokensMu.RUnlock()

	uptime := time.Since(startTime).Seconds()

	recentMu.Lock()
	// 仅返回最近100条事件
	n := len(recentEvents)
	start := 0
	if n > 100 { start = n - 100 }
	events := make([]EventItem, n-start)
	copy(events, recentEvents[start:])
	recentMu.Unlock()

	return map[string]interface{}{
		"active_clients":        atomic.LoadInt64(&activeClients),
		"total_clients":         atomic.LoadInt64(&totalClients),
		"active_public_conns":   atomic.LoadInt64(&activePublicConns),
		"total_public_accepted": atomic.LoadInt64(&totalPublicAccepted),
		"port_bindings":         atomic.LoadInt64(&portBindings),
		"tokens_loaded":         tokenCount,
		"server_addr":           serverAddr,
		"http_addr":             httpAddr,
		"goroutines":            runtime.NumGoroutine(),
		"mem_alloc":             ms.Alloc,
		"uptime_seconds":        int64(uptime),
		"recent_events":         events,
	}
}

func eventsHandler(w http.ResponseWriter, r *http.Request) {
	// 受保护接口：分页返回事件
	offset := 0
	limit := 20
	if v := r.URL.Query().Get("offset"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 { offset = n }
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 { limit = n }
	}
	recentMu.Lock()
	total := len(recentEvents)
	start := offset
	if start < 0 { start = 0 }
	if start > total { start = total }
	end := start + limit
	if end > total { end = total }
	items := make([]EventItem, end-start)
	copy(items, recentEvents[start:end])
	recentMu.Unlock()
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"items": items,
		"total": total,
		"offset": start,
		"limit": limit,
	})
}

func sseHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()
	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			data, _ := json.Marshal(collectStats())
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}

func startHTTPServer() {
	mux := http.NewServeMux()
	// 公共端点
	mux.HandleFunc("/events", authMiddleware(sseHandler))
	// 监控页：未登录直接重定向到 /login，已登录则展示
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("ftcp_admin")
		if err != nil || !validateSession(c.Value) {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}
		serveEmbeddedFile(w, "static/monitor/index.html")
	})

	mux.HandleFunc("/static/", func(w http.ResponseWriter, r *http.Request) {
		// 将请求路径映射到 embed 内的 static/ 目录
		// 例如 /static/monitor/style.css -> static/monitor/style.css
		p := strings.TrimPrefix(r.URL.Path, "/static/")
		serveEmbeddedFile(w, "static/"+p)
	})

	// 登录与授权管理 API（需登录）
	mux.HandleFunc("/login", loginHandler)
	mux.HandleFunc("/api/tokens", authMiddleware(tokensListOrCreateHandler))
	mux.HandleFunc("/api/tokens/", authMiddleware(tokenDeleteHandler))
	mux.HandleFunc("/api/events", authMiddleware(eventsHandler))

	log.Printf("监控 HTTP 监听 %s，访问 /", httpAddr)
	if err := http.ListenAndServe(httpAddr, mux); err != nil {
		log.Printf("HTTP 监控服务退出: %v", err)
	}
}

func initDB() error {
	return db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte("tokens")); err != nil {
			return err
		}
		return nil
	})
}

func upsertTokenToDB(token, clientID string, ports ...int) error {
	// value: {"client_id":"...","ports":[...]}
	v := map[string]interface{}{"client_id": clientID, "ports": ports}
	b, _ := json.Marshal(v)
	return db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("tokens")).Put([]byte(token), b)
	})
}

func deleteTokenFromDB(token string) error {
	return db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte("tokens")).Delete([]byte(token))
	})
}

func loadTokensFromDB() error {
	tmpTokens := make(map[string]string)
	tmpPorts := make(map[string][]int)
	err := db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte("tokens"))
		if b == nil {
			return nil
		}
		return b.ForEach(func(k, v []byte) error {
			type rec struct {
				ClientID string `json:"client_id"`
				Ports    []int  `json:"ports"`
			}
			var r rec
			_ = json.Unmarshal(v, &r)
			tmpTokens[string(k)] = r.ClientID
			if len(r.Ports) > 0 {
				tmpPorts[string(k)] = r.Ports
			} else {
				tmpPorts[string(k)] = nil // nil/空表示不限端口
			}
			return nil
		})
	})
	if err != nil {
		return err
	}
	allowedTokensMu.Lock()
	allowedTokens = tmpTokens
	allowedTokensMu.Unlock()
	allowedTokenPortsMu.Lock()
	allowedTokenPorts = tmpPorts
	allowedTokenPortsMu.Unlock()
	return nil
}

// enforceAuthorizations 在授权更新后校验所有在线客户端，不合规者断开会话
func enforceAuthorizations() {
	// 收集需要关闭的客户端，避免持锁时修改结构
	var toClose []*ClientInfo
	portMapMu.RLock()
	seen := make(map[*ClientInfo]struct{})
	for port, ci := range portMap {
		if ci == nil {
			continue
		}
		if _, ok := seen[ci]; ok {
			continue
		}
		// 校验 token 是否仍有效
		if !validateToken(ci.Token, ci.ID) {
			toClose = append(toClose, ci)
			seen[ci] = struct{}{}
			continue
		}
		// 校验每个绑定端口是否仍被该 token 允许
		okAll := true
		for _, p := range ci.ports {
			if !isPortAllowedForToken(ci.Token, p) {
				okAll = false
				break
			}
		}
		if !okAll {
			toClose = append(toClose, ci)
			seen[ci] = struct{}{}
		}
		_ = port // 仅用于遍历，避免未使用警告
	}
	portMapMu.RUnlock()

	// 关闭不合规的客户端会话（触发既有清理逻辑）
	for _, ci := range toClose {
		if ci != nil && ci.Session != nil {
			_ = ci.Session.Close()
		} else if ci != nil && ci.cancel != nil {
			ci.cancel()
		}
	}
}

func isPortAllowedForToken(token string, port int) bool {
	allowedTokenPortsMu.RLock()
	ports, ok := allowedTokenPorts[token]
	allowedTokenPortsMu.RUnlock()
	if !ok {
		return false
	}
	if len(ports) == 0 {
		return true
	}
	for _, p := range ports {
		if p == port {
			return true
		}
	}
	return false
}



func genRandomPassword(n int) string {
	buf := make([]byte, n)
	_, _ = rand.Read(buf)
	return base64.RawURLEncoding.EncodeToString(buf)
}

// 简单会话与鉴权
func authMiddleware(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie("ftcp_admin")
		if err != nil || !validateSession(c.Value) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

func validateSession(token string) bool {
	sessionMu.Lock()
	defer sessionMu.Unlock()
	exp, ok := sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(exp) {
		delete(sessions, token)
		return false
	}
	return true
}

func loginHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// 展示登录页
		serveEmbeddedFile(w, "static/monitor/login.html")
		return
	case http.MethodPost:
		// 执行登录
		var req struct{ Password string `json:"password"` }
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if strings.TrimSpace(req.Password) != adminPassword {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		// 发放会话
		token := genRandomPassword(24)
		sessionMu.Lock()
		sessions[token] = time.Now().Add(12 * time.Hour)
		sessionMu.Unlock()
		http.SetCookie(w, &http.Cookie{
			Name:     "ftcp_admin",
			Value:    token,
			MaxAge:   12 * 3600,
			Path:     "/",
			HttpOnly: true,
		})
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func tokensListOrCreateHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		type item struct {
			Token    string `json:"token"`
			ClientID string `json:"client_id"`
			Ports    []int  `json:"ports"`
		}
		var list []item
		_ = db.View(func(tx *bbolt.Tx) error {
			b := tx.Bucket([]byte("tokens"))
			if b == nil {
				return nil
			}
			return b.ForEach(func(k, v []byte) error {
				var rec item
				rec.Token = string(k)
				_ = json.Unmarshal(v, &rec)
				list = append(list, rec)
				return nil
			})
		})
		json.NewEncoder(w).Encode(map[string]interface{}{"items": list})
	case http.MethodPost:
		var req struct {
			Token    string `json:"token"`
			ClientID string `json:"client_id"`
			Ports    []int  `json:"ports"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
			http.Error(w, "Bad Request", http.StatusBadRequest)
			return
		}
		if err := upsertTokenToDB(strings.TrimSpace(req.Token), strings.TrimSpace(req.ClientID), req.Ports...); err != nil {
			http.Error(w, "DB Error", http.StatusInternalServerError)
			return
		}
		_ = loadTokensFromDB()
		// 授权变更立刻生效：强制校验在线客户端
		enforceAuthorizations()
		json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

func tokenDeleteHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	// URL: /api/tokens/{token}
	key := strings.TrimPrefix(r.URL.Path, "/api/tokens/")
	if key == "" {
		http.Error(w, "Bad Request", http.StatusBadRequest)
		return
	}
	if err := deleteTokenFromDB(key); err != nil {
		http.Error(w, "DB Error", http.StatusInternalServerError)
		return
	}
	_ = loadTokensFromDB()
	// 授权删除立刻生效：强制校验在线客户端
	enforceAuthorizations()
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

func cleanupAllClients() {
	portMapMu.Lock()
	defer portMapMu.Unlock()
	for _, ci := range portMap {
		if ci == nil {
			continue
		}
		ci.cancel()
		ci.mu.Lock()
		for p, ln := range ci.listeners {
			if ln != nil {
				_ = ln.Close()
			}
			delete(ci.listeners, p)
		}
		ci.mu.Unlock()
		if ci.Session != nil {
			_ = ci.Session.Close()
		}
	}
	// 清空映射
	portMap = make(map[int]*ClientInfo)
}

// sendJSONLine 将对象作为 JSON 行写入（追加换行符）
func sendJSONLine(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
}
