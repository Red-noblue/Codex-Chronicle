// 中文说明：提供无第三方依赖的 SOCKS5/SOCKS5h 拨号器，供归档网络 transport 使用。
package archive

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"
)

type socks5Dialer struct {
	proxyAddress string
	username     string
	password     string
	remoteDNS    bool
	dialer       net.Dialer
}

func newSOCKS5Dialer(proxyURL *url.URL) (*socks5Dialer, error) {
	if proxyURL == nil || proxyURL.Hostname() == "" || proxyURL.Port() == "" {
		return nil, fmt.Errorf("SOCKS5 代理必须包含主机和端口")
	}
	username := ""
	password := ""
	if proxyURL.User != nil {
		username = proxyURL.User.Username()
		if passwordValue, ok := proxyURL.User.Password(); ok {
			password = passwordValue
		}
	}
	if len(username) > 255 || len(password) > 255 {
		return nil, fmt.Errorf("SOCKS5 用户名或密码过长")
	}
	return &socks5Dialer{
		proxyAddress: net.JoinHostPort(proxyURL.Hostname(), proxyURL.Port()),
		username:     username,
		password:     password,
		remoteDNS:    strings.EqualFold(proxyURL.Scheme, "socks5h"),
	}, nil
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if d == nil {
		return nil, fmt.Errorf("SOCKS5 拨号器未初始化")
	}
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("SOCKS5 仅支持 TCP 目标")
	}
	conn, err := d.dialer.DialContext(ctx, "tcp", d.proxyAddress)
	if err != nil {
		return nil, fmt.Errorf("连接 SOCKS5 代理失败: %w", err)
	}
	watchDone := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-watchDone:
		}
	}()
	defer close(watchDone)
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := d.handshake(ctx, conn, address); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

func (d *socks5Dialer) handshake(ctx context.Context, conn net.Conn, address string) error {
	methods := []byte{0x00}
	if d.username != "" {
		methods = append(methods, 0x02)
	}
	if err := writeAll(conn, append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return fmt.Errorf("发送 SOCKS5 方法协商失败: %w", err)
	}
	methodReply := make([]byte, 2)
	if _, err := io.ReadFull(conn, methodReply); err != nil {
		return fmt.Errorf("读取 SOCKS5 方法协商失败: %w", err)
	}
	if methodReply[0] != 0x05 {
		return fmt.Errorf("SOCKS5 代理返回了未知版本 %d", methodReply[0])
	}
	switch methodReply[1] {
	case 0x00:
	case 0x02:
		if err := d.authenticate(conn); err != nil {
			return err
		}
	default:
		return fmt.Errorf("SOCKS5 代理不接受配置的认证方式 %d", methodReply[1])
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("解析 SOCKS5 目标地址失败: %w", err)
	}
	portNumber, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return fmt.Errorf("解析 SOCKS5 目标端口失败: %w", err)
	}
	target, err := d.targetAddress(ctx, host, uint16(portNumber))
	if err != nil {
		return err
	}
	request := append([]byte{0x05, 0x01, 0x00}, target...)
	if err := writeAll(conn, request); err != nil {
		return fmt.Errorf("发送 SOCKS5 连接请求失败: %w", err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(conn, response); err != nil {
		return fmt.Errorf("读取 SOCKS5 连接响应失败: %w", err)
	}
	if response[0] != 0x05 {
		return fmt.Errorf("SOCKS5 连接响应版本无效: %d", response[0])
	}
	if response[1] != 0x00 {
		return fmt.Errorf("SOCKS5 代理拒绝连接，错误码 %d", response[1])
	}
	if err := discardSOCKS5Address(conn, response[3]); err != nil {
		return err
	}
	return nil
}

func (d *socks5Dialer) authenticate(conn net.Conn) error {
	request := []byte{0x01, byte(len(d.username))}
	request = append(request, d.username...)
	request = append(request, byte(len(d.password)))
	request = append(request, d.password...)
	if err := writeAll(conn, request); err != nil {
		return fmt.Errorf("发送 SOCKS5 用户认证失败: %w", err)
	}
	reply := make([]byte, 2)
	if _, err := io.ReadFull(conn, reply); err != nil {
		return fmt.Errorf("读取 SOCKS5 用户认证失败: %w", err)
	}
	if reply[0] != 0x01 || reply[1] != 0x00 {
		return fmt.Errorf("SOCKS5 用户认证失败")
	}
	return nil
}

func (d *socks5Dialer) targetAddress(ctx context.Context, host string, port uint16) ([]byte, error) {
	if d.remoteDNS {
		if len(host) > 255 {
			return nil, fmt.Errorf("SOCKS5 目标域名过长")
		}
		return append([]byte{0x03, byte(len(host))}, append([]byte(host), portBytes(port)...)...), nil
	}
	ip := net.ParseIP(host)
	if ip == nil {
		addresses, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(addresses) == 0 {
			if err == nil {
				err = fmt.Errorf("没有解析结果")
			}
			return nil, fmt.Errorf("解析 SOCKS5 目标域名失败: %w", err)
		}
		ip = addresses[0]
	}
	if ip4 := ip.To4(); ip4 != nil {
		return append([]byte{0x01}, append(ip4, portBytes(port)...)...), nil
	}
	if ip16 := ip.To16(); ip16 != nil {
		return append([]byte{0x04}, append(ip16, portBytes(port)...)...), nil
	}
	return nil, fmt.Errorf("SOCKS5 目标 IP 无效")
}

func portBytes(port uint16) []byte {
	var value [2]byte
	binary.BigEndian.PutUint16(value[:], port)
	return value[:]
}

func discardSOCKS5Address(reader io.Reader, addressType byte) error {
	var size int
	switch addressType {
	case 0x01:
		size = net.IPv4len
	case 0x04:
		size = net.IPv6len
	case 0x03:
		length := []byte{0}
		if _, err := io.ReadFull(reader, length); err != nil {
			return fmt.Errorf("读取 SOCKS5 域名长度失败: %w", err)
		}
		size = int(length[0])
	default:
		return fmt.Errorf("SOCKS5 返回了未知地址类型 %d", addressType)
	}
	buffer := make([]byte, size+2)
	if _, err := io.ReadFull(reader, buffer); err != nil {
		return fmt.Errorf("读取 SOCKS5 绑定地址失败: %w", err)
	}
	return nil
}

func writeAll(writer io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := writer.Write(data)
		if err != nil {
			return err
		}
		if written == 0 {
			return fmt.Errorf("写入返回零字节")
		}
		data = data[written:]
	}
	return nil
}

func validateProxyURL(proxyURL *url.URL) error {
	if proxyURL == nil || proxyURL.Host == "" || proxyURL.Path != "" || proxyURL.RawQuery != "" || proxyURL.Fragment != "" {
		return fmt.Errorf("代理地址必须包含主机且不能包含路径、查询参数或片段")
	}
	switch strings.ToLower(proxyURL.Scheme) {
	case "http", "https", "socks5", "socks5h":
		return nil
	default:
		return fmt.Errorf("代理协议必须是 HTTP、HTTPS、SOCKS5 或 SOCKS5h")
	}
}
