package output

import "gopkg.in/yaml.v3"

// ClashAdapter 直接把主模型序列化为 clash yaml。
type ClashAdapter struct{}

func (ClashAdapter) Format() string { return "clash" }

func (ClashAdapter) Render(cfg map[string]any) ([]byte, string, error) {
	b, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, "", err
	}
	return b, "text/yaml; charset=utf-8", nil
}
