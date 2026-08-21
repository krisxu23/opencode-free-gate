package main

import (
	"bufio"
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

// listProxyURLs 读取 PROXY_LIST_URLS 环境变量：
// 逗号/换行/空格分隔的免费代理列表文本地址（raw 文件 URL）。
func listProxyURLs() []string {
	raw := strings.TrimSpace(os.Getenv("PROXY_LIST_URLS"))
	if raw == "" {
		return nil
	}
	fields := strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == '\r' || r == ' ' || r == '\t'
	})
	seen := make(map[string]struct{}, len(fields))
	urls := make([]string, 0, len(fields))
	for _, field := range fields {
		field = strings.TrimSpace(field)
		if field == "" {
			continue
		}
		if !strings.HasPrefix(field, "http://") && !strings.HasPrefix(field, "https://") {
			log.Printf("[列表] 忽略非 http(s) 地址 %q", field)
			continue
		}
		if _, dup := seen[field]; dup {
			continue
		}
		seen[field] = struct{}{}
		urls = append(urls, field)
	}
	return urls
}

// fetchListProxyItems 拉取全部自定义列表并解析为候选代理，与公共池合并。
// 支持每行一条：host:port 或 scheme://host:port；忽略空行、# 注释及行内备注。
// 未写协议前缀的行按 socks5 处理（常见免费 socks5 列表格式）。
func fetchListProxyItems(parent context.Context) []proxyItem {
	urls := listProxyURLs()
	if len(urls) == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(parent, 30*time.Second)
	defer cancel()

	client := &http.Client{Transport: controlTransport(10 * time.Second)}
	defer client.CloseIdleConnections()

	var items []proxyItem
	seen := make(map[string]struct{})
	for _, listURL := range urls {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, listURL, nil)
		if err != nil {
			log.Printf("[列表] 跳过 %q: %v", redactProxy(listURL), err)
			continue
		}
		res, err := client.Do(req)
		if err != nil {
			log.Printf("[列表] 拉取失败 %q: %v", redactProxy(listURL), err)
			continue
		}
		count := 0
		scanner := bufio.NewScanner(io.LimitReader(res.Body, 16<<20))
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") {
				continue
			}
			if idx := strings.IndexAny(line, "#| \t"); idx >= 0 {
				line = strings.TrimSpace(line[:idx])
			}
			if line == "" {
				continue
			}
			protocol := ""
			lower := strings.ToLower(line)
			for _, prefix := range []string{"socks5h://", "socks5://", "socks4a://", "socks4://", "https://", "http://"} {
				if strings.HasPrefix(lower, prefix) {
					protocol = strings.TrimSuffix(prefix, "://")
					line = line[len(prefix):]
					break
				}
			}
			if protocol == "socks4" || protocol == "socks4a" {
				continue // 网关出口仅支持 http / socks5
			}
			if !strings.Contains(line, ":") {
				continue
			}
			key := protocol + line
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			if protocol == "" {
				protocol = "socks5"
			}
			items = append(items, proxyItem{
				Address:      line,
				Protocol:     protocol,
				QualityGrade: "S",
				Status:       "active",
			})
			count++
		}
		res.Body.Close()
		log.Printf("[列表] %s -> %d 条候选", listURL, count)
	}
	log.Printf("[列表] 自定义列表共 %d 条候选", len(items))
	return items
}
