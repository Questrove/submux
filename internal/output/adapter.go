package output

// Adapter 把 clash 主模型渲染成某种客户端格式的订阅内容。
type Adapter interface {
	Render(cfg map[string]any) (body []byte, contentType string, err error)
	Format() string
}

// ContentType 返回已缓存输出格式对应的媒体类型。
func ContentType(format string) string {
	if format == "base64" {
		return "text/plain; charset=utf-8"
	}
	return "text/yaml; charset=utf-8"
}
