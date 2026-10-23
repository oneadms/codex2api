package proxy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
	xproxy "golang.org/x/net/proxy"

	"github.com/codex2api/security"
)

// ==================== utls RoundTripper（Chrome 指纹 + HTTP/2） ====================
//
// 设计要点：
//   - 使用 HelloChrome_Auto 模拟 Chrome 浏览器的 TLS 指纹
//   - 支持 HTTP/2 协议（与 OpenAI/Anthropic API 兼容）
//   - 连接池 + pending 管理：防止同一 host 重复创建连接
//   - 代理支持：HTTP(S) 和 SOCKS5

// 连接生命周期参数（issue #446）。
//
// 该 transport 自管 HTTP/2 连接池，不经标准库 *http.Transport，因此标准库的
// IdleConnTimeout 一概不生效——必须显式配到 http2.Transport 上，否则
// NewClientConn 不会安装空闲定时器（idleConnTimeout()==0 时直接跳过），
// 连接的 readLoop goroutine 会永久持有 socket 与缓冲区，既不关闭也无法 GC。
const (
	// codexUTLSIdleConnTimeout 是自管 HTTP/2 连接的空闲存活上限，
	// 与标准 transport 的 IdleConnTimeout 对齐。到点由 h2 自身的空闲定时器
	// 调用 closeIfIdle 回收，是所有回收路径的最终兜底。
	codexUTLSIdleConnTimeout = 90 * time.Second

	// utlsCloseIdleGrace 是 CloseIdleConnections 的保护窗口：连接从池中取出到
	// 真正发起 RoundTrip 之间存在无锁窗口，此时 h2 侧尚无任何 stream，看起来
	// 「空闲」。用最后交付时间兜住这个窗口，避免关掉刚被取走待用的连接。
	utlsCloseIdleGrace = 5 * time.Second
)

// utlsShutdownTimeout 返回优雅关闭时等待在途 stream 收尾的上限。
//
// 可在系统设置里调（utls_shutdown_timeout_minutes，默认 30 分钟）：流式回答
// 单回合可超过 10 分钟（issue #287），默认给足余量；超时才强制关闭，
// 保证异常挂死的 stream 不会把连接永久留住。
func utlsShutdownTimeout() time.Duration {
	return CurrentRuntimeSettings().UTLSShutdownTimeout()
}

// utlsConn 包装一条自管 HTTP/2 连接，并记录最后一次「交付给请求」的时间。
// h2 的 State() 只能反映当前 stream 数，无法区分「刚取出还没发请求」与
// 「已彻底空闲」，故额外记录时间戳做空闲判定。
type utlsConn struct {
	conn     *http2.ClientConn
	lastUsed atomic.Int64 // UnixNano
}

func (c *utlsConn) touch() {
	c.lastUsed.Store(time.Now().UnixNano())
}

// busy 报告连接上是否有在途请求（含已预留与排队等待的 stream）。
// 流式响应在 body 关闭前始终计入 StreamsActive，因此长回答不会被误判为空闲。
func (c *utlsConn) busy() bool {
	st := c.conn.State()
	return st.StreamsActive > 0 || st.StreamsReserved > 0 || st.StreamsPending > 0
}

// reclaimable 报告该连接可否安全关闭：无在途请求，且已过交付保护窗口。
func (c *utlsConn) reclaimable(now time.Time, grace time.Duration) bool {
	if c.busy() {
		return false
	}
	return now.Sub(time.Unix(0, c.lastUsed.Load())) >= grace
}

// utlsRoundTripper 实现 http.RoundTripper 接口
// 使用 utls 模拟 Chrome 浏览器的 TLS 指纹以绕过 TLS 指纹检测
type utlsRoundTripper struct {
	proxyErr    error
	mu          sync.Mutex
	connections map[string]*utlsConn  // HTTP/2 连接池，按 host 索引
	pending     map[string]*sync.Cond // 防止重复连接创建
	// http1Hosts 记录「TLS 握手后没有协商出 h2」的上游。这类上游（典型是只
	// 说 HTTP/1.1 的中转网关）必须走 HTTP/1.1，否则我们会按 h2 发 preface，
	// 对端把 HTTP/1.1 文本响应回过来，h2 客户端读帧头时报
	// "frame too large ... looked like an HTTP/1.1 header"。
	http1Hosts map[string]struct{}
	dialer     xproxy.Dialer // 底层拨号器（支持代理）
	// fallback 承载 HTTP/1.1 上游的请求。它复用同一个 dialer（代理设置一致），
	// 只是换成标准库的 TLS + h1 编解码：这些上游通常是不做 TLS 指纹检测的
	// 中转网关，保住「能通」比保住 Chrome 指纹更重要。
	fallback *http.Transport
}

