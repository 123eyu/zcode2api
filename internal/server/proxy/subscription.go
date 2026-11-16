package proxy

import (
	"encoding/base64"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"zcode2api/internal/server/shared"
)

type SubscriptionNode struct {
	Name   string   `json:"-"`
	Config shared.M `json:"-"`
}

func (node SubscriptionNode) Identity() string {
	config := maps.Clone(shared.Obj(node.Config))
	delete(config, "name")
	return shared.Hash(shared.JSON(config))
}

func DecodeSubscription(body []byte, filter string) ([]SubscriptionNode, error) {
	text := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
	if !strings.Contains(text, "://") {
		compact := strings.Join(strings.Fields(text), "")
		var decoded []byte
		var err error
		for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
			decoded, err = encoding.DecodeString(compact)
			if err == nil {
				break
			}
		}
		if err != nil {
			return nil, ProxyFailure("proxy_subscription_invalid", "订阅不是有效的 URI 或 Base64 URI 列表")
		}
		text = strings.TrimSpace(strings.TrimPrefix(string(decoded), "\ufeff"))
	}
	pattern, err := regexp.Compile(filter)
	if err != nil {
		return nil, ProxyFailure("proxy_subscription_invalid", "节点筛选表达式无效")
	}
	seen := map[string]bool{}
	nodes := []SubscriptionNode{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		u, err := url.Parse(line)
		if err != nil || u.Hostname() == "" {
			continue
		}
		name := u.Fragment
		if name == "" {
			name = fmt.Sprintf("节点 %d", len(nodes)+1)
		}
		if !pattern.MatchString(name) || strings.ContainsAny(name, "\r\n\x00") {
			continue
		}
		port := 443
		if u.Port() != "" {
			port, err = strconv.Atoi(u.Port())
			if err != nil || port < 1 || port > 65535 {
				continue
			}
		} else if u.Scheme == "http" {
			port = 80
		}
		query := u.Query()
		config := shared.M{"server": u.Hostname(), "port": port, "udp": true}
		flag := func(key string) bool { return query.Get(key) == "1" || strings.EqualFold(query.Get(key), "true") }
		switch u.Scheme {
		case "vless":
			if u.User == nil || u.User.Username() == "" {
				continue
			}
			config["type"], config["uuid"] = "vless", u.User.Username()
			network := query.Get("type")
			if network == "" || network == "raw" {
				network = "tcp"
			}
			if network != "tcp" && network != "ws" {
				continue
			}
			config["network"] = network
			security := query.Get("security")
			config["tls"] = security == "tls" || security == "reality"
			config["skip-cert-verify"] = flag("insecure") || flag("allowInsecure")
			for from, to := range map[string]string{"flow": "flow", "sni": "servername", "fp": "client-fingerprint"} {
				if value := query.Get(from); value != "" {
					config[to] = value
				}
			}
			if security == "reality" {
				if query.Get("pbk") == "" {
					continue
				}
				config["reality-opts"] = shared.M{"public-key": query.Get("pbk"), "short-id": query.Get("sid")}
			}
			if network == "ws" {
				options := shared.M{"path": shared.Fallback(query.Get("path"), "/")}
				if host := query.Get("host"); host != "" {
					options["headers"] = shared.M{"Host": host}
				}
				config["ws-opts"] = options
			}
			if value := query.Get("alpn"); value != "" {
				config["alpn"] = strings.Split(value, ",")
			}
		case "hysteria2", "hy2":
			if u.User == nil || u.User.Username() == "" {
				continue
			}
			password := u.User.Username()
			if value, present := u.User.Password(); present {
				password += ":" + value
			}
			config["type"], config["password"] = "hysteria2", password
			config["sni"] = shared.Fallback(query.Get("sni"), shared.Fallback(query.Get("peer"), u.Hostname()))
			config["skip-cert-verify"] = flag("insecure")
			if pin := query.Get("pinSHA256"); pin != "" {
				config["fingerprint"] = strings.ReplaceAll(pin, ":", "")
			}
			if obfs := query.Get("obfs"); obfs != "" {
				config["obfs"], config["obfs-password"] = obfs, query.Get("obfs-password")
			}
		case "http", "https":
			config["type"], config["tls"] = "http", u.Scheme == "https"
			if u.User != nil {
				config["username"] = u.User.Username()
				config["password"], _ = u.User.Password()
			}
		default:
			continue
		}
		identity := shared.Hash(shared.JSON(config))
		if !seen[identity] {
			config["name"] = fmt.Sprintf("node-%d", len(nodes)+1)
			nodes = append(nodes, SubscriptionNode{name, config})
			seen[identity] = true
		}
		if len(nodes) > 64 {
			return nil, ProxyFailure("proxy_subscription_too_large", "匹配节点超过 64 个，请收紧节点筛选条件")
		}
	}
	if len(nodes) == 0 {
		return nil, ProxyFailure("proxy_subscription_empty", "没有匹配的受支持节点；支持 VLESS TCP/WS、Hysteria2 和 HTTP/HTTPS URI 订阅")
	}
	return nodes, nil
}
