package source

import (
	"fmt"
	"strconv"
	"strings"
)

type Userinfo struct {
	Upload   int64
	Download int64
	Total    int64
	Expire   int64
}

// ParseUserinfo 解析形如 "upload=1; download=2; total=3; expire=4" 的头。
// 至少解析出一个字段才返回 ok=true。
func ParseUserinfo(header string) (Userinfo, bool) {
	header = strings.TrimSpace(header)
	if header == "" {
		return Userinfo{}, false
	}
	var u Userinfo
	found := false
	for _, part := range strings.Split(header, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) != 2 {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimSpace(kv[1]), 10, 64)
		if err != nil {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(kv[0])) {
		case "upload":
			u.Upload, found = n, true
		case "download":
			u.Download, found = n, true
		case "total":
			u.Total, found = n, true
		case "expire":
			u.Expire, found = n, true
		}
	}
	return u, found
}

// AggregateUserinfo 聚合多源流量:up/down/total 求和,expire 取最早非零。
func AggregateUserinfo(infos []Userinfo) Userinfo {
	var a Userinfo
	for _, u := range infos {
		a.Upload += u.Upload
		a.Download += u.Download
		a.Total += u.Total
		if u.Expire > 0 && (a.Expire == 0 || u.Expire < a.Expire) {
			a.Expire = u.Expire
		}
	}
	return a
}

// Header 渲染为 Subscription-Userinfo 头值。
func (u Userinfo) Header() string {
	return fmt.Sprintf("upload=%d; download=%d; total=%d; expire=%d", u.Upload, u.Download, u.Total, u.Expire)
}