// errUTLSHTTP1Only 表示上游在 TLS 握手时没有协商出 h2（ALPN 为空或 http/1.1）。
// 它是内部信号，不直接暴露给调用方：RoundTrip 收到后会改用 fallback 重发。
var errUTLSHTTP1Only = errors.New("utls: 上游未协商 HTTP/2")

// isUTLSHTTP1Only 报告错误是否代表「该上游只能走 HTTP/1.1」。
func isUTLSHTTP1Only(err error) bool {
	return errors.Is(err, errUTLSHTTP1Only)
}

// utlsNegotiatedHTTP2 判断 TLS 握手协商结果能否承载 HTTP/2。
//
// 只认 ALPN 明确给出的 h2。空值意味着对端不支持 ALPN（或只回 http/1.1），
// 此时按 h2 发帧会让对端把我们的二进制 preface 当成垃圾、回一个 HTTP/1.1
// 文本响应，客户端读帧头时报 frame too large。
func utlsNegotiatedHTTP2(state utls.ConnectionState) bool {
	return state.NegotiatedProtocol == http2.NextProtoTLS
}

// utlsHTTP1FallbackTransport 构造 HTTP/1.1 回退 transport。
// 走与 uTLS 相同的拨号器（代理 CONNECT / SOCKS5 语义一致），TLS 交给标准库。
func utlsHTTP1FallbackTransport(dialer xproxy.Dialer) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConnsPerHost = 4
	transport.IdleConnTimeout = codexUTLSIdleConnTimeout
	transport.ResponseHeaderTimeout = 5 * time.Minute
	// 明确不要 h2：这条路径存在的意义就是服务只支持 HTTP/1.1 的上游。
	transport.ForceAttemptHTTP2 = false
	transport.TLSNextProto = map[string]func(string, *tls.Conn) http.RoundTripper{}
	if transport.TLSClientConfig == nil {
		transport.TLSClientConfig = &tls.Config{}
	}
	transport.TLSClientConfig.ClientSessionCache = codexTLSSessionCache
	// 回退路径自己完成 CONNECT/SOCKS 隧道与 TLS，故不使用 Proxy 字段。
	transport.Proxy = nil
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		type contextDialer interface {
			DialContext(context.Context, string, string) (net.Conn, error)
		}
		if cd, ok := dialer.(contextDialer); ok {
			return cd.DialContext(ctx, network, addr)
		}
		return dialer.Dial(network, addr)
	}
	return transport
}

// utlsSessionCache 在所有 uTLS 连接间共享 TLS 会话缓存，让重连走 TLS resumption。
// 必须实例级共享（而非每次 new），否则缓存无法命中。
var utlsSessionCache = utls.NewLRUClientSessionCache(256)

// NewUTLSTransport 创建使用 Chrome TLS 指纹的 RoundTripper
// 支持 HTTP(S) 和 SOCKS5 代理
func NewUTLSTransport(proxyURL string) http.RoundTripper {
	var dialer xproxy.Dialer = xproxy.Direct
	var proxyErr error

	if proxyURL != "" {
		d, err := buildProxyDialer(proxyURL)
		if err != nil {
			proxyErr = fmt.Errorf("代理配置无效，已拒绝直连")
		} else {
			dialer = d
		}
	}

	return &utlsRoundTripper{
		connections: make(map[string]*utlsConn),
		proxyErr:    proxyErr,
		pending:     make(map[string]*sync.Cond),
		http1Hosts:  make(map[string]struct{}),
		dialer:      dialer,
		fallback:    utlsHTTP1FallbackTransport(dialer),
	}
}

// NewUTLSHttpClient 创建使用 Chrome TLS 指纹的 HTTP 客户端
func NewUTLSHttpClient(proxyURL string) *http.Client {
	return &http.Client{
		Transport: NewUTLSTransport(proxyURL),
		Timeout:   0, // 不设置全局超时，由请求上下文控制
	}
}

// buildProxyDialer 根据代理 URL 创建拨号器
func buildProxyDialer(proxyURL string) (xproxy.Dialer, error) {
	u, err := security.ParseProxyURL(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("解析代理 URL 失败: %w", err)
	}

	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		return security.NewHTTPConnectDialer(u), nil
	case "socks5", "socks5h":
		return buildSOCKS5Dialer(u)
	default:
		return nil, fmt.Errorf("不支持的代理协议: %s", u.Scheme)
	}
}

