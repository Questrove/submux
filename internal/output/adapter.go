package output

// Adapter 把 clash 主模型渲染成某种客户端格式的订阅内容。
type Adapter interface {
	Render(cfg map[string]any) (body []byte, contentType string, err error)
	Format() string
}
