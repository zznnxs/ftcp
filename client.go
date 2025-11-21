package main

import (
	"bufio"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/xtaci/smux"
)

// Registration 客户端注册信息（包含 token）
type Registration struct {
	ClientID string `json:"client_id"`
	Ports    []int  `json:"ports"`
	Token    string `json:"token,omitempty"`
}

// PortResult 端口注册结果
type PortResult struct {
	Port   int    `json:"port"`
	OK     bool   `json:"ok"`
	Reason string `json:"reason,omitempty"`
}

// RegResult 注册结果
type RegResult struct {
	Results []PortResult `json:"results"`
}

var (
	serverAddr string
	clientID   string
	clientMap  string
	localMap   = map[int]string{}
	authToken  string
)

func init() {
	flag.StringVar(&serverAddr, "server", "1.2.3.4:7000", "服务端地址")
	flag.StringVar(&clientID, "id", fmt.Sprintf("user-%d", rand.Intn(1000000)), "客户端ID")
	flag.StringVar(&clientMap, "map", "9000:127.0.0.1:3389", "端口映射，格式 public:local;public2:local2")
	flag.StringVar(&authToken, "token", "", "连接服务端的 token（必需）")
	flag.Parse()

	parseMap(clientMap)
}

func parseMap(raw string) {
	log.Println("参考：client.exe -server 1.2.3.4:7000 -id pc -token xxx -map \"9000:127.0.0.1:3389;9001:127.0.0.1:22\"")
	log.Println("客户端已启动，正在读取映射配置...")
	if raw == "" {
		return
	}
	for _, part := range strings.Split(raw, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		colonIndex := strings.Index(part, ":")
		if colonIndex <= 0 {
			log.Printf("无效的映射配置: %s", part)
			continue
		}
		pubStr := part[:colonIndex]
		local := part[colonIndex+1:]
		pub, err := strconv.Atoi(pubStr)
		if err != nil {
			log.Printf("无效的公网端口: %s", pubStr)
			continue
		}
		localMap[pub] = local
	}
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	log.Printf("运行参数：ClientID=%s Server=%s Maps=%v Token=%s", clientID, serverAddr, localMap, maskToken(authToken))

	// 捕获退出信号
	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	fixedDelay := 10 * time.Second
	for {
		select {
		case <-shutdown:
			log.Println("收到退出信号，退出客户端")
			return
		default:
		}

		err := runOnce(shutdown)
		if err != nil {
			log.Printf("连接出错: %v", err)
		}
		log.Printf("重连中，遇到错误固定将在 %v 后重连...", fixedDelay)
		time.Sleep(fixedDelay)
	}
}

func runOnce(shutdown <-chan os.Signal) error {
	conn, err := net.Dial("tcp", serverAddr)
	if err != nil {
		return err
	}
	defer conn.Close()

	cfg := smux.DefaultConfig()
	cfg.KeepAliveInterval = 10 * time.Second
	session, err := smux.Client(conn, cfg)
	if err != nil {
		return err
	}
	log.Println("已经成功与服务端会话已建立~")

	// 注册端口（带 token）
	reg := Registration{ClientID: clientID, Token: authToken}
	for p := range localMap {
		reg.Ports = append(reg.Ports, p)
	}
	regStream, err := session.OpenStream()
	if err != nil {
		session.Close()
		return err
	}
	if err := sendJSONLine(regStream, reg); err != nil {
		regStream.Close()
		session.Close()
		return err
	}

	// 接收注册结果
	br := bufio.NewReader(regStream)
	line, err := br.ReadString('\n')
	if err != nil {
		log.Printf("读取注册结果失败: %v", err)
		regStream.Close()
		session.Close()
		return err
	}
	var rr RegResult
	if err := json.Unmarshal([]byte(line), &rr); err != nil {
		log.Printf("注册结果 JSON 解析失败: %v", err)
	} else {
		for _, r := range rr.Results {
			if r.OK {
				log.Printf("[注册成功] 公网 %d -> %s", r.Port, localMap[r.Port])
			} else {
				log.Printf("[注册失败] 公网 %d : %s", r.Port, r.Reason)
			}
		}
	}
	regStream.Close()

	// 心跳保持
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go heartbeater(ctx, session)

	// 接收 server 流
	acceptErr := make(chan error, 1)
	go func() {
		for {
			s, err := session.AcceptStream()
			if err != nil {
				acceptErr <- err
				return
			}
			go handleIncomingStream(s)
		}
	}()

	// 等待 session 断开或外部 shutdown 信号
	select {
	case <-shutdown:
		_ = session.Close()
		return nil
	case err := <-acceptErr:
		_ = session.Close()
		return err
	}
}

func handleIncomingStream(s *smux.Stream) {
	defer s.Close()
	br := bufio.NewReader(s)
	line, err := br.ReadString('\n')
	if err != nil {
		log.Printf("读取头部信息失败: %v", err)
		return
	}
	var header map[string]int
	if err := json.Unmarshal([]byte(line), &header); err != nil {
		log.Printf("头部信息 JSON 解析失败: %v", err)
		return
	}
	port := header["port"]
	localAddr, ok := localMap[port]
	if !ok {
		log.Printf("未找到本地映射端口 %d", port)
		return
	}

	localConn, err := net.Dial("tcp", localAddr)
	if err != nil {
		log.Printf("连接本地服务 %s 失败: %v", localAddr, err)
		return
	}
	defer localConn.Close()

	done := make(chan struct{}, 2)
	go func() { io.Copy(localConn, br); done <- struct{}{} }()
	go func() { io.Copy(s, localConn); done <- struct{}{} }()
	<-done
}

func heartbeater(ctx context.Context, session *smux.Session) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s, err := session.OpenStream()
			if err != nil {
				log.Printf("心跳失败: %v", err)
				return
			}
			_, err = s.Write([]byte("hb\n"))
			if err != nil {
				log.Printf("心跳写入失败: %v", err)
				s.Close()
				return
			}
			s.Close()
		}
	}
}

func sendJSONLine(w io.Writer, v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = w.Write(append(b, '\n'))
	return err
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