// buildSOCKS5Dialer 创建 SOCKS5 代理拨号器
func buildSOCKS5Dialer(u *url.URL) (xproxy.Dialer, error) {
	var auth *xproxy.Auth
	if u.User != nil {
		password, _ := u.User.Password()
		auth = &xproxy.Auth{
			User:     u.User.Username(),
			Password: password,
		}
	}

	return xproxy.SOCKS5("tcp", u.Host, auth, xproxy.Direct)
}

// getOrCreateConnection 获取或创建 HTTP/2 连接
// 使用 sync.Cond 防止同一 host 的重复连接创建
//
// 返回前会 touch() 命中的连接：该时间戳是 CloseIdleConnections 的保护窗口依据，
// 防止连接在“已取出、尚未发 stream”的窗口里被当成空闲连接关掉。
func (t *utlsRoundTripper) getOrCreateConnection(host, addr string) (*utlsConn, error) {
	t.mu.Lock()

	// 已判定只能走 HTTP/1.1 的上游不再尝试 h2：每次重连都要多花一次 TLS
	// 握手才发现不支持，且失败后连接会被丢弃。
	if _, ok := t.http1Hosts[host]; ok {
		t.mu.Unlock()
		return nil, errUTLSHTTP1Only
	}

	// 检查是否已有可用连接
	if entry, ok := t.connections[host]; ok && entry.conn.CanTakeNewRequest() {
		entry.touch()
		t.mu.Unlock()
		return entry, nil
	}

	// 检查是否有其他 goroutine 正在创建连接
	if cond, ok := t.pending[host]; ok {
		// 等待其他 goroutine 完成（循环重试，避免虚假唤醒）
		for {
			cond.Wait()
			// 建连者可能刚得出「该 host 只说 HTTP/1.1」的结论：等待者必须
			// 跟着走 h1 回退，而不是自己再握一次手重蹈覆辙。
			if _, h1 := t.http1Hosts[host]; h1 {
				t.mu.Unlock()
				return nil, errUTLSHTTP1Only
			}
			// 再次检查连接是否可用
			if entry, ok := t.connections[host]; ok && entry.conn.CanTakeNewRequest() {
				entry.touch()
				t.mu.Unlock()
				return entry, nil
			}
			// 如果 pending 已移除，说明创建完成（可能失败），跳出循环自己创建
			if _, still := t.pending[host]; !still {
				break
			}
		}
	}

	// 标记此 host 正在创建连接
	cond := sync.NewCond(&t.mu)
	t.pending[host] = cond
	t.mu.Unlock()

	// 在锁外创建连接
	h2Conn, err := t.createConnection(host, addr)

	t.mu.Lock()
	defer t.mu.Unlock()

	// 移除 pending 标记并唤醒一个等待者（Signal 而非 Broadcast，避免惊群）
	delete(t.pending, host)
	cond.Broadcast()

	if err != nil {
		// 记录「这个上游只说 HTTP/1.1」，后续请求直接走 h1 回退路径，
		// 不再每次都先做一轮注定失败的 h2 握手。
		if isUTLSHTTP1Only(err) {
			if _, known := t.http1Hosts[host]; !known {
				t.http1Hosts[host] = struct{}{}
				// 只在这个 host 首次被判定时记一次：h1 上游是常态（中转网关），
				// 每条请求都打一行日志会把日志刷满。
				log.Printf("[UTLS] 上游未协商 h2，改用 HTTP/1.1: host=%s", host)
			}
		}
		return nil, err
	}

	// 旧连接已不可用（走到这里说明 CanTakeNewRequest()==false），但可能仍有
	// 在途 stream（例如已收 GOAWAY 但旧请求未完）。用 graceful shutdown 等它们
	// 收尾再关，不能直接 Close 掉——否则会把进行中的流式回答直接截断。
	if oldEntry, ok := t.connections[host]; ok {
		shutdownUTLSConn(oldEntry.conn)
	}

	// 存储新连接
	entry := &utlsConn{conn: h2Conn}
	entry.touch()
	t.connections[host] = entry
	return entry, nil
}

// shutdownUTLSConn 异步优雅关闭一条连接：先发 GOAWAY 并等在途 stream 收尾，
// 超时（或异常）后强制关闭。两路径都保证底层 socket 与 readLoop goroutine
// 最终被释放，不会像修复前那样泄漏（issue #446）。
func shutdownUTLSConn(conn *http2.ClientConn) {
	if conn == nil {
		return
	}
	timeout := utlsShutdownTimeout()
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		defer cancel()
		if err := conn.Shutdown(ctx); err != nil {
			// 超时/失败均强制关闭，宁可截断挂死请求也不能泄漏连接。
			_ = conn.Close()
		}
	}()
}

