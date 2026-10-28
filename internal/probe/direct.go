package probe

// DirectExit 直接经出口(socks5/http/https)发起探测,不经过前置。
//
// 用途:
//   - 面板上没有可切换的组、或没开混合入站时,至少能验证出口本身可用;
//   - 排查时区分"是前置坏了还是出口坏了"。
//
// 注意:住宅代理通常只允许来自白名单或前置的连入,所以这种方式失败
// **不代表**链路不可用,面板上会给出对应的提示文案。
import (
	"context"
	"net"
	"net/url"
	"strconv"
	"time"

	"boxhub/internal/config"
)

// DirectExit 直接连出口做一次探测。
func DirectExit(ctx context.Context, ch *config.Chain, opts Options) Result {
	if opts.Timeout == 0 {
		opts = DefaultOptions()
	}
	var dialer Dialer
	switch ch.Type {
	case config.ChainSocks5:
		// 直接连出口的 SOCKS5:构造一个 Dialer 指向出口本身
		dialer = &plainSocksDialer{addr: net.JoinHostPort(ch.Host, strconv.Itoa(ch.Port)), user: ch.Username, pass: ch.Password}
	case config.ChainHTTP, config.ChainHTTPS:
		scheme := "http"
		if ch.Type == config.ChainHTTPS {
			scheme = "https"
		}
		u := &url.URL{Scheme: scheme, Host: net.JoinHostPort(ch.Host, strconv.Itoa(ch.Port))}
		if ch.Username != "" {
			u.User = url.UserPassword(ch.Username, ch.Password)
		}
		dialer = &HTTPDialer{ProxyURL: u, Timeout: opts.Timeout}
	default:
		return Result{At: time.Now(), Delay: -1, Error: "不支持的出口类型"}
	}
	res := Run(ctx, dialer, opts)
	if res.Delay < 0 && res.Error != "" {
		res.Error += "(直连出口探测失败:住宅代理通常只允许经前置连入,这不代表链路不可用)"
	}
	return res
}

// plainSocksDialer 直连 SOCKS5 出口(不经 sing-box)。
type plainSocksDialer struct {
	addr string
	user string
	pass string
}

func (p *plainSocksDialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	var nd net.Dialer
	conn, err := nd.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return nil, err
	}
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := socks5Handshake(conn, addr, p.user, p.pass); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}