// createConnection 创建新的 HTTP/2 连接
// 使用 utls 的 HelloChrome_Auto 模拟 Chrome 浏览器的 TLS 指纹
func (t *utlsRoundTripper) createConnection(host, addr string) (*http2.ClientConn, error) {
	// 1. 建立 TCP 连接（通过代理或直连）
	conn, err := t.dialer.Dial("tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("TCP 连接失败: %w", err)
	}

	// 2. 配置 TLS（共享会话缓存，握手走 resumption 降低重连成本）
	// NextProtos 显式声明与 Chrome 一致的 ALPN 候选：HelloChrome_Auto 的
	// 指纹扩展里已带同样的列表，这里再写一遍是为了让协商结果可预期——
	// 握手后我们据此判断该 host 能否走 h2。
	tlsConfig := &utls.Config{
		ServerName:         host,
		ClientSessionCache: utlsSessionCache,
		NextProtos:         []string{http2.NextProtoTLS, "http/1.1"},
	}

	// 3. 使用 utls 握手（Chrome 指纹）
	tlsConn := utls.UClient(conn, tlsConfig, utls.HelloChrome_Auto)

	// 设置握手超时
	handshakeCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := tlsConn.HandshakeContext(handshakeCtx); err != nil {
		conn.Close()
		return nil, fmt.Errorf("TLS 握手失败: %w", err)
	}

	// 4. 必须先看 ALPN 协商结果再决定编解码协议。
	//
	// 修复前的 bug：无条件按 HTTP/2 发 preface。只支持 HTTP/1.1 的上游
	// （典型是 new-api 系中转网关，如 ddl.ddpapi.top）会把 HTTP/1.1 文本响应
	// 回过来，h2 客户端按帧头解析时报
	// "http2: frame too large, note that the frame header looked like an HTTP/1.1 header"。
	if !utlsNegotiatedHTTP2(tlsConn.ConnectionState()) {
		_ = conn.Close()
		return nil, errUTLSHTTP1Only
	}

	// 5. 创建 HTTP/2 连接
	// 启用保活 PING（ReadIdleTimeout/PingTimeout）：uTLS 自管连接池，池化连接
	// 被代理/NAT 静默掐断后无法被感知，请求会挂到 TCP 重传超时。开启后空闲连接
	// 主动发 PING，探测失败即在读循环里报错，触发上层从池中剔除并重建。
	//
	// IdleConnTimeout 必须显式设置：NewClientConn 仅在 idleConnTimeout()!=0 时安装
	// 空闲定时器（closeIfIdle）。缺失时连接永不自动回收，readLoop goroutine 与
	// socket 常驻，是 issue #446 万级连接/goroutine 泄漏的根因。
	tr := &http2.Transport{
		ReadIdleTimeout: codexHTTP2ReadIdleTimeout,
		PingTimeout:     codexHTTP2PingTimeout,
		IdleConnTimeout: codexUTLSIdleConnTimeout,
	}
	h2Conn, err := tr.NewClientConn(tlsConn)
	if err != nil {
		tlsConn.Close()
		return nil, fmt.Errorf("HTTP/2 连接创建失败: %w", err)
	}

	return h2Conn, nil
}

// RoundTrip 实现 http.RoundTripper 接口
func (t *utlsRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.proxyErr != nil {
		return nil, t.proxyErr
	}
	host := req.URL.Host
	addr := host
	if !strings.Contains(addr, ":") {
		addr += ":443"
	}

	// 获取主机名（不含端口）用于 TLS ServerName
	hostname := req.URL.Hostname()

	// 取连耗时：连接池命中约等于 0，冷连接含拨号/TLS 握手；同主机并发建连时
	// getOrCreateConnection 会等在途拨号，这段排队时间也一并计入（issue #413）。
	acquireStart := time.Now()
	entry, err := t.getOrCreateConnection(hostname, addr)
	AddWsAcquireDuration(req.Context(), time.Since(acquireStart))
	if err != nil {
		// 上游只支持 HTTP/1.1（中转网关）。改用 h1 回退 transport 重发，
		// 而不是把 "frame too large" 这类协议错配错误抛给上层。
		if isUTLSHTTP1Only(err) {
			return t.fallback.RoundTrip(req)
		}
		return nil, err
	}

	resp, err := entry.conn.RoundTrip(req)
	if err != nil {
		// 连接失败，从缓存中移除并关闭连接
		t.mu.Lock()
		if cached, ok := t.connections[hostname]; ok && cached == entry {
			delete(t.connections, hostname)
		}
		t.mu.Unlock()
		// 关闭失败的连接，避免资源泄漏。已摘出池，同一连接上可能还有
		// 其他并发 stream（h2 多路复用），故走优雅关闭而非直接 Close，
		// 避免误杀邻居请求；超时后仍会强制关闭。
		shutdownUTLSConn(entry.conn)
		return nil, err
	}

	// 响应头已到，body 可能还在流式传输（stream 仍计入 StreamsActive）；
	// 刷新时间戳使空闲判定以“最后一次使用”为基准。
	entry.touch()
	return resp, nil
}

// CloseIdleConnections 关闭所有空闲连接。
//
// 判据是“连接上无在途 stream”，而不能用 CanTakeNewRequest()：后者对健康空闲
// 连接恒为 true，修复前的 !CanTakeNewRequest() 条件恰好把真正需要回收的空闲
// 连接全部跳过，导致上层 TTL 淘汰路径彻底失效（issue #446）。
func (t *utlsRoundTripper) CloseIdleConnections() {
	t.closeIdleConnections(utlsCloseIdleGrace)
}

func (t *utlsRoundTripper) closeIdleConnections(grace time.Duration) {
	now := time.Now()

	// 先在锁内摘除，再在锁外关闭：Close 会触发 h2 内部锁与回调，持锁关闭
	// 有自身与 RoundTrip 互锁的风险。
	var reclaimed []*http2.ClientConn
	t.mu.Lock()
	for host, entry := range t.connections {
		if entry.reclaimable(now, grace) {
			delete(t.connections, host)
			reclaimed = append(reclaimed, entry.conn)
		}
	}
	t.mu.Unlock()

	for _, conn := range reclaimed {
		_ = conn.Close()
	}
	// HTTP/1.1 回退路径也持有连接（issue #446 的同类泄漏点），一并回收。
	if t.fallback != nil {
		t.fallback.CloseIdleConnections()
	}
}

// CloseAllConnections 摘除并关闭该 transport 持有的全部连接。
//
// 给“这个 transport 不再被持有”的场景用（一次性 Client 用完、连接池 TTL 淘汰），
// 确保不留下无人回收的连接。在途 stream 走优雅关闭（等收尾），不截断正在
// 传输的响应体。
//
// 调用后 transport 仍可用（后续请求会重新拨号）：故意不做“永久毒化”，否则与
// 连接池淘汰存在竞争——刚被取走的 client 会直接报错。漏网的掉队请求新建的
// 连接由 IdleConnTimeout 兜底回收，不会无限积累。
func (t *utlsRoundTripper) CloseAllConnections() {
	t.mu.Lock()
	entries := make([]*utlsConn, 0, len(t.connections))
	for host, entry := range t.connections {
		entries = append(entries, entry)
		delete(t.connections, host)
	}
	t.mu.Unlock()

	for _, entry := range entries {
		if entry.busy() {
			shutdownUTLSConn(entry.conn)
			continue
		}
		_ = entry.conn.Close()
	}
	// 回退 transport 的连接同样必须收口，否则一次性 transport 的 h1 连接
	// 会随 entry 一起失去持有者而泄漏。
	if t.fallback != nil {
		t.fallback.CloseIdleConnections()
	}
}

// connCountForTest 返回当前池内连接数，仅测试断言用。
func (t *utlsRoundTripper) connCountForTest() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.connections)
}

// ==================== 兼容现有代码的封装 ====================

// uTLSHTTPClientWrapper 包装 utlsRoundTripper 以兼容现有的 http.Client 接口
type uTLSHTTPClientWrapper struct {
	transport *utlsRoundTripper
}

// NewUTLSClient 创建使用 Chrome TLS 指纹的 HTTP 客户端
// 返回包装后的客户端，支持 CloseIdleConnections
func NewUTLSClient(proxyURL string) *uTLSHTTPClientWrapper {
	rt := NewUTLSTransport(proxyURL).(*utlsRoundTripper)
	return &uTLSHTTPClientWrapper{
		transport: rt,
	}
}

// Do 执行 HTTP 请求
func (c *uTLSHTTPClientWrapper) Do(req *http.Request) (*http.Response, error) {
	return c.transport.RoundTrip(req)
}

// CloseIdleConnections 关闭空闲连接
func (c *uTLSHTTPClientWrapper) CloseIdleConnections() {
	c.transport.CloseIdleConnections()
}
